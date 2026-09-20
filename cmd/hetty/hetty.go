package main

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/gorilla/mux"
	"github.com/mitchellh/go-homedir"
	"github.com/peterbourgon/ff/v3/ffcli"
	"go.etcd.io/bbolt"
	"go.uber.org/zap"

	"github.com/dstotijn/hetty/pkg/api"
	"github.com/dstotijn/hetty/pkg/chrome"
	"github.com/dstotijn/hetty/pkg/db/bolt"
	"github.com/dstotijn/hetty/pkg/proj"
	"github.com/dstotijn/hetty/pkg/proxy"
	"github.com/dstotijn/hetty/pkg/proxy/intercept"
	"github.com/dstotijn/hetty/pkg/reqid"
	"github.com/dstotijn/hetty/pkg/reqlog"
	"github.com/dstotijn/hetty/pkg/scope"
	"github.com/dstotijn/hetty/pkg/sender"
)

var version = "0.0.0"

//go:embed admin
//go:embed admin/_next/static
//go:embed admin/_next/static/chunks/pages/*.js
//go:embed admin/_next/static/*/*.js
var adminContent embed.FS

var hettyUsage = `
Usage:
    hetty [flags] [subcommand] [flags]

Runs an HTTP server with (MITM) proxy, GraphQL service, and a web based admin interface.

Options:
    --cert         Path to root CA certificate. Creates file if it doesn't exist. (Default: "~/.hetty/hetty_cert.pem")
    --key          Path to root CA private key. Creates file if it doesn't exist. (Default: "~/.hetty/hetty_key.pem")
    --db           Database file path. Creates file if it doesn't exist. (Default: "~/.hetty/hetty.db")
    --addr         TCP address for HTTP server to listen on, in the form \"host:port\". (Default: ":8080")
    --chrome       Launch Chrome with proxy settings applied and certificate errors ignored. (Default: false)
    --verbose      Enable verbose logging.
    --json         Encode logs as JSON, instead of pretty/human readable output.
    --version, -v  Output version.
    --help, -h     Output this usage text.

Subcommands:
    - cert  Certificate management

Run ` + "`hetty <subcommand> --help`" + ` for subcommand specific usage instructions.

Visit https://hetty.xyz to learn more about Hetty.
`

type HettyCommand struct {
	config *Config

	cert    string
	key     string
	db      string
	addr    string
	chrome  bool
	version bool
}

func NewHettyCommand() (*ffcli.Command, *Config) {
	cmd := HettyCommand{
		config: &Config{},
	}

	fs := flag.NewFlagSet("hetty", flag.ExitOnError)

	fs.StringVar(&cmd.cert, "cert", "~/.hetty/hetty_cert.pem",
		"Path to root CA certificate. Creates a new certificate if file doesn't exist.")
	fs.StringVar(&cmd.key, "key", "~/.hetty/hetty_key.pem",
		"Path to root CA private key. Creates a new private key if file doesn't exist.")
	fs.StringVar(&cmd.db, "db", "~/.hetty/hetty.db", "Database file path. Creates file if it doesn't exist.")
	fs.StringVar(&cmd.addr, "addr", ":8080", "TCP address to listen on, in the form \"host:port\".")
	fs.BoolVar(&cmd.chrome, "chrome", false, "Launch Chrome with proxy settings applied and certificate errors ignored.")
	fs.BoolVar(&cmd.version, "version", false, "Output version.")
	fs.BoolVar(&cmd.version, "v", false, "Output version.")

	cmd.config.RegisterFlags(fs)

	return &ffcli.Command{
		Name:    "hetty",
		FlagSet: fs,
		Subcommands: []*ffcli.Command{
			NewCertCommand(cmd.config),
		},
		Exec: cmd.Exec,
		UsageFunc: func(*ffcli.Command) string {
			return hettyUsage
		},
	}, cmd.config
}

func (cmd *HettyCommand) Exec(ctx context.Context, _ []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	if cmd.version {
		fmt.Fprint(os.Stdout, version+"\n")
		return nil
	}

	mainLogger := cmd.config.logger.Named("main")

	listenHost, listenPort, err := net.SplitHostPort(cmd.addr)
	if err != nil {
		mainLogger.Fatal("Failed to parse listening address.", zap.Error(err))
	}

	url := fmt.Sprintf("http://%v:%v", listenHost, listenPort)
	if listenHost == "" || listenHost == "0.0.0.0" || listenHost == "127.0.0.1" || listenHost == "::1" {
		url = fmt.Sprintf("http://localhost:%v", listenPort)
	}

	// Expand `~` in filepaths.
	caCertFile, err := homedir.Expand(cmd.cert)
	if err != nil {
		cmd.config.logger.Fatal("Failed to parse CA certificate filepath.", zap.Error(err))
	}

	caKeyFile, err := homedir.Expand(cmd.key)
	if err != nil {
		cmd.config.logger.Fatal("Failed to parse CA private key filepath.", zap.Error(err))
	}

	dbPath, err := homedir.Expand(cmd.db)
	if err != nil {
		cmd.config.logger.Fatal("Failed to parse database path.", zap.Error(err))
	}

	// Load existing CA certificate and key from disk, or generate and write
	// to disk if no files exist yet.
	caCert, caKey, err := proxy.LoadOrCreateCA(caKeyFile, caCertFile)
	if err != nil {
		cmd.config.logger.Fatal("Failed to load or create CA key pair.", zap.Error(err))
	}

	dbLogger := cmd.config.logger.Named("boltdb").Sugar()
	boltOpts := *bbolt.DefaultOptions
	boltOpts.Logger = &bolt.Logger{SugaredLogger: dbLogger}

	boltDB, err := bolt.OpenDatabase(dbPath, &boltOpts)
	if err != nil {
		cmd.config.logger.Fatal("Failed to open database.", zap.Error(err))
	}
	defer boltDB.Close()

	scope := &scope.Scope{}

	reqLogService := reqlog.NewService(reqlog.Config{
		Scope:      scope,
		Repository: boltDB,
		Logger:     cmd.config.logger.Named("reqlog").Sugar(),
	})

	interceptService := intercept.NewService(intercept.Config{
		Logger: cmd.config.logger.Named("intercept").Sugar(),
	})

	senderService := sender.NewService(sender.Config{
		Repository:    boltDB,
		ReqLogService: reqLogService,
	})

	projService, err := proj.NewService(proj.Config{
		Repository:       boltDB,
		InterceptService: interceptService,
		ReqLogService:    reqLogService,
		SenderService:    senderService,
		Scope:            scope,
	})
	if err != nil {
		cmd.config.logger.Fatal("Failed to create new projects service.", zap.Error(err))
	}

	proxy, err := proxy.NewProxy(proxy.Config{
		CACert: caCert,
		CAKey:  caKey,
		Logger: cmd.config.logger.Named("proxy").Sugar(),
	})
	if err != nil {
		cmd.config.logger.Fatal("Failed to create new proxy.", zap.Error(err))
	}

	proxy.UseRequestModifier(reqLogService.RequestModifier)
	proxy.UseResponseModifier(reqLogService.ResponseModifier)
	proxy.UseRequestModifier(interceptService.RequestModifier)
	proxy.UseResponseModifier(interceptService.ResponseModifier)

	fsSub, err := fs.Sub(adminContent, "admin")
	if err != nil {
		cmd.config.logger.Fatal("Failed to construct file system subtree from admin dir.", zap.Error(err))
	}

	adminHandler := http.FileServer(http.FS(fsSub))
	// SkipClean(true) disables gorilla/mux's default request path cleaning
	// (e.g. /foo/../bar -> /bar). It MUST stay enabled: the catch-all route
	// is the MITM proxy, which must proxy paths exactly as the client sent
	// them. Cleaning them here would corrupt upstream URLs. Do not remove
	// without auditing both proxying and admin route matching.
	router := mux.NewRouter().SkipClean(true)
	// The request ID middleware runs on the outermost router so every
	// request — admin API, static assets and proxied traffic handled below
	// — carries a correlation ID on its context and response header.
	router.Use(reqid.Middleware)
	adminRouter := router.MatcherFunc(func(req *http.Request, match *mux.RouteMatch) bool {
		hostname, _ := os.Hostname()
		host, _, _ := net.SplitHostPort(req.Host)

		// Serve local admin routes when either:
		// - The `Host` is well-known, e.g. `hetty.proxy`, `localhost:[port]`
		//   or the listen addr `[host]:[port]`.
		// - The request is not for TLS proxying (e.g. no `CONNECT`) and not
		//   for proxying an external URL. E.g. Request-Line (RFC 7230, Section 3.1.1)
		//   has no scheme.
		return strings.EqualFold(host, hostname) ||
			req.Host == "hetty.proxy" ||
			req.Host == fmt.Sprintf("%v:%v", "localhost", listenPort) ||
			req.Host == fmt.Sprintf("%v:%v", listenHost, listenPort) ||
			req.Method != http.MethodConnect && !strings.HasPrefix(req.RequestURI, "http://")
	}).Subrouter().StrictSlash(true)
	// StrictSlash(true) applies ONLY to the admin subrouter: it redirects
	// "/api/graphql" -> "/api/graphql/" so the documented trailing-slash
	// endpoint is reached regardless of how the client types it, while the
	// underlying proxy routes remain untouched. Keep it scoped to this
	// subrouter for that reason.

	// GraphQL server. The handler itself applies request size limiting,
	// per-client rate limiting, query complexity limiting and per-operation
	// audit logging on top of the router-wide request ID middleware.
	gqlEndpoint := "/api/graphql/"
	// One audit logger is shared by the gqlgen operation audit extension
	// (every operation) and the resolver-level mutation audit entries
	// (ModifyRequest, SendRequest, ...), so all entries are correlated by
	// request_id under the same logger name.
	auditLogger := cmd.config.logger.Named("audit").Sugar()
	adminRouter.Path(gqlEndpoint).Handler(api.HTTPHandler(&api.Resolver{
		ProjectService:    projService,
		RequestLogService: reqLogService,
		InterceptService:  interceptService,
		SenderService:     senderService,
		Logger:            auditLogger,
	}, gqlEndpoint, api.WithLogger(auditLogger)))

	// Admin interface.
	adminRouter.PathPrefix("").Handler(adminHandler)

	// Fallback (default) is the Proxy handler.
	router.PathPrefix("").Handler(proxy)

	httpServer := &http.Server{
		Addr: cmd.addr,
		// ReadHeaderTimeout bounds slowloris-style clients that hold a
		// connection open while trickling headers. ReadTimeout/WriteTimeout
		// are deliberately NOT set: long lived CONNECT tunnels, intercepted
		// requests and long running sender responses must not be cut off.
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout caps keep-alive idle time (the zero value would inherit
		// ReadHeaderTimeout, but we set it explicitly for clarity).
		IdleTimeout:  90 * time.Second,
		Handler:      router,
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}, // Disable HTTP/2
		ErrorLog:     zap.NewStdLog(cmd.config.logger.Named("http")),
	}

	go func() {
		mainLogger.Info(fmt.Sprintf("Hetty (v%v) is running on %v ...", version, cmd.addr))
		mainLogger.Info(fmt.Sprintf("\x1b[%dm%s\x1b[0m", uint8(32), "Get started at "+url))

		err := httpServer.ListenAndServe()
		if err != http.ErrServerClosed {
			mainLogger.Fatal("HTTP server closed unexpected.", zap.Error(err))
		}
	}()

	if cmd.chrome {
		ctx, cancel := chrome.NewExecAllocator(ctx, chrome.Config{
			ProxyServer:      url,
			ProxyBypassHosts: []string{url},
		})
		defer cancel()

		taskCtx, cancel := chromedp.NewContext(ctx)
		defer cancel()

		err = chromedp.Run(taskCtx, chromedp.Navigate(url))

		switch {
		case errors.Is(err, exec.ErrNotFound):
			mainLogger.Info("Chrome executable not found.")
		case err != nil:
			mainLogger.Error(fmt.Sprintf("Failed to navigate to %v.", url), zap.Error(err))
		default:
			mainLogger.Info("Launched Chrome.")
		}
	}

	// Wait for interrupt signal.
	<-ctx.Done()
	// Restore signal, allowing "force quit".
	stop()

	mainLogger.Info("Shutting down HTTP server. Press Ctrl+C to force quit.")

	// Note: We expect httpServer.Handler to handle timeouts, thus, we don't
	// need a context value with deadline here.
	//nolint:contextcheck
	err = httpServer.Shutdown(context.Background())
	if err != nil {
		return fmt.Errorf("failed to shutdown HTTP server: %w", err)
	}

	return nil
}

package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/gorilla/mux"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/dstotijn/hetty/pkg/log"
	"github.com/dstotijn/hetty/pkg/ratelimit"
	"github.com/dstotijn/hetty/pkg/reqid"
)

const (
	// defaultComplexityLimit caps how expensive a single GraphQL query may
	// be, preventing deeply nested or heavily batched queries from exhausting
	// CPU. Introspection queries from the playground stay well under it.
	defaultComplexityLimit = 2000
	// defaultMaxQueryBytes caps the size of an incoming GraphQL request body.
	// GraphQL payloads are small JSON documents; anything larger is abuse.
	defaultMaxQueryBytes int64 = 1 << 20
	// defaultRateLimitRPS is the sustained per-client request budget.
	defaultRateLimitRPS = 10
	// defaultRateLimitBurst is the short burst allowance per client.
	defaultRateLimitBurst = 20
)

// HTTPHandlerConfig configures the protected GraphQL HTTP handler.
type HTTPHandlerConfig struct {
	// GQLEndpoint is the path the playground sends queries to.
	GQLEndpoint string
	// Logger receives per-operation audit entries. When nil, audit logging
	// is disabled.
	Logger log.Logger
	// ComplexityLimit overrides defaultComplexityLimit when > 0.
	ComplexityLimit int
	// MaxQueryBytes overrides defaultMaxQueryBytes when > 0.
	MaxQueryBytes int64
	// Limiter, when set, throttles requests per client IP. When nil, a limiter
	// with the default budget is used.
	Limiter *ratelimit.Limiter
	// TrustProxyHeader makes the X-Forwarded-For header authoritative for the
	// rate limit key. Only enable behind a sanitizing reverse proxy.
	TrustProxyHeader bool
}

// Option customizes HTTPHandler.
type Option func(*HTTPHandlerConfig)

// WithLogger enables operation audit logging.
func WithLogger(logger log.Logger) Option {
	return func(cfg *HTTPHandlerConfig) { cfg.Logger = logger }
}

// WithComplexityLimit overrides the default GraphQL complexity limit.
func WithComplexityLimit(limit int) Option {
	return func(cfg *HTTPHandlerConfig) { cfg.ComplexityLimit = limit }
}

// WithMaxQueryBytes overrides the default GraphQL request body size cap.
func WithMaxQueryBytes(n int64) Option {
	return func(cfg *HTTPHandlerConfig) { cfg.MaxQueryBytes = n }
}

// WithRateLimiter supplies a custom per-client limiter.
func WithRateLimiter(l *ratelimit.Limiter) Option {
	return func(cfg *HTTPHandlerConfig) { cfg.Limiter = l }
}

// WithTrustProxyHeader trusts X-Forwarded-For for rate limit keying.
func WithTrustProxyHeader() Option {
	return func(cfg *HTTPHandlerConfig) { cfg.TrustProxyHeader = true }
}

// HTTPHandler builds the GraphQL HTTP handler with the full protection stack:
// request ID propagation, request size limiting, per-client rate limiting,
// query complexity limiting, and operation audit logging.
//
// The returned handler is mounted on a gorilla/mux subrouter matching the
// upstream setup. mux's default path cleaning is intentionally left enabled
// (SkipClean is NOT set here): the GraphQL endpoint is a single canonical
// path and cleaning guarantees that variants such as `/../api/graphql/` map
// to it instead of reaching the catch-all admin file server.
func HTTPHandler(resolver *Resolver, gqlEndpoint string, opts ...Option) http.Handler {
	cfg := HTTPHandlerConfig{
		GQLEndpoint:     gqlEndpoint,
		ComplexityLimit: defaultComplexityLimit,
		MaxQueryBytes:   defaultMaxQueryBytes,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.Limiter == nil {
		cfg.Limiter = ratelimit.New(defaultRateLimitRPS, defaultRateLimitBurst)
	}

	gqlServer := handler.NewDefaultServer(NewExecutableSchema(Config{
		Resolvers: resolver,
	}))

	// Reject queries whose computed cost exceeds the complexity budget.
	gqlServer.Use(extension.FixedComplexityLimit(cfg.ComplexityLimit))
	// Structured audit log entry per operation (request ID, fields, latency).
	gqlServer.Use(newAuditExtension(cfg.Logger))

	// Attach the correlation ID to every returned GraphQL error so client
	// errors can be matched against server logs.
	gqlServer.SetErrorPresenter(func(ctx context.Context, err error) *gqlerror.Error {
		presented := graphql.DefaultErrorPresenter(ctx, err)
		if presented.Extensions == nil {
			presented.Extensions = map[string]interface{}{}
		}

		if id, ok := reqid.FromContext(ctx); ok {
			presented.Extensions["request_id"] = id.String()
		}

		return presented
	})

	gqlHandler := http.Handler(gqlServer)
	gqlHandler = maxBodyHandler(cfg.MaxQueryBytes, gqlHandler)
	gqlHandler = rateLimitHandler(cfg.Limiter, cfg.TrustProxyHeader, gqlHandler)
	// Outermost: assign/extract the request ID used by all inner layers.
	gqlHandler = reqid.Middleware(gqlHandler)

	router := mux.NewRouter()
	router.Methods(http.MethodPost).Handler(gqlHandler)
	router.Methods(http.MethodGet).Handler(
		playground.Handler("GraphQL Playground", cfg.GQLEndpoint))

	return router
}

// maxBodyHandler caps the number of bytes read from an incoming request body,
// turning an oversized payload into an HTTP 413 before the GraphQL parser
// touches it.
func maxBodyHandler(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// rateLimitHandler enforces the per-client request budget and emits a
// GraphQL-shaped 429 response so existing clients can parse the error.
func rateLimitHandler(l *ratelimit.Limiter, trustProxy bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client := ratelimit.ClientIP(r, trustProxy)
		if !l.Allow(client) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)

			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"errors": []map[string]interface{}{
					{
						"message": "Rate limit exceeded, slow down.",
						"extensions": map[string]interface{}{
							"code": "rate_limited",
						},
					},
				},
			})

			return
		}

		next.ServeHTTP(w, r)
	})
}

package api

import (
	"net/http"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/gorilla/mux"

	"github.com/dstotijn/hetty/pkg/log"
	"github.com/dstotijn/hetty/pkg/proxy"
)

const (
	// maxGraphQLComplexity caps the total complexity of a single GraphQL
	// operation. Without a cap, a malicious client could send deeply nested
	// or excessively broad queries and exhaust server CPU/memory.
	maxGraphQLComplexity = 1000

	// maxConcurrentRequests caps the number of GraphQL requests being
	// processed at the same time. Excess requests are rejected with
	// 429 Too Many Requests instead of piling up goroutines.
	maxConcurrentRequests = 64
)

func HTTPHandler(resolver *Resolver, gqlEndpoint string) http.Handler {
	logger := resolver.Logger
	if logger == nil {
		logger = log.NewNopLogger()
	}

	// SkipClean(true) disables mux's default path cleaning (which redirects
	// "unclean" paths). The GraphQL endpoint is registered with a trailing
	// slash and must also tolerate encoded characters in the path; cleaning
	// would issue unwanted redirects or alter the request path.
	router := mux.NewRouter().SkipClean(true)

	srv := handler.NewDefaultServer(NewExecutableSchema(Config{
		Resolvers: resolver,
	}))
	// Reject queries whose static complexity exceeds the limit, to protect
	// the server from resource-exhaustion attacks via expensive queries.
	srv.Use(extension.FixedComplexityLimit(maxGraphQLComplexity))

	gqlHandler := proxy.RequestIDMiddleware(
		limitConcurrencyMiddleware(
			requestLoggerMiddleware(logger, srv),
			maxConcurrentRequests,
		),
	)

	router.Methods("POST").Handler(gqlHandler)
	router.Methods("GET").Handler(playground.Handler("GraphQL Playground", gqlEndpoint))

	return router
}

// limitConcurrencyMiddleware allows at most max requests to be handled
// concurrently; additional requests are rejected immediately.
func limitConcurrencyMiddleware(next http.Handler, max int) http.Handler {
	sem := make(chan struct{}, max)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "Too many concurrent requests.", http.StatusTooManyRequests)
		}
	})
}

// statusRecorder wraps http.ResponseWriter to capture the response status
// code for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// requestLoggerMiddleware logs every handled request, including the request
// ID (when present) so log lines can be correlated with audit log entries
// and proxy logs.
func requestLoggerMiddleware(logger log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		fields := []interface{}{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).String(),
		}
		if reqID, ok := proxy.RequestIDFromContext(r.Context()); ok {
			fields = append(fields, "request_id", reqID.String())
		}

		logger.Infow("Handled GraphQL request.", fields...)
	})
}

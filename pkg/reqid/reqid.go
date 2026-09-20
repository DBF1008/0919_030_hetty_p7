// Package reqid carries a per-request ULID through the whole request
// lifecycle: HTTP transport -> proxy -> GraphQL resolvers -> downstream
// services and audit logs.
package reqid

import (
	"context"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/oklog/ulid"
)

// HeaderName is the HTTP header used to propagate the request ID to clients
// (and, when trusted, to accept an inbound correlation ID).
const HeaderName = "X-Request-ID"

type contextKey struct{}

// entropy is guarded by a mutex because math/rand sources are not safe for
// concurrent use (every proxied/GraphQL request generates a ULID).
var (
	entropyMu sync.Mutex
	entropy   = rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
)

// New returns a monotonic-ish, time sortable unique identifier.
func New() ulid.ULID {
	entropyMu.Lock()
	defer entropyMu.Unlock()

	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy)
}

// ContextWithID returns a copy of ctx carrying the given request ID.
func ContextWithID(ctx context.Context, id ulid.ULID) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext returns the request ID stored in ctx, if any.
func FromContext(ctx context.Context) (ulid.ULID, bool) {
	id, ok := ctx.Value(contextKey{}).(ulid.ULID)

	return id, ok
}

// Middleware assigns a request ID to every handled request, stores it on the
// request context, sets it on the response header and invokes next.
//
// An inbound X-Request-ID header is honored when it parses as a ULID. This
// makes it possible to correlate Hetty's logs with logs of systems that sit
// in front of it, while malformed values can never poison the context.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := New()

		if raw := r.Header.Get(HeaderName); raw != "" {
			if parsed, err := ulid.ParseStrict(raw); err == nil {
				id = parsed
			}
		}

		w.Header().Set(HeaderName, id.String())
		*r = *r.WithContext(ContextWithID(r.Context(), id))

		next.ServeHTTP(w, r)
	})
}

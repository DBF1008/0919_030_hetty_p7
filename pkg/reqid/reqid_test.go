package reqid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oklog/ulid"
)

func TestContextRoundTrip(t *testing.T) {
	id := New()
	ctx := ContextWithID(context.Background(), id)

	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("expected request ID in context")
	}

	if got.Compare(id) != 0 {
		t.Fatalf("got %v, want %v", got, id)
	}

	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("expected no ID in empty context")
	}
}

func TestNewIsUnique(t *testing.T) {
	ids := make(map[ulid.ULID]struct{}, 100)

	for i := 0; i < 100; i++ {
		id := New()
		if _, exists := ids[id]; exists {
			t.Fatalf("duplicate ULID generated: %v", id)
		}

		ids[id] = struct{}{}
	}
}

func TestNewConcurrentSafe(t *testing.T) {
	done := make(chan struct{})

	for i := 0; i < 16; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = New()
			}
			done <- struct{}{}
		}()
	}

	for i := 0; i < 16; i++ {
		<-done
	}
}

func TestMiddlewareGeneratesAndEchoesID(t *testing.T) {
	var seenID ulid.ULID

	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromContext(r.Context())
		if !ok {
			t.Error("expected request ID on context")
		}

		seenID = id
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/graphql/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %v", rec.Code)
	}

	if got := rec.Header().Get(HeaderName); got != seenID.String() {
		t.Fatalf("response header %q = %q, want %q", HeaderName, got, seenID.String())
	}
}

func TestMiddlewareHonorsValidInboundID(t *testing.T) {
	inbound := New().String()
	var ctxID ulid.ULID

	h := Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctxID, _ = FromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, inbound)
	h.ServeHTTP(rec, req)

	if ctxID.String() != inbound {
		t.Fatalf("expected inbound ID %q to be honored, got %q", inbound, ctxID.String())
	}

	if got := rec.Header().Get(HeaderName); got != inbound {
		t.Fatalf("expected echoed inbound ID, got %q", got)
	}
}

func TestMiddlewareRejectsMalformedInboundID(t *testing.T) {
	var ctxID ulid.ULID

	h := Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctxID, _ = FromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, "not-a-ulid")
	h.ServeHTTP(rec, req)

	if ctxID.Compare(ulid.ULID{}) == 0 {
		t.Fatal("expected a freshly generated ID despite malformed inbound header")
	}

	if got := rec.Header().Get(HeaderName); got != ctxID.String() {
		t.Fatalf("expected fresh ID echoed, got %q", got)
	}
}

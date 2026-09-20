package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oklog/ulid"
)

func TestRequestIDMiddlewareGeneratesID(t *testing.T) {
	var gotID ulid.ULID
	var gotOK bool

	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotOK = RequestIDFromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/graphql/", nil))

	if !gotOK {
		t.Fatal("expected request ID in context")
	}
	if gotID == (ulid.ULID{}) {
		t.Fatal("expected non-zero request ID")
	}
	if got := rec.Header().Get(RequestIDHeader); got != gotID.String() {
		t.Fatalf("expected %v header %q, got %q", RequestIDHeader, gotID.String(), got)
	}
	if ts := ulid.Time(gotID.Time()); time.Since(ts) > time.Minute {
		t.Fatalf("request ID timestamp %v is not recent", ts)
	}
}

func TestRequestIDMiddlewarePreservesExistingID(t *testing.T) {
	existing := ulid.MustNew(ulid.Timestamp(time.Now()), ulidEntropy)

	var gotID ulid.ULID
	var gotOK bool

	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotOK = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/graphql/", nil)
	req = req.WithContext(WithRequestID(req.Context(), existing))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !gotOK {
		t.Fatal("expected request ID in context")
	}
	if gotID != existing {
		t.Fatalf("expected existing request ID %v to be preserved, got %v", existing, gotID)
	}
	if got := rec.Header().Get(RequestIDHeader); got != existing.String() {
		t.Fatalf("expected %v header %q, got %q", RequestIDHeader, existing.String(), got)
	}
}

func TestNewRequestIDUnique(t *testing.T) {
	seen := make(map[ulid.ULID]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewRequestID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate request ID generated: %v", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewRequestIDConcurrent(t *testing.T) {
	const goroutines = 32

	ids := make(chan ulid.ULID, goroutines*100)
	for i := 0; i < goroutines; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				ids <- NewRequestID()
			}
		}()
	}

	seen := make(map[ulid.ULID]struct{}, goroutines*100)
	for i := 0; i < goroutines*100; i++ {
		id := <-ids
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate request ID generated concurrently: %v", id)
		}
		seen[id] = struct{}{}
	}
}

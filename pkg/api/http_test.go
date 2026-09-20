package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dstotijn/hetty/pkg/ratelimit"
)

// captureLogger is a log.Logger that records structured calls.
type captureLogger struct {
	mu     sync.Mutex
	infos  [][]interface{}
	errors [][]interface{}
}

func (c *captureLogger) Debugw(_ string, _ ...interface{}) {}

func (c *captureLogger) Infow(_ string, v ...interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.infos = append(c.infos, v)
}

func (c *captureLogger) Errorw(_ string, v ...interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.errors = append(c.errors, v)
}

func fieldsMap(pairs []interface{}) map[string]interface{} {
	m := make(map[string]interface{})

	for i := 0; i+1 < len(pairs); i += 2 {
		if key, ok := pairs[i].(string); ok {
			m[key] = pairs[i+1]
		}
	}

	return m
}

func gqlPost(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		t.Fatalf("failed to marshal query: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/graphql/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func newTestHandler(t *testing.T, opts ...Option) http.Handler {
	t.Helper()
	return HTTPHandler(&Resolver{}, "/api/graphql/", opts...)
}

func TestRequestIDHeaderOnGraphQL(t *testing.T) {
	h := newTestHandler(t)

	rec := gqlPost(t, h, "{__typename}")

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %v (body: %s)", rec.Code, rec.Body.String())
	}

	id := rec.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("expected X-Request-ID response header")
	}
}

func TestGraphQLErrorCarriesRequestID(t *testing.T) {
	h := newTestHandler(t)

	rec := gqlPost(t, h, "{thisFieldDoesNotExist}")

	var resp struct {
		Errors []struct {
			Extensions map[string]interface{} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Errors) == 0 {
		t.Fatal("expected GraphQL errors, got none")
	}

	wantID := rec.Header().Get("X-Request-ID")
	if got := resp.Errors[0].Extensions["request_id"]; got != wantID {
		t.Fatalf("error extension request_id = %v, want %q", got, wantID)
	}
}

func TestComplexityLimitRejectsExpensiveQuery(t *testing.T) {
	// A zero budget rejects any query whose computed cost is >= 1, which
	// covers every real query (even the cheapest field costs 1).
	h := newTestHandler(t, WithComplexityLimit(0))

	rec := gqlPost(t, h, "{__typename}")

	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Errors) == 0 {
		t.Fatalf("expected complexity limit error, body: %s", rec.Body.String())
	}

	if !strings.Contains(strings.ToLower(resp.Errors[0].Message), "complex") {
		t.Fatalf("expected complexity error message, got %q", resp.Errors[0].Message)
	}
}

func TestComplexityLimitAllowsCheapQuery(t *testing.T) {
	h := newTestHandler(t, WithComplexityLimit(2000))

	rec := gqlPost(t, h, "{__typename}")

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %v (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestRateLimitReturns429(t *testing.T) {
	h := newTestHandler(t, WithRateLimiter(ratelimit.New(1, 1)))

	first := gqlPost(t, h, "{__typename}")
	if first.Code != http.StatusOK {
		t.Fatalf("unexpected first status: %v", first.Code)
	}

	second := gqlPost(t, h, "{__typename}")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %v (body: %s)", second.Code, second.Body.String())
	}

	if ct := second.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected JSON content type, got %q", ct)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatalf("rate limit response is not JSON: %v", err)
	}

	if _, ok := resp["errors"]; !ok {
		t.Fatal("expected GraphQL-shaped error payload")
	}
}

func TestMaxQueryBytesRejectsOversizedBody(t *testing.T) {
	h := newTestHandler(t, WithMaxQueryBytes(16))

	big := bytes.Repeat([]byte("a"), 4096)
	req := httptest.NewRequest(http.MethodPost, "/api/graphql/", bytes.NewReader(big))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < http.StatusBadRequest {
		t.Fatalf("expected 4xx for oversized body, got %v", rec.Code)
	}
}

func TestAuditExtensionLogsOperation(t *testing.T) {
	logger := &captureLogger{}
	h := newTestHandler(t, WithLogger(logger))

	rec := gqlPost(t, h, "{__typename}")
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %v", rec.Code)
	}

	if len(logger.infos) != 1 {
		t.Fatalf("expected 1 info audit entry, got %d", len(logger.infos))
	}

	fields := fieldsMap(logger.infos[0])

	if fields["type"] != "query" {
		t.Fatalf("expected type=query, got %v", fields["type"])
	}

	if id, _ := fields["request_id"].(string); id == "" {
		t.Fatal("expected non-empty request_id in audit entry")
	}

	if _, ok := fields["latency_ms"]; !ok {
		t.Fatal("expected latency_ms in audit entry")
	}
}

func TestAuditExtensionLogsFailedOperation(t *testing.T) {
	logger := &captureLogger{}
	h := newTestHandler(t, WithLogger(logger))

	_ = gqlPost(t, h, "{thisFieldDoesNotExist}")

	if len(logger.errors) == 0 {
		t.Fatal("expected an error audit entry for a failed operation")
	}

	fields := fieldsMap(logger.errors[0])
	if id, _ := fields["request_id"].(string); id == "" {
		t.Fatal("expected request_id in error audit entry")
	}

	if _, ok := fields["errors"]; !ok {
		t.Fatal("expected error detail in audit entry")
	}
}

func TestPlaygroundAlsoCarriesRequestID(t *testing.T) {
	h := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/graphql/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected playground status: %v", rec.Code)
	}

	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("expected X-Request-ID header on playground response")
	}
}

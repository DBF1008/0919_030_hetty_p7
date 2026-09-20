package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAllowBurstThenRefill(t *testing.T) {
	l := New(10, 2)

	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("expected first two requests within burst to be allowed")
	}

	if l.Allow("a") {
		t.Fatal("expected third request within burst to be rejected")
	}

	// At 10 tokens/s, 110ms should fully refill the burst of 2.
	time.Sleep(110 * time.Millisecond)

	if !l.Allow("a") {
		t.Fatal("expected request to be allowed after refill")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(1, 1)

	if !l.Allow("a") {
		t.Fatal("expected first request for a to be allowed")
	}

	if l.Allow("a") {
		t.Fatal("expected second request for a to be rejected")
	}

	if !l.Allow("b") {
		t.Fatal("expected independent bucket for b to allow")
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		trust      bool
		want       string
	}{
		{
			name:       "remote addr used by default",
			remoteAddr: "10.0.0.5:54321",
			want:       "10.0.0.5",
		},
		{
			name:       "xff ignored when proxy not trusted",
			remoteAddr: "10.0.0.5:54321",
			xff:        "1.2.3.4",
			trust:      false,
			want:       "10.0.0.5",
		},
		{
			name:       "leftmost xff used when trusted",
			remoteAddr: "10.0.0.5:54321",
			xff:        "203.0.113.7, 10.0.0.1",
			trust:      true,
			want:       "203.0.113.7",
		},
		{
			name:       "bad xff falls back to remote addr",
			remoteAddr: "10.0.0.5:54321",
			xff:        "not-an-ip",
			trust:      true,
			want:       "10.0.0.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}

			if got := ClientIP(req, tt.trust); got != tt.want {
				t.Fatalf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

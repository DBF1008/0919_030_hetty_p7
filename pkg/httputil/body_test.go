package httputil

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// errAfterReader returns err after the remaining bytes have been read,
// simulating a client that disconnects mid-transfer.
type errAfterReader struct {
	remaining int
	err       error
	closed    bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, r.err
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= len(p)
	return len(p), nil
}

func (r *errAfterReader) Close() error {
	r.closed = true
	return nil
}

type trackedBody struct {
	*strings.Reader
	closed *bool
}

func (b *trackedBody) Close() error {
	*b.closed = true
	return nil
}

func newTrackedBody(s string) (*trackedBody, *bool) {
	closed := new(bool)
	return &trackedBody{Reader: strings.NewReader(s), closed: closed}, closed
}

func TestReadAndRestoreBodyNil(t *testing.T) {
	data, restored, err := ReadAndRestoreBody(nil, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if data != nil || restored != nil {
		t.Fatal("expected nil data and restored body for nil input")
	}
}

func TestReadAndRestoreBody(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		limit    int64
		wantData string
		wantErr  error
	}{
		{"empty body", "", 10, "", nil},
		{"body within limit", "hello", 10, "hello", nil},
		{"body exactly at limit", "0123456789", 10, "0123456789", nil},
		{"body exceeds limit", "0123456789abcdef", 10, "", ErrBodyTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, closed := newTrackedBody(tt.input)

			data, restored, err := ReadAndRestoreBody(body, tt.limit)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}
			if string(data) != tt.wantData {
				t.Fatalf("expected data %q, got %q", tt.wantData, string(data))
			}
			if !*closed {
				t.Fatal("expected original body to be closed")
			}
			if restored == nil {
				t.Fatal("expected non-nil restored body")
			}

			restoredData, err := io.ReadAll(restored)
			if err != nil {
				t.Fatalf("failed to read restored body: %v", err)
			}
			if string(restoredData) != tt.wantData {
				t.Fatalf("expected restored body %q, got %q", tt.wantData, string(restoredData))
			}
		})
	}
}

func TestReadAndRestoreBodyClientDisconnect(t *testing.T) {
	readErr := errors.New("connection reset by peer")
	body := &errAfterReader{remaining: 5, err: readErr}

	data, restored, err := ReadAndRestoreBody(body, 100)
	if !errors.Is(err, readErr) {
		t.Fatalf("expected read error %v, got %v", readErr, err)
	}
	if !body.closed {
		t.Fatal("expected body to be closed after read error")
	}
	if string(data) != "xxxxx" {
		t.Fatalf("expected partial data %q, got %q", "xxxxx", string(data))
	}
	if restored == nil {
		t.Fatal("expected restored body even on read error")
	}

	restoredData, err := io.ReadAll(restored)
	if err != nil {
		t.Fatalf("failed to read restored body: %v", err)
	}
	if string(restoredData) != "xxxxx" {
		t.Fatalf("expected restored partial data %q, got %q", "xxxxx", string(restoredData))
	}
}

func TestReadAndRestoreBodyHugeBodyDoesNotExhaustMemory(t *testing.T) {
	// A 1 GiB stream against a 1 MiB limit must terminate promptly instead
	// of buffering the whole stream in memory.
	body := &errAfterReader{remaining: 1 << 30, err: errors.New("should not be reached")}

	_, _, err := ReadAndRestoreBody(body, 1<<20)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("expected ErrBodyTooLarge, got %v", err)
	}
	if !body.closed {
		t.Fatal("expected body to be closed")
	}
}

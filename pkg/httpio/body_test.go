package httpio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadBodySmall(t *testing.T) {
	body := io.NopCloser(strings.NewReader("hello"))

	data, err := ReadBody(context.Background(), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(data) != "hello" {
		t.Fatalf("got %q, want %q", data, "hello")
	}
}

func TestReadBodyTooLarge(t *testing.T) {
	big := bytes.Repeat([]byte("x"), int(MaxBodySize)+5)
	body := io.NopCloser(bytes.NewReader(big))

	_, err := ReadBody(context.Background(), body)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("expected ErrBodyTooLarge, got %v", err)
	}
}

func TestReadBodyExactlyAtLimit(t *testing.T) {
	full := bytes.Repeat([]byte("y"), int(MaxBodySize))
	body := io.NopCloser(bytes.NewReader(full))

	data, err := ReadBody(context.Background(), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if int64(len(data)) != MaxBodySize {
		t.Fatalf("read %d bytes, want %d", len(data), MaxBodySize)
	}
}

func TestReadBodyContextCancelAbortsRead(t *testing.T) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)

	go func() {
		_, err := ReadBody(ctx, pr)
		errCh <- err
	}()

	// Give the goroutine time to block on Read, then simulate the client
	// going away.
	cancel()

	// Closing the writer as well so no data is ever produced.
	_ = pw.Close()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestReadAndRestoreBody(t *testing.T) {
	original := io.NopCloser(strings.NewReader("payload"))

	data, restored, err := ReadAndRestoreBody(context.Background(), original)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(data) != "payload" {
		t.Fatalf("data = %q", data)
	}

	again, err := io.ReadAll(restored)
	if err != nil {
		t.Fatalf("unexpected error reading restored body: %v", err)
	}

	if string(again) != "payload" {
		t.Fatalf("restored body = %q, want %q", again, "payload")
	}
}

// Package httpio contains safe helpers for reading HTTP message bodies.
package httpio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// MaxBodySize caps how much of an intercepted request/response body is
// buffered in memory while it is being inspected and restored. Without a cap
// a single huge response (or a client trickling data forever) can exhaust
// server memory. 10 MiB matches the size at which bodies stop being practical
// to inspect in the admin UI anyway.
const MaxBodySize int64 = 10 << 20

// ErrBodyTooLarge is returned when a body exceeds MaxBodySize bytes.
var ErrBodyTooLarge = errors.New("httpio: message body exceeds maximum allowed size")

type drainResult struct {
	data []byte
	err  error
}

// ReadBody reads at most MaxBodySize bytes from body into memory.
//
// It differs from io.ReadAll in two ways:
//
//   - The read is bounded: ErrBodyTooLarge is returned instead of buffering an
//     unbounded amount of memory.
//   - Reading aborts as soon as ctx is cancelled (e.g. the client disconnects),
//     so a vanished peer can no longer pin a goroutine and an ever growing
//     buffer until the socket times out.
//
// body is always closed before ReadBody returns. Callers that need to forward
// the body afterwards should use ReadAndRestoreBody instead.
func ReadBody(ctx context.Context, body io.ReadCloser) ([]byte, error) {
	resultCh := make(chan drainResult, 1)

	go func() {
		// Copy one extra byte to detect an oversize body: if MaxBodySize+1
		// bytes come back, the underlying body has more data than allowed.
		data, err := io.ReadAll(io.LimitReader(body, MaxBodySize+1))
		resultCh <- drainResult{data: data, err: err}
	}()

	select {
	case <-ctx.Done():
		// Closing the body unblocks the blocked Read in the goroutine; the
		// buffered result is dropped thanks to the capacity-1 channel.
		_ = body.Close()
		return nil, ctx.Err()
	case result := <-resultCh:
		_ = body.Close()

		if result.err != nil {
			return nil, fmt.Errorf("failed to read message body: %w", result.err)
		}

		if int64(len(result.data)) > MaxBodySize {
			return nil, ErrBodyTooLarge
		}

		return result.data, nil
	}
}

// ReadAndRestoreBody buffers a message body in memory (with the protections of
// ReadBody) and returns it alongside a fresh io.ReadCloser so the message can
// still be forwarded after inspection.
//
// On error the original body is already closed; callers must not forward the
// message anymore.
func ReadAndRestoreBody(ctx context.Context, body io.ReadCloser) ([]byte, io.ReadCloser, error) {
	data, err := ReadBody(ctx, body)
	if err != nil {
		return nil, nil, err
	}

	return data, io.NopCloser(bytes.NewReader(data)), nil
}

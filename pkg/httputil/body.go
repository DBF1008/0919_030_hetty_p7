// Package httputil provides helpers for safely handling HTTP message bodies.
package httputil

import (
	"bytes"
	"errors"
	"io"
)

// DefaultMaxBodySize is the default upper bound (10 MiB) for bodies that are
// buffered in memory, e.g. when parsing intercepted requests or responses.
const DefaultMaxBodySize = 10 << 20

// ErrBodyTooLarge is returned when a body exceeds the configured limit.
var ErrBodyTooLarge = errors.New("httputil: body exceeds maximum allowed size")

// ReadAndRestoreBody reads body up to limit bytes, always closes the original
// body, and returns a restored ReadCloser containing the bytes that were read.
//
// The limit guards against unbounded memory growth from malicious or broken
// clients. If the read fails midway (e.g. the client disconnects), the partial
// data is returned together with the error, and the restored body still
// contains the partial data so downstream consumers never block on a broken
// connection.
func ReadAndRestoreBody(body io.ReadCloser, limit int64) (data []byte, restored io.ReadCloser, err error) {
	if body == nil {
		return nil, nil, nil
	}

	// Always close the original body, even on read errors, so the underlying
	// connection resources are released instead of leaked.
	defer body.Close()

	// Read at most limit+1 bytes to detect overflow without buffering an
	// unbounded amount of data.
	data, err = io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return data, io.NopCloser(bytes.NewReader(data)), err
	}

	if int64(len(data)) > limit {
		return nil, io.NopCloser(bytes.NewReader(nil)), ErrBodyTooLarge
	}

	return data, io.NopCloser(bytes.NewReader(data)), nil
}

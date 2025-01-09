package ioutils // import "github.com/docker/docker/pkg/ioutils"

import (
	"io"
	"sync/atomic"
)

// NopWriter represents a type which write operation is nop.
type NopWriter struct{}

func (*NopWriter) Write(buf []byte) (int, error) {
	return len(buf), nil
}

type nopWriteCloser struct {
	io.Writer
}

func (w *nopWriteCloser) Close() error { return nil }

// NopWriteCloser returns a nopWriteCloser.
func NopWriteCloser(w io.Writer) io.WriteCloser {
	return &nopWriteCloser{w}
}

// NopFlusher represents a type which flush operation is nop.
//
// Deprecated: NopFlusher is only used internally and will be removed in the next release.
type NopFlusher = nopFlusher

type writeCloserWrapper struct {
	io.Writer
	closer func() error
	closed atomic.Bool
}

func (r *writeCloserWrapper) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		subsequentCloseWarn("WriteCloserWrapper")
		return nil
	}
	return r.closer()
}

// NewWriteCloserWrapper returns a new io.WriteCloser.
func NewWriteCloserWrapper(r io.Writer, closer func() error) io.WriteCloser {
	return &writeCloserWrapper{
		Writer: r,
		closer: closer,
	}
}

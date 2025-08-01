package local

import (
	"io"
	"sync"

	"github.com/Microsoft/hcsshim"
	"github.com/moby/moby/v2/pkg/ioutils"
)

type autoClosingReader struct {
	io.ReadCloser
	sync.Once
}

func (r *autoClosingReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if err != nil {
		r.Once.Do(func() { r.ReadCloser.Close() })
	}
	return n, err
}

func createStdInCloser(pipe io.WriteCloser, process hcsshim.Process) io.WriteCloser {
	return ioutils.NewWriteCloserWrapper(pipe, func() error {
		if err := pipe.Close(); err != nil {
			return err
		}

		err := process.CloseStdin()
		if err != nil && !hcsshim.IsNotExist(err) && !hcsshim.IsAlreadyClosed(err) {
			// This error will occur if the compute system is currently shutting down
			if perr, ok := err.(*hcsshim.ProcessError); ok && perr.Err != hcsshim.ErrVmcomputeOperationInvalidState {
				return err
			}
		}

		return nil
	})
}

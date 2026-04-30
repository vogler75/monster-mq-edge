//go:build windows

package winccua

import (
	"context"
	"io"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialPipe opens \\.\pipe\HmiRuntime as a duplex named pipe with overlapped
// I/O so concurrent reads and writes from separate goroutines do not block
// each other (the same reason WinCCUaPipeConnector.kt uses
// AsynchronousFileChannel — see the file's class doc).
func dialPipe(ctx context.Context, path string, timeout time.Duration) (io.ReadWriteCloser, error) {
	dialCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return winio.DialPipeContext(dialCtx, path)
}

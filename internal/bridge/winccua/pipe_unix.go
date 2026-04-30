//go:build !windows

package winccua

import (
	"context"
	"io"
	"os"
	"time"
)

// dialPipe opens the FIFO created by WinCC RT (default /tmp/HmiRuntime) for
// duplex I/O. On Linux the user must be a member of group "industrial".
func dialPipe(_ context.Context, path string, _ time.Duration) (io.ReadWriteCloser, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}

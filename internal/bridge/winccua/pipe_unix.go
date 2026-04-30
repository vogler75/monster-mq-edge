//go:build !windows

package winccua

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"time"
)

// dialPipe connects to WinCC Unified RT on Linux.
//
// Despite Siemens' documentation using the term "FIFO", WinCC RT exposes its
// Open Pipe endpoint as a Unix domain socket (SOCK_STREAM) so that each client
// gets its own full-duplex channel — the same isolation Windows named-pipe
// instances provide. We dial the Unix socket first, which gives proper context
// cancellation and timeout support. If the path turns out to be an actual FIFO
// (ENOTSOCK / ENXIO from connect), we fall back to O_RDWR file open; O_RDWR
// on a Linux FIFO is non-blocking and lets two parties share the kernel buffer.
//
// On Linux the OS user must be a member of the "industrial" group.
func dialPipe(ctx context.Context, path string, timeout time.Duration) (io.ReadWriteCloser, error) {
	dialCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", path)
	if err == nil {
		return conn, nil
	}
	// Path is a FIFO, not a socket — fall back to plain file open.
	if isNotSocket(err) {
		if dialCtx.Err() != nil {
			return nil, dialCtx.Err()
		}
		return os.OpenFile(path, os.O_RDWR, 0)
	}
	return nil, err
}

// isNotSocket reports whether err from a Unix-socket dial means the path
// exists but is not a socket (e.g. a FIFO or regular file).
func isNotSocket(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var sysErr *os.SyscallError
		if errors.As(opErr.Err, &sysErr) {
			return errors.Is(sysErr.Err, syscall.ENOTSOCK) ||
				errors.Is(sysErr.Err, syscall.ENXIO)
		}
	}
	return false
}

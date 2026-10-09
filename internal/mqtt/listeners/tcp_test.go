package listeners

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestTCPClosesConnectionAcceptedDuringShutdown(t *testing.T) {
	l := NewTCP(Config{ID: "t1", Address: "127.0.0.1:0"})
	if err := l.Init(slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.listen.Close() })

	var established atomic.Bool
	go l.Serve(func(string, net.Conn) error {
		established.Store(true)
		return nil
	})
	time.Sleep(20 * time.Millisecond) // let Serve block in Accept

	// Shutdown begins while Serve is blocked in Accept; the next connection is
	// accepted but must be closed rather than leaked.
	atomic.StoreUint32(&l.end, 1)

	conn, err := net.Dial("tcp", l.Address())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read = %v, want EOF from a server-side close", err)
	}
	if established.Load() {
		t.Fatal("establish called after shutdown began")
	}
}

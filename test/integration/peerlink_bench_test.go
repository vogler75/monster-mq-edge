package integration

import (
	"log/slog"
	"path/filepath"
	"testing"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
)

// BenchmarkInjectPublish drives server.InjectPacket on a broker built with
// its full hook set (22.3, anchors 21.1): without PeerLink, and with PeerLink
// capturing into the log for one (never connected) consumer.
func BenchmarkInjectPublish(b *testing.B) {
	for _, tc := range []struct {
		name     string
		peerLink bool
	}{{"plain", false}, {"peerlink", true}} {
		b.Run(tc.name, func(b *testing.B) {
			cfg := plConfig("bench-"+tc.name, 0, 27388, b.TempDir(), []config.PeerConfig{plPeer("bench-peer", 0)})
			cfg.SQLite.Path = filepath.Join(b.TempDir(), "bench.db")
			cfg.PeerLink.Enabled = tc.peerLink
			cfg.PeerLink.Log.NeverConnectedWarnSec = intPtr(0)
			srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
			if err != nil {
				b.Fatal(err)
			}
			if err := srv.Serve(); err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = srv.Close() })
			eng := srv.MQTT()
			cl := eng.NewClient(nil, "tcp", "bench-publisher", false)
			cl.Properties.ProtocolVersion = 5
			cl.State.Inflight.ResetReceiveQuota(1 << 20)
			pk := packets.Packet{
				FixedHeader: packets.FixedHeader{Type: packets.Publish},
				TopicName:   "bench/line1/temperature",
				Payload:     make([]byte, 200),
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := eng.InjectPacket(cl, pk); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

package peerlink

import (
	"context"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
)

func benchNode(b *testing.B, id string, peers []config.PeerConfig) *testNode {
	n := newNode(b, id, "127.0.0.1:0", peers)
	_ = n.srv.Unsubscribe("#", 1)
	return n
}

// BenchmarkCapture measures the hook capture path for a 200-byte record: filters, encode, append.
func BenchmarkCapture(b *testing.B) {
	n := benchNode(b, "node-a", []config.PeerConfig{{NodeID: "node-b"}})
	cl := n.srv.NewClient(nil, "tcp", "bench-client-01", false)
	pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, TopicName: "plant/area/line/machine/temp01",
		Payload: make([]byte, 100)}
	h := n.m.Hook()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.OnPublished(cl, pk)
	}
}

// BenchmarkApplyBatch measures the receiver: decode, validate and InjectPacket into an engine with
// no subscribers, 4096 records per batch.
func BenchmarkApplyBatch(b *testing.B) {
	n := benchNode(b, "node-b", []config.PeerConfig{{NodeID: "node-a", Address: "127.0.0.1:1", Serve: boolp(false)}})
	p := n.m.pullers[0]
	const count = 4096
	var region []byte
	for i := 0; i < count; i++ {
		region = wire.AppendRecord(region, &wire.Record{Topic: "plant/area/line/machine/temp01", ClientID: "dev", Payload: make([]byte, 100),
			CaptureMonoMs: 1000})
	}
	ac := &applyCtx{ctx: context.Background(), epoch: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		in := &batchIn{recvAt: time.Now()}
		in.b = wire.Batch{Header: wire.BatchHeader{BaseOffset: uint64(i*count + 1), Count: count, RecordsBytes: uint32(len(region)),
			Leo: uint64((i + 1) * count), SourceMonoMs: 1000}, Records: region}
		p.appliedNext.Store(uint64(i*count + 1))
		p.applyBatch(ac, in)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*count), "ns/record")
}

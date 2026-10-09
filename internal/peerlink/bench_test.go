package peerlink

import (
	"context"
	"fmt"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
)

func benchNode(b *testing.B, id string, peers []config.PeerConfig, opts ...nodeOpt) *testNode {
	n := newNode(b, id, "127.0.0.1:0", peers, opts...)
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

// BenchmarkCaptureInterest is gate G-IR1 on the source (plan-peerlink-interest-routing 12.4): two
// consumers, 1k or 10k remote filters per peer (half wildcards), and 0, 10 or 100 % of publishes
// wanted by a peer, against interest routing off. Skipped publishes must not allocate.
func BenchmarkCaptureInterest(b *testing.B) {
	const topics = 100
	names := make([]string, topics)
	for i := range names {
		names[i] = fmt.Sprintf("skip/area/line%d/machine/temp", i)
	}
	peers := []config.PeerConfig{{NodeID: "node-b"}, {NodeID: "node-c"}}
	run := func(b *testing.B, n *testNode, pct int) {
		for i := range names {
			if i < pct {
				names[i] = fmt.Sprintf("want/area/line%d/machine/temp", i)
			} else {
				names[i] = fmt.Sprintf("skip/area/line%d/machine/temp", i)
			}
		}
		cl := n.srv.NewClient(nil, "tcp", "bench-client-01", false)
		pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, Payload: make([]byte, 100)}
		h := n.m.Hook()
		before := n.m.log.Stats().AppendedBytes
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			pk.TopicName = names[i%topics]
			h.OnPublished(cl, pk)
		}
		b.StopTimer()
		b.ReportMetric(float64(n.m.log.Stats().AppendedBytes-before)/float64(b.N), "logB/op")
	}
	for _, pct := range []int{0, 10, 100} {
		b.Run(fmt.Sprintf("off/%d%%", pct), func(b *testing.B) {
			run(b, benchNode(b, "node-a", peers), pct)
		})
	}
	for _, filters := range []int{1000, 10000} {
		entries := make([]wire.InterestEntry, filters)
		for j := range entries {
			f := fmt.Sprintf("want/area/line%d/machine/temp", j)
			if j%2 == 1 {
				f = fmt.Sprintf("want/area/line%d/+/temp", j)
			}
			entries[j] = wire.InterestEntry{Class: wire.InterestVol, Filter: f}
		}
		for _, pct := range []int{0, 10, 100} {
			b.Run(fmt.Sprintf("%dfilters/%d%%", filters, pct), func(b *testing.B) {
				n := benchNode(b, "node-a", peers, withInterest)
				for p := range peers {
					n.m.interest.connect(p, uint64(p+1), true)
					snap := &wire.InterestSnapshot{Generation: 1, Flags: wire.InterestFlagFirst | wire.InterestFlagLast, Entries: entries}
					if err := n.m.interest.applySnapshot(p, snap, time.Now()); err != nil {
						b.Fatal(err)
					}
				}
				run(b, n, pct)
			})
		}
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

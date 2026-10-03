package wire

import (
	"fmt"
	"strings"
	"testing"

	"monstermq.io/edge/internal/mqtt/packets"
)

var sinkBytes []byte

// benchRecord is the plan's reference record (8.7): topic 40 B, client id 16 B, payload 100 B = 200 B.
func benchRecord(i int) Record {
	topic := fmt.Sprintf("plant/area-01/line-02/cell-03/sens%06d", i)
	return Record{
		Flags:         1,
		PublishWallNs: 1_759_500_000_000_000_000 + int64(i),
		CaptureMonoMs: uint64(i),
		Topic:         topic[:40],
		ClientID:      "client-000000001",
		Payload:       []byte(strings.Repeat("p", 100)),
	}
}

func BenchmarkEncodeRecord(b *testing.B) {
	r := benchRecord(1)
	if n := RecordSize(&r); n != 200 {
		b.Fatalf("reference record is %d B, want 200", n)
	}
	b.Run("alloc", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(200)
		for b.Loop() {
			buf := make([]byte, RecordSize(&r))
			EncodeRecord(buf, &r)
			sinkBytes = buf
		}
	})
	b.Run("reuse", func(b *testing.B) {
		buf := make([]byte, RecordSize(&r))
		b.ReportAllocs()
		b.SetBytes(200)
		for b.Loop() {
			EncodeRecord(buf[:RecordSize(&r)], &r)
		}
	})
	b.Run("props", func(b *testing.B) {
		p := r
		p.Flags |= FlagPayloadFormat
		p.PayloadFormat = 1
		p.ContentType = "application/json"
		p.User = []packets.UserProperty{{Key: "site", Val: "a"}, {Key: "unit", Val: "degC"}}
		b.ReportAllocs()
		b.SetBytes(int64(RecordSize(&p)))
		for b.Loop() {
			buf := make([]byte, RecordSize(&p))
			EncodeRecord(buf, &p)
			sinkBytes = buf
		}
	})
}

func BenchmarkDecodeBatch(b *testing.B) {
	const count = 4096
	recs := make([][]byte, count)
	for i := range recs {
		r := benchRecord(i)
		recs[i] = AppendRecord(nil, &r)
	}
	for _, crc := range []bool{false, true} {
		h := BatchHeader{BaseOffset: 1, SourceMonoMs: count}
		if crc {
			h.Flags = BatchFlagCRC
		}
		frame := buildBatch(b, h, recs...)
		body := frame[FrameHeaderLen:]
		b.Run(fmt.Sprintf("crc=%v", crc), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			var bt Batch
			var v RecordView
			for b.Loop() {
				if err := bt.Decode(body); err != nil {
					b.Fatal(err)
				}
				if !bt.CRCValid() {
					b.Fatal("crc")
				}
				it := bt.Iter()
				n := 0
				for {
					ok, err := it.Next(&v)
					if !ok {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
					n++
				}
				if n != count {
					b.Fatalf("decoded %d records", n)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*count), "ns/record")
		})
	}
}

func TestDecodeBatchNoAlloc(t *testing.T) {
	recs := make([][]byte, 64)
	for i := range recs {
		r := benchRecord(i)
		r.ContentType = "text/plain"
		r.User = []packets.UserProperty{{Key: "k", Val: "v"}}
		recs[i] = AppendRecord(nil, &r)
	}
	frame := buildBatch(t, BatchHeader{Flags: BatchFlagCRC, SourceMonoMs: 1 << 20}, recs...)
	body := frame[FrameHeaderLen:]
	var bt Batch
	var v RecordView
	allocs := testing.AllocsPerRun(50, func() {
		if err := bt.Decode(body); err != nil || !bt.CRCValid() {
			t.Fatal("decode")
		}
		it := bt.Iter()
		for {
			ok, err := it.Next(&v)
			if !ok {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	})
	if allocs != 0 {
		t.Fatalf("batch decode allocates %.1f per batch", allocs)
	}
	r := benchRecord(1)
	buf := make([]byte, RecordSize(&r))
	if a := testing.AllocsPerRun(50, func() { EncodeRecord(buf, &r) }); a != 0 {
		t.Fatalf("EncodeRecord allocates %.1f", a)
	}
}

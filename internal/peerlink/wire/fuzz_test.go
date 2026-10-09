package wire

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"monstermq.io/edge/internal/mqtt/packets"
)

func recordSeeds(t testing.TB) [][]byte {
	full := fullRecord()
	ts := encode(t, &full)
	return [][]byte{
		ts,
		encode(t, &Record{Topic: "a"}),
		AppendTombstone(nil, ts),
		rawRec{hdrLen: 52, topic: []byte("t"), props: append(tlv(0x7f, []byte("x")), userTLV("k", "v")...)}.build(),
		rawRec{topic: []byte("a/+")}.build(),
		rawRec{topic: []byte("a"), props: []byte{PropContentType, 9, 0, 0, 0}}.build(),
		{4, 0, 0, 0, 1},
	}
}

func FuzzDecodeRecord(f *testing.F) {
	for _, s := range recordSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var v RecordView
		err := DecodeRecord(data, &v)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("error %v does not match ErrMalformed", err)
			}
			return
		}
		if len(v.Frame) != RecordFrameLen(data) || int(v.HdrLen) < RecordHeaderLen || v.QoS() > 2 {
			t.Fatalf("accepted an inconsistent record: %+v", v)
		}
		if v.Skipped() {
			return
		}
		// Whatever the decoder accepts, capture would accept and re-encode to an equivalent record.
		r := v.Record()
		if !r.ValidContent() {
			t.Fatalf("decoder accepted content that capture rejects: %+v", r)
		}
		enc := AppendRecord(nil, &r)
		if len(enc) == 0 {
			t.Fatalf("accepted record cannot be re-encoded")
		}
		var v2 RecordView
		if err := DecodeRecord(enc, &v2); err != nil {
			t.Fatalf("re-encoded record rejected: %v", err)
		}
		if v2.UnknownProps != 0 || v2.Flags != v.Flags || v2.PublishWallNs != v.PublishWallNs ||
			v2.CaptureMonoMs != v.CaptureMonoMs || v2.ExpirySec != v.ExpirySec || v2.PayloadFormat != v.PayloadFormat {
			t.Fatalf("header changed in round trip: %+v vs %+v", v2, v)
		}
		if got := v2.Record(); !reflect.DeepEqual(normalized(got), normalized(r)) {
			t.Fatalf("round trip mismatch\n got %+v\nwant %+v", got, r)
		}
	})
}

func frameSeeds() [][]byte {
	var all []byte
	var seeds [][]byte
	for _, f := range sampleFrames() {
		enc := f.AppendFrame(nil)
		seeds = append(seeds, enc)
		all = append(all, enc...)
	}
	full := fullRecord()
	recs := [][]byte{AppendRecord(nil, &full), AppendRecord(nil, &Record{Topic: "b", CaptureMonoMs: 1})}
	h := BatchHeader{Flags: BatchFlagCRC, Count: 2, SourceMonoMs: 1 << 40,
		RecordsBytes: uint32(len(recs[0]) + len(recs[1]))}
	var prefix [BatchPrefixLen]byte
	EncodeBatchPrefix(&prefix, &h)
	SetBatchCRC(&prefix, nil, recs)
	batch := bytes.Join(append([][]byte{prefix[:]}, recs...), nil)
	seeds = append(seeds, batch, all, []byte{6, 0, 0, 0, 0x7e, 1, 2, 3, 4, 5})
	return seeds
}

func FuzzFrame(f *testing.F) {
	for _, s := range frameSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fr := NewFrameReader(bytes.NewReader(data), 1<<20)
		for range 64 {
			typ, body, err := fr.ReadFrame()
			if err != nil {
				return
			}
			fm, err := DecodeFrame(typ, body)
			if err != nil {
				if !errors.Is(err, ErrUnknownFrame) && !errors.Is(err, ErrShortFrame) && !errors.Is(err, ErrBatchRecords) && !errors.Is(err, ErrBatchCountRange) && !errors.Is(err, ErrInterestCount) && !errors.Is(err, ErrBatchSparse) {
					t.Fatalf("unexpected error %v", err)
				}
				if errors.Is(err, ErrUnknownFrame) == typ.Known() {
					t.Fatalf("type %#x: Known() = %v but err = %v", byte(typ), typ.Known(), err)
				}
				continue
			}
			if fm.Type() != typ {
				t.Fatalf("decoded %s from type %#x", fm.Type(), byte(typ))
			}
			enc := fm.AppendFrame(nil)
			fr2 := NewFrameReader(bytes.NewReader(enc), 1<<21)
			typ2, body2, err := fr2.ReadFrame()
			if err != nil || typ2 != typ {
				t.Fatalf("re-encoded frame unreadable: %v", err)
			}
			fm2, err := DecodeFrame(typ2, body2)
			if err != nil {
				t.Fatalf("re-encoded frame rejected: %v", err)
			}
			if !reflect.DeepEqual(withoutRaw(fm2), withoutRaw(fm)) {
				t.Fatalf("round trip mismatch\n got %+v\nwant %+v", fm2, fm)
			}
			if b, ok := fm.(*Batch); ok {
				_ = b.CRCValid()
				it := b.Iter()
				var v RecordView
				for {
					ok, err := it.Next(&v)
					if !ok {
						break
					}
					if err != nil && !errors.Is(err, ErrMalformed) {
						t.Fatalf("per-record error %v does not match ErrMalformed", err)
					}
				}
			}
		}
	})
}

func FuzzTLV(f *testing.F) {
	f.Add(tlv(PropContentType, []byte("ct")))
	f.Add(append(userTLV("k", "v"), tlv(0x55, []byte("future"))...))
	f.Add([]byte{PropUserProperty, 3, 0, 0, 0, 9, 0, 'k'})
	f.Fuzz(func(t *testing.T, block []byte) {
		unknown, err := ValidateProps(block)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("error %v does not match ErrMalformed", err)
			}
			return
		}
		it := NewPropIter(block)
		n, consumed := 0, 0
		for {
			id, val, ok := it.Next()
			if !ok {
				break
			}
			consumed += TLVHeaderLen + len(val)
			switch id {
			case PropContentType, PropResponseTopic, PropCorrelationData, PropUserProperty:
			default:
				n++
			}
		}
		if n != unknown || consumed != len(block) {
			t.Fatalf("unknown %d vs %d, consumed %d of %d", n, unknown, consumed, len(block))
		}
		v := RecordView{Props: block}
		var p packets.Properties
		v.Properties(&p)
	})
}

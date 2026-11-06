package wire

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

func TestPrimitivesRoundTrip(t *testing.T) {
	w := NewWriter()
	w.Int8(-3)
	w.Int16(-2)
	w.Int32(70000)
	w.Int64(-1 << 40)
	w.String("héllo")
	w.NullString(nil)
	s := "x"
	w.NullString(&s)
	w.Bytes32([]byte{1, 2, 3})
	w.Bytes32(nil)
	w.Varint(-1)
	w.Varint(300)
	w.Varlong(-1 << 50)

	r := NewReader(w.Bytes())
	if r.Int8() != -3 || r.Int16() != -2 || r.Int32() != 70000 || r.Int64() != -1<<40 {
		t.Fatal("fixed ints")
	}
	if r.String() != "héllo" {
		t.Fatal("string")
	}
	if r.NullableString() != nil {
		t.Fatal("null string")
	}
	if p := r.NullableString(); p == nil || *p != "x" {
		t.Fatal("nullable string")
	}
	if !bytes.Equal(r.Bytes(), []byte{1, 2, 3}) || r.Bytes() != nil {
		t.Fatal("bytes")
	}
	if r.Varint() != -1 || r.Varint() != 300 || r.Varlong() != -1<<50 {
		t.Fatal("varints")
	}
	if r.Err() != nil || r.Remaining() != 0 {
		t.Fatalf("err=%v remaining=%d", r.Err(), r.Remaining())
	}
}

func TestReaderShortIsSticky(t *testing.T) {
	r := NewReader([]byte{0, 1})
	if r.Int32() != 0 || !errors.Is(r.Err(), ErrShort) {
		t.Fatal("expected short read")
	}
	if r.Int8() != 0 || r.String() != "" {
		t.Fatal("reads after error must return zero values")
	}
}

func TestArrayLenRejectsHugeCounts(t *testing.T) {
	w := NewWriter()
	w.Int32(1 << 30)
	r := NewReader(w.Bytes())
	if n := r.ArrayLen(4); n != 0 || r.Err() == nil {
		t.Fatalf("n=%d err=%v", n, r.Err())
	}
	w = NewWriter()
	w.Int32(-1)
	if n := NewReader(w.Bytes()).ArrayLen(4); n != 0 {
		t.Fatal("null array")
	}
}

// recordBytes encodes one v2 record.
func recordBytes(key, val []byte, nullKey bool) []byte {
	b := NewWriter()
	b.Int8(0)    // attributes
	b.Varlong(0) // timestamp delta
	b.Varint(0)  // offset delta
	if nullKey {
		b.Varint(-1)
	} else {
		b.Varint(int32(len(key)))
		b.Raw(key)
	}
	if val == nil {
		b.Varint(-1)
	} else {
		b.Varint(int32(len(val)))
		b.Raw(val)
	}
	b.Varint(0) // headers
	out := NewWriter()
	out.Varint(int32(b.Len()))
	out.Raw(b.Bytes())
	return out.Bytes()
}

type tr struct {
	key, val []byte
	nullKey  bool
}

// batchV2 builds a RecordBatch v2 (CRC not computed; the decoder ignores it).
func batchV2(codec int, recs []tr) []byte {
	body := NewWriter()
	for _, r := range recs {
		body.Raw(recordBytes(r.key, r.val, r.nullKey))
	}
	payload := body.Bytes()
	if codec == 1 {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(payload)
		_ = zw.Close()
		payload = buf.Bytes()
	}
	b := NewWriter()
	b.Int32(0) // partition leader epoch
	b.Int8(2)  // magic
	b.Int32(0) // crc
	b.Int16(int16(codec))
	b.Int32(int32(len(recs) - 1)) // last offset delta
	b.Int64(0)                    // base timestamp
	b.Int64(0)                    // max timestamp
	b.Int64(-1)                   // producer id
	b.Int16(-1)                   // producer epoch
	b.Int32(-1)                   // base sequence
	b.Int32(int32(len(recs)))
	b.Raw(payload)
	out := NewWriter()
	out.Int64(0) // base offset
	out.Int32(int32(b.Len()))
	out.Raw(b.Bytes())
	return out.Bytes()
}

func TestDecodeRecordBatchV2(t *testing.T) {
	for _, codec := range []int{0, 1} {
		set := batchV2(codec, []tr{
			{key: []byte("a/b"), val: []byte("1")},
			{nullKey: true, val: []byte("2")},
			{key: []byte("c"), val: nil},      // null value skipped
			{key: []byte("d"), val: []byte{}}, // empty value skipped
			{key: []byte("e/f"), val: []byte("5")},
		})
		set = append(set, batchV2(codec, []tr{{key: []byte("g"), val: []byte("6")}})...)
		set = append(set, 1, 2, 3) // truncated trailing garbage is ignored
		recs, err := DecodeRecords(set)
		if err != nil {
			t.Fatalf("codec %d: %v", codec, err)
		}
		if len(recs) != 4 {
			t.Fatalf("codec %d: got %d records", codec, len(recs))
		}
		if string(recs[0].Key) != "a/b" || string(recs[0].Value) != "1" || recs[1].Key != nil ||
			string(recs[2].Key) != "e/f" || string(recs[3].Key) != "g" {
			t.Fatalf("codec %d: %+v", codec, recs)
		}
	}
}

func TestDecodeRecordBatchUnsupportedCodec(t *testing.T) {
	for _, codec := range []int{2, 3, 4} { // snappy, lz4, zstd
		_, err := DecodeRecords(batchV2(codec, []tr{{key: []byte("a"), val: []byte("1")}}))
		var ce *UnsupportedCodecError
		if !errors.As(err, &ce) || ce.Codec != codec {
			t.Fatalf("codec %d: err=%v", codec, err)
		}
	}
}

func TestDecodeRecordBatchCorruptGzip(t *testing.T) {
	set := batchV2(1, []tr{{key: []byte("a"), val: []byte("1")}})
	for i := len(set) - 8; i < len(set); i++ {
		set[i] ^= 0xff
	}
	if _, err := DecodeRecords(set); err == nil {
		t.Fatal("expected error for corrupt gzip")
	}
}

func legacyMsg(magic int8, attrs int8, key, val []byte) []byte {
	body := NewWriter()
	body.Int8(magic)
	body.Int8(attrs)
	if magic == 1 {
		body.Int64(0)
	}
	body.Bytes32(key)
	body.Bytes32(val)
	m := NewWriter()
	m.Int64(0) // offset
	m.Int32(int32(4 + body.Len()))
	m.Int32(int32(crc32.ChecksumIEEE(body.Bytes())))
	m.Raw(body.Bytes())
	return m.Bytes()
}

func TestDecodeLegacyMessageSet(t *testing.T) {
	set := append(legacyMsg(0, 0, []byte("t/1"), []byte("v1")), legacyMsg(1, 0, nil, []byte("v2"))...)
	set = append(set, legacyMsg(0, 0, []byte("x"), nil)...) // null value skipped
	recs, err := DecodeRecords(set)
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs=%+v err=%v", recs, err)
	}
	if string(recs[0].Key) != "t/1" || string(recs[0].Value) != "v1" || recs[1].Key != nil || string(recs[1].Value) != "v2" {
		t.Fatalf("%+v", recs)
	}
}

func TestDecodeLegacyGzipWrapper(t *testing.T) {
	inner := append(legacyMsg(0, 0, []byte("a"), []byte("1")), legacyMsg(0, 0, []byte("b"), []byte("2"))...)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(inner)
	_ = zw.Close()
	recs, err := DecodeRecords(legacyMsg(0, 1, nil, buf.Bytes()))
	if err != nil || len(recs) != 2 || string(recs[1].Key) != "b" {
		t.Fatalf("recs=%+v err=%v", recs, err)
	}
	_, err = DecodeRecords(legacyMsg(0, 2, nil, []byte("snappy")))
	var ce *UnsupportedCodecError
	if !errors.As(err, &ce) {
		t.Fatalf("err=%v", err)
	}
}

func TestEncodeMessageSetV0RoundTrip(t *testing.T) {
	set := EncodeMessageSetV0([]MessageV0{
		{Offset: 7, Key: []byte("a/b"), Value: []byte("hello")},
		{Offset: 8, Key: []byte("c"), Value: []byte{0, 1, 2}},
	})
	// verify framing and CRC by hand
	off := 0
	for i, want := range []struct {
		off int64
		key string
	}{{7, "a/b"}, {8, "c"}} {
		if got := int64(binary.BigEndian.Uint64(set[off:])); got != want.off {
			t.Fatalf("msg %d offset %d", i, got)
		}
		size := int(binary.BigEndian.Uint32(set[off+8:]))
		crc := binary.BigEndian.Uint32(set[off+12:])
		body := set[off+16 : off+12+size]
		if crc32.ChecksumIEEE(body) != crc {
			t.Fatalf("msg %d crc mismatch", i)
		}
		off += 12 + size
	}
	if off != len(set) {
		t.Fatal("trailing bytes")
	}
	recs, err := DecodeRecords(set)
	if err != nil || len(recs) != 2 || string(recs[0].Key) != "a/b" || string(recs[0].Value) != "hello" {
		t.Fatalf("recs=%+v err=%v", recs, err)
	}
}

func TestSupportedRange(t *testing.T) {
	if len(SupportedAPIs) != 15 {
		t.Fatalf("want 15 APIs, got %d", len(SupportedAPIs))
	}
	if r, ok := SupportedRange(APIFetch); !ok || r.Max != 10 {
		t.Fatal("fetch range")
	}
	if _, ok := SupportedRange(99); ok {
		t.Fatal("unknown key")
	}
}

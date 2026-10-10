package wire

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func nonce(seed byte) (n [32]byte) {
	for i := range n {
		n[i] = seed + byte(i)
	}
	return n
}

func sampleFrames() []Frame {
	return []Frame{
		&ServerHello{VersionMajor: 1, VersionMinor: 3, Capabilities: CapsV1 | 1<<40, AuthModes: AuthClientCertRequested | AuthSharedSecret, NonceS: nonce(1)},
		&Hello{
			Flags: HelloFlagMAC, Capabilities: CapsV1, InstanceID: 0xdeadbeefcafe, LastEpoch: 77, ResumeOffset: 1000,
			LastSeenLeo: 2000, MaxRecordBytes: 1<<20 + 64<<10, RetainedClass: RetainedWinCCOA, NonceC: nonce(2), MAC: nonce(3),
			ConsumerNodeID: "oa-b", ExpectedSourceNodeID: "oa-a", TopicRoot: "winccoa", OASystem: "System1",
			BrokerType: BrokerTypeEdge, BrokerVersion: "1.4.2+abc",
		},
		&Hello{ConsumerNodeID: "b", ExpectedSourceNodeID: "a"},
		&HelloOK{
			Flags: HelloOKSourceReset | HelloOKSnapshotAvailable, Capabilities: CapTombstone | CapSnapshotFill, Epoch: 0x1234567890abcdef,
			ResumeAt: 5, LogStart: 3, Leo: 9, Committed: 5, LostOnResume: 2, WallNowMs: -1, MonoNowMs: 42,
			MaxRecordBytes: 1 << 20, RetainedClass: RetainedDB, MACS: nonce(4), SourceNodeID: "oa-a", TopicRoot: "", OASystem: "",
			BrokerType: BrokerTypeFull, BrokerVersion: "1.8.33",
		},
		&GoAway{Code: GoAwayShutdown, Reason: "source stopping"},
		&GoAway{Code: GoAwayAuthFailed},
		&Fetch{FetchID: 7, Flags: FetchFlagSnapshot, LingerMs: 5, Offset: 100, Commit: 99, MaxRecords: 4096, MaxBytes: 1 << 20, MinRecords: 1, MaxWaitMs: 1000},
		&Batch{Header: BatchHeader{FetchID: 7, Flags: BatchFlagGap | BatchFlagTruncated, Reserved: 3, BaseOffset: 105, Count: 2,
			RecordsBytes: 8, LogStart: 105, Leo: 200, Lost: 5, SourceMonoMs: 9000, SourceWallMs: 1_759_500_000_000, CRC32C: 0xabcdef01},
			Records: []byte("abcdefgh")},
		&Batch{Header: BatchHeader{Flags: BatchFlagEmpty, BaseOffset: 1}, Records: []byte{}},
		&Commit{Commit: 123456789},
		&Ping{Token: 1},
		&Pong{Token: 0xffffffffffffffff},
		&InterestSnapshot{Generation: 3, Flags: InterestFlagFirst | InterestFlagLast, Entries: []InterestEntry{
			{Class: InterestVol, Filter: "a/#"}, {Class: InterestPer, ExpirySec: InterestExpiryNever, Filter: "b/+"}}},
		&InterestDelta{Generation: 4, Entries: []InterestEntry{{Class: InterestNone, Filter: "a/#"}, {Class: InterestPer, ExpirySec: 60, Filter: "c"}}},
	}
}

// isInterest reports frames whose body ends with a counted entry list: bytes after the entries
// or a truncated entry are ErrInterestCount rather than ignored or ErrShortFrame.
func isInterest(f Frame) bool {
	return f.Type() == FrameInterestSnapshot || f.Type() == FrameInterestDelta
}

// withoutRaw drops the unexported received-header view of a decoded batch.
func withoutRaw(f Frame) Frame {
	if b, ok := f.(*Batch); ok {
		c := *b
		c.raw = nil
		return &c
	}
	return f
}

func TestFrameRoundTrip(t *testing.T) {
	for _, f := range sampleFrames() {
		t.Run(f.Type().String(), func(t *testing.T) {
			enc := f.AppendFrame([]byte("xyz"))
			if string(enc[:3]) != "xyz" {
				t.Fatal("AppendFrame clobbered the prefix")
			}
			enc = enc[3:]
			fr := NewFrameReader(bytes.NewReader(enc), 1<<20)
			typ, body, err := fr.ReadFrame()
			if err != nil || typ != f.Type() {
				t.Fatalf("ReadFrame: %v %v", typ, err)
			}
			if int(binary.LittleEndian.Uint32(enc)) != len(enc)-4 {
				t.Fatalf("frameLen %d for %d bytes", binary.LittleEndian.Uint32(enc), len(enc))
			}
			got, err := DecodeFrame(typ, body)
			if err != nil {
				t.Fatalf("DecodeFrame: %v", err)
			}
			if !reflect.DeepEqual(withoutRaw(got), f) {
				t.Fatalf("round trip\n got %+v\nwant %+v", got, f)
			}

			// Forward compatibility: fields appended by a later minor version are ignored.
			ext := append(append([]byte(nil), enc...), 0xaa, 0xbb, 0xcc)
			binary.LittleEndian.PutUint32(ext, uint32(len(ext)-4))
			got, err = DecodeFrame(typ, ext[FrameHeaderLen:])
			if isInterest(f) {
				if !errors.Is(err, ErrInterestCount) {
					t.Fatalf("trailing bytes after interest entries: %v", err)
				}
			} else if err != nil || !reflect.DeepEqual(withoutRaw(got), f) {
				t.Fatalf("trailing bytes: %v\n got %+v\nwant %+v", err, got, f)
			}

			var buf bytes.Buffer
			if err := WriteFrame(&buf, f); err != nil || !bytes.Equal(buf.Bytes(), enc) {
				t.Fatalf("WriteFrame differs: %v", err)
			}
		})
	}
}

func TestFrameShortBodies(t *testing.T) {
	for _, f := range sampleFrames() {
		enc := f.AppendFrame(nil)
		body := enc[FrameHeaderLen:]
		for n := 0; n < len(body); n++ {
			if b, ok := f.(*Batch); ok && n >= BatchHeaderLen {
				_ = b
				// A shorter records region is reported separately (ErrBatchRecords).
				if _, err := DecodeFrame(f.Type(), body[:n]); !errors.Is(err, ErrBatchRecords) {
					t.Fatalf("%s truncated to %d: %v", f.Type(), n, err)
				}
				continue
			}
			if isInterest(f) && n >= len(body)-interestBodyLen(interestEntries(f)) {
				if _, err := DecodeFrame(f.Type(), body[:n]); !errors.Is(err, ErrInterestCount) {
					t.Fatalf("%s truncated to %d: %v", f.Type(), n, err)
				}
				continue
			}
			if n == legacyHelloLen(f) {
				continue // a peer that predates brokerType/brokerVersion (TestHelloWithoutBrokerInfo)
			}
			if _, err := DecodeFrame(f.Type(), body[:n]); !errors.Is(err, ErrShortFrame) {
				t.Fatalf("%s truncated to %d of %d: err %v", f.Type(), n, len(body), err)
			}
		}
	}
}

// legacyHelloLen is the body length of a HELLO or HELLO_OK without the trailing brokerType and
// brokerVersion, or -1 for other frames.
func legacyHelloLen(f Frame) int {
	switch m := f.(type) {
	case *Hello:
		return len(m.AppendFrame(nil)) - FrameHeaderLen - 2 - len(m.BrokerType) - len(m.BrokerVersion)
	case *HelloOK:
		return len(m.AppendFrame(nil)) - FrameHeaderLen - 2 - len(m.BrokerType) - len(m.BrokerVersion)
	}
	return -1
}

// A HELLO or HELLO_OK from a peer that predates brokerType/brokerVersion decodes with both empty.
func TestHelloWithoutBrokerInfo(t *testing.T) {
	for _, f := range []Frame{
		&Hello{ConsumerNodeID: "b", ExpectedSourceNodeID: "a", OASystem: "S", BrokerType: BrokerTypeEdge, BrokerVersion: "1.0"},
		&HelloOK{SourceNodeID: "a", OASystem: "S", BrokerType: BrokerTypeFull, BrokerVersion: "2.0"},
	} {
		body := f.AppendFrame(nil)[FrameHeaderLen:]
		got, err := DecodeFrame(f.Type(), body[:legacyHelloLen(f)])
		if err != nil {
			t.Fatalf("%s without broker info: %v", f.Type(), err)
		}
		switch m := got.(type) {
		case *Hello:
			if m.OASystem != "S" || m.BrokerType != "" || m.BrokerVersion != "" {
				t.Fatalf("HELLO: %+v", m)
			}
		case *HelloOK:
			if m.OASystem != "S" || m.BrokerType != "" || m.BrokerVersion != "" {
				t.Fatalf("HELLO_OK: %+v", m)
			}
		}
	}
	if v := ProtocolVersion(VersionMajor, VersionMinor); v != "1.0" {
		t.Fatalf("protocol version %q", v)
	}
}

func interestEntries(f Frame) []InterestEntry {
	switch m := f.(type) {
	case *InterestSnapshot:
		return m.Entries
	case *InterestDelta:
		return m.Entries
	}
	return nil
}

func TestInterestGolden(t *testing.T) {
	snap := (&InterestSnapshot{Generation: 0x01020304, Flags: InterestFlagLast, Entries: []InterestEntry{
		{Class: InterestPer, ExpirySec: 0x0a0b0c0d, Filter: "x/#"}}}).AppendFrame(nil)
	want := []byte{
		20, 0, 0, 0, byte(FrameInterestSnapshot),
		4, 3, 2, 1, InterestFlagLast, 1, 0, 0, 0,
		InterestPer, 0x0d, 0x0c, 0x0b, 0x0a, 3, 0, 'x', '/', '#',
	}
	if !bytes.Equal(snap, want) {
		t.Fatalf("snapshot\n got % x\nwant % x", snap, want)
	}
	if len(snap)-FrameHeaderLen != InterestSnapshotHeaderLen+InterestEntryOverhead+3 {
		t.Fatalf("snapshot body %d", len(snap)-FrameHeaderLen)
	}
	delta := (&InterestDelta{Generation: 7, Entries: []InterestEntry{{Class: InterestNone, Filter: "y"}}}).AppendFrame(nil)
	if got := len(delta) - FrameHeaderLen; got != InterestDeltaHeaderLen+InterestEntryOverhead+1 {
		t.Fatalf("delta body %d", got)
	}
	empty := (&InterestSnapshot{Flags: InterestFlagFirst | InterestFlagLast}).AppendFrame(nil)
	f, err := DecodeFrame(FrameInterestSnapshot, empty[FrameHeaderLen:])
	if err != nil || len(f.(*InterestSnapshot).Entries) != 0 {
		t.Fatalf("empty snapshot: %v %+v", err, f)
	}
	// A count no body could hold is rejected before entries are allocated.
	huge := append([]byte(nil), empty...)
	binary.LittleEndian.PutUint32(huge[FrameHeaderLen+5:], 0xffffffff)
	if _, err := DecodeFrame(FrameInterestSnapshot, huge[FrameHeaderLen:]); !errors.Is(err, ErrInterestCount) {
		t.Fatalf("huge count: %v", err)
	}
}

func TestFrameGolden(t *testing.T) {
	got := (&Commit{Commit: 0x0102030405060708}).AppendFrame(nil)
	want := []byte{9, 0, 0, 0, 0x12, 8, 7, 6, 5, 4, 3, 2, 1}
	if !bytes.Equal(got, want) {
		t.Fatalf("COMMIT % x", got)
	}
	got = (&GoAway{Code: GoAwayProtocol, Reason: "ab"}).AppendFrame(nil)
	want = []byte{7, 0, 0, 0, 0x04, 10, 0, 2, 0, 'a', 'b'}
	if !bytes.Equal(got, want) {
		t.Fatalf("GOAWAY % x", got)
	}
	sizes := map[FrameType]int{FrameServerHello: 45, FrameFetch: 40, FrameBatch: BatchHeaderLen, FramePing: 8, FramePong: 8}
	for _, f := range []Frame{&ServerHello{}, &Fetch{}, &Batch{}, &Ping{}, &Pong{}} {
		if n := len(f.AppendFrame(nil)) - FrameHeaderLen; n != sizes[f.Type()] {
			t.Errorf("%s body %d bytes, want %d", f.Type(), n, sizes[f.Type()])
		}
	}
	// Fixed parts: HELLO 111 bytes + str8 + str8 + str16 + str8 + str8 + str8,
	// HELLO_OK 111 + str8 + str16 + str8 + str8 + str8.
	if n := len((&Hello{}).AppendFrame(nil)) - FrameHeaderLen; n != 111+1+1+2+1+1+1 {
		t.Errorf("empty HELLO body %d", n)
	}
	if n := len((&HelloOK{}).AppendFrame(nil)) - FrameHeaderLen; n != 111+1+2+1+1+1 {
		t.Errorf("empty HELLO_OK body %d", n)
	}
	h := (&Hello{OASystem: "Sys", BrokerType: "EDGE", BrokerVersion: "1.2"}).AppendFrame(nil)
	if tail := h[len(h)-13:]; !bytes.Equal(tail, []byte{3, 'S', 'y', 's', 4, 'E', 'D', 'G', 'E', 3, '1', '.', '2'}) {
		t.Errorf("HELLO must end with oaSystem, brokerType, brokerVersion str8, got % x", tail)
	}
	ok := (&HelloOK{SourceNodeID: "a", TopicRoot: "wr", OASystem: "S", BrokerType: "FULL", BrokerVersion: "9"}).AppendFrame(nil)
	if tail := ok[len(ok)-15:]; !bytes.Equal(tail, []byte{1, 'a', 2, 0, 'w', 'r', 1, 'S', 4, 'F', 'U', 'L', 'L', 1, '9'}) {
		t.Errorf("HELLO_OK tail % x", tail)
	}
}

func TestStringTruncation(t *testing.T) {
	long := strings.Repeat("é", 200) // 400 bytes, 2 per rune
	m := &Hello{ConsumerNodeID: long, OASystem: "x" + long}
	var got Hello
	enc := m.AppendFrame(nil)
	if err := got.Decode(enc[FrameHeaderLen:]); err != nil {
		t.Fatal(err)
	}
	if len(got.ConsumerNodeID) != 254 || !strings.HasPrefix(long, got.ConsumerNodeID) {
		t.Fatalf("str8 truncation: %d bytes", len(got.ConsumerNodeID))
	}
	if len(got.OASystem) != 255 {
		t.Fatalf("str8 truncation at a rune boundary: %d bytes", len(got.OASystem))
	}
	g := &GoAway{Code: 1, Reason: strings.Repeat("r", 70000)}
	var gd GoAway
	enc = g.AppendFrame(nil)
	if err := gd.Decode(enc[FrameHeaderLen:]); err != nil || len(gd.Reason) != 0xffff {
		t.Fatalf("str16 truncation: %d %v", len(gd.Reason), err)
	}
}

func TestDecodeFrameUnknown(t *testing.T) {
	if _, err := DecodeFrame(0x7e, []byte{1, 2, 3}); !errors.Is(err, ErrUnknownFrame) {
		t.Fatalf("unknown type: %v", err)
	}
	if FrameType(0x7e).Known() || !FramePong.Known() || FrameType(0x7e).String() != "UNKNOWN" {
		t.Fatal("Known/String")
	}
	// A reader skips an unknown frame and continues with the next one.
	var stream []byte
	stream = append(stream, 6, 0, 0, 0, 0x7e, 1, 2, 3, 4, 5)
	stream = (&Ping{Token: 9}).AppendFrame(stream)
	fr := NewFrameReader(bytes.NewReader(stream), MaxPreAuthFrame)
	typ, n, err := fr.ReadHeader()
	if err != nil || typ != 0x7e || n != 5 {
		t.Fatalf("ReadHeader: %v %d %v", typ, n, err)
	}
	if err := fr.Discard(n); err != nil {
		t.Fatal(err)
	}
	typ, body, err := fr.ReadFrame()
	if err != nil || typ != FramePing {
		t.Fatalf("next frame: %v %v", typ, err)
	}
	var p Ping
	if err := p.Decode(body); err != nil || p.Token != 9 {
		t.Fatalf("ping %+v %v", p, err)
	}
	if _, _, err := fr.ReadFrame(); err != io.EOF {
		t.Fatalf("end of stream: %v", err)
	}
}

func TestFrameReaderLimits(t *testing.T) {
	fr := NewFrameReader(bytes.NewReader([]byte{0, 0, 0, 0, 1}), 100)
	if _, _, err := fr.ReadHeader(); !errors.Is(err, ErrFrameEmpty) {
		t.Fatalf("frameLen 0: %v", err)
	}
	big := binary.LittleEndian.AppendUint32(nil, MaxPreAuthFrame+1)
	fr = NewFrameReader(bytes.NewReader(append(big, 1)), MaxPreAuthFrame)
	if _, _, err := fr.ReadHeader(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("over cap: %v", err)
	}
	atCap := binary.LittleEndian.AppendUint32(nil, MaxPreAuthFrame)
	fr = NewFrameReader(bytes.NewReader(append(atCap, 0x13)), MaxPreAuthFrame)
	if _, n, err := fr.ReadHeader(); err != nil || n != MaxPreAuthFrame-1 {
		t.Fatalf("at cap: %d %v", n, err)
	}
	if _, err := fr.Body(MaxPreAuthFrame - 1); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated body: %v", err)
	}
	fr = NewFrameReader(bytes.NewReader([]byte{5, 0}), 100)
	if _, _, err := fr.ReadHeader(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated header: %v", err)
	}
	fr = NewFrameReader(bytes.NewReader(nil), 100)
	if _, _, err := fr.ReadHeader(); err != io.EOF {
		t.Fatalf("clean EOF: %v", err)
	}
}

func TestFrameReaderProgress(t *testing.T) {
	recs := bytes.Repeat([]byte{1}, 200<<10)
	b := &Batch{Header: BatchHeader{RecordsBytes: uint32(len(recs))}, Records: recs}
	enc := b.AppendFrame(nil)
	fr := NewFrameReader(bytes.NewReader(enc), DefaultMaxFrameBytes)
	calls := 0
	fr.Progress = func() { calls++ }
	typ, n, err := fr.ReadHeader()
	if err != nil || typ != FrameBatch {
		t.Fatal(typ, err)
	}
	body := make([]byte, n)
	if err := fr.ReadBody(body); err != nil {
		t.Fatal(err)
	}
	if want := (n + readChunk - 1) / readChunk; calls != want {
		t.Fatalf("Progress called %d times, want %d", calls, want)
	}
	var got Batch
	if err := got.Decode(body); err != nil || !bytes.Equal(got.Records, recs) {
		t.Fatalf("decode: %v", err)
	}
}

func TestPreamble(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePreamble(&buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), []byte{'M', 'M', 'Q', 'P', 1, 0, 0, 0}) {
		t.Fatalf("preamble % x", buf.Bytes())
	}
	major, minor, err := ReadPreamble(&buf)
	if err != nil || major != VersionMajor || minor != VersionMinor {
		t.Fatalf("ReadPreamble %d.%d %v", major, minor, err)
	}
	major, minor, err = ParsePreamble(AppendPreambleVersion(nil, 2, 7))
	if err != nil || major != 2 || minor != 7 {
		t.Fatalf("v2.7: %d.%d %v", major, minor, err)
	}
	if _, _, err := ParsePreamble([]byte("MMQX\x01\x00\x00\x00")); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("bad magic: %v", err)
	}
	if _, _, err := ReadPreamble(bytes.NewReader([]byte("MMQP\x01"))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short: %v", err)
	}
}

func buildBatch(t testing.TB, h BatchHeader, recs ...[]byte) []byte {
	t.Helper()
	h.Count = uint32(len(recs))
	h.RecordsBytes = 0
	for _, r := range recs {
		h.RecordsBytes += uint32(len(r))
	}
	var prefix [BatchPrefixLen]byte
	EncodeBatchPrefix(&prefix, &h)
	if h.Flags&BatchFlagCRC != 0 {
		SetBatchCRC(&prefix, nil, recs)
	}
	return bytes.Join(append([][]byte{prefix[:]}, recs...), nil)
}

func TestBatchPrefixAndCRC(t *testing.T) {
	r1 := encode(t, &Record{Topic: "a/1", Payload: []byte("one"), CaptureMonoMs: 5})
	r2 := encode(t, &Record{Topic: "a/2", Payload: []byte("two"), CaptureMonoMs: 6})
	h := BatchHeader{FetchID: 3, Flags: BatchFlagCRC, BaseOffset: 11, LogStart: 1, Leo: 13, SourceMonoMs: 6, SourceWallMs: 99}
	frame := buildBatch(t, h, r1, r2)

	fr := NewFrameReader(bytes.NewReader(frame), DefaultMaxFrameBytes)
	typ, body, err := fr.ReadFrame()
	if err != nil || typ != FrameBatch {
		t.Fatal(typ, err)
	}
	var b Batch
	if err := b.Decode(body); err != nil {
		t.Fatal(err)
	}
	if b.Header.Count != 2 || b.Header.BaseOffset != 11 || int(b.Header.RecordsBytes) != len(r1)+len(r2) {
		t.Fatalf("header %+v", b.Header)
	}
	if !b.CRCValid() {
		t.Fatal("CRC mismatch on an intact batch")
	}
	// The contiguous encoder produces the same bytes as prefix + records.
	if enc := b.AppendFrame(nil); !bytes.Equal(enc, frame) {
		t.Fatal("Batch.AppendFrame differs from EncodeBatchPrefix + records")
	}
	it := b.Iter()
	var v RecordView
	for i, want := range []string{"a/1", "a/2"} {
		if ok, err := it.Next(&v); !ok || err != nil || string(v.Topic) != want {
			t.Fatalf("record %d: %v %v %q", i, ok, err, v.Topic)
		}
	}
	if ok, err := it.Next(&v); ok || err != nil {
		t.Fatalf("end: %v %v", ok, err)
	}

	for _, off := range []int{FrameHeaderLen + 8, FrameHeaderLen + 4, BatchPrefixLen + 50, len(frame) - 1} {
		bad := append([]byte(nil), frame...)
		bad[off] ^= 0x40
		var bb Batch
		if err := bb.Decode(bad[FrameHeaderLen:]); err != nil {
			continue // e.g. the flip set BatchFlagSparse: rejected before the CRC check
		}
		if bb.CRCValid() {
			t.Fatalf("flipped byte %d not detected", off)
		}
	}
	// Without the CRC flag the field is not checked.
	nocrc := buildBatch(t, BatchHeader{CRC32C: 1}, r1)
	var nb Batch
	if err := nb.Decode(nocrc[FrameHeaderLen:]); err != nil || !nb.CRCValid() {
		t.Fatalf("no-CRC batch: %v", err)
	}
	if nb.Header.CRC32C != 1 {
		t.Fatal("CRC field not carried verbatim")
	}
}

func TestBatchStructure(t *testing.T) {
	r1 := encode(t, &Record{Topic: "a"})
	frame := buildBatch(t, BatchHeader{}, r1)
	body := frame[FrameHeaderLen:]

	// recordsBytes larger than the body is a framing error.
	bad := append([]byte(nil), body...)
	binary.LittleEndian.PutUint32(bad[20:], uint32(len(r1)+1))
	var b Batch
	if err := b.Decode(bad); !errors.Is(err, ErrBatchRecords) {
		t.Fatalf("overlong recordsBytes: %v", err)
	}
	// Bytes after the records region are ignored (appended fields of a later minor version).
	ext := append(append([]byte(nil), body...), 1, 2, 3)
	if err := b.Decode(ext); err != nil || !bytes.Equal(b.Records, r1) {
		t.Fatalf("trailing bytes: %v", err)
	}
	it0 := b.Iter()
	if ok, err := it0.Next(&RecordView{}); !ok || err != nil {
		t.Fatalf("record after trailing bytes: %v %v", ok, err)
	}
	// A record overrunning recordsBytes is batch-structural.
	short := append([]byte(nil), body...)
	binary.LittleEndian.PutUint32(short[20:], uint32(len(r1)-1))
	if err := b.Decode(short); err != nil {
		t.Fatal(err)
	}
	it := b.Iter()
	if ok, err := it.Next(&RecordView{}); ok || !errors.Is(err, ErrRecordOverrun) || it.Remaining() != 1 {
		t.Fatalf("overrun: %v %v %d", ok, err, it.Remaining())
	}
}

func TestEncodeBatchPrefixNoAlloc(t *testing.T) {
	recs := [][]byte{encode(t, &Record{Topic: "a"}), encode(t, &Record{Topic: "b"})}
	var prefix [BatchPrefixLen]byte
	h := BatchHeader{Flags: BatchFlagCRC, Count: 2, RecordsBytes: uint32(len(recs[0]) + len(recs[1]))}
	allocs := testing.AllocsPerRun(100, func() {
		EncodeBatchPrefix(&prefix, &h)
		SetBatchCRC(&prefix, nil, recs)
	})
	if allocs != 0 {
		t.Fatalf("EncodeBatchPrefix + SetBatchCRC allocate %.1f per run", allocs)
	}
	var b Batch
	if err := b.Decode(append(prefix[FrameHeaderLen:], bytes.Join(recs, nil)...)); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = b.CRCValid() }); allocs != 0 {
		t.Fatalf("CRCValid on a decoded batch allocates %.1f per run", allocs)
	}
	built := Batch{Header: b.Header, Records: b.Records}
	if !b.CRCValid() || !built.CRCValid() || built.ComputeCRC() != b.Header.CRC32C {
		t.Fatal("CRC of a decoded and of a constructed batch differ")
	}
}

func TestGoAwayCodes(t *testing.T) {
	names := map[GoAwayCode]string{
		1: "version", 2: "unknown_peer", 3: "not_allowed", 4: "auth_failed", 5: "identity_mismatch",
		6: "self_connection", 7: "wrong_node", 8: "superseded", 9: "shutdown", 10: "protocol",
		11: "offset_out_of_range", 12: "busy", 13: "duplicate_node", 99: "unknown",
	}
	for c, n := range names {
		if c.String() != n {
			t.Errorf("%d: %s, want %s", c, c.String(), n)
		}
	}
	config := []GoAwayCode{GoAwayUnknownPeer, GoAwayNotAllowed, GoAwayAuthFailed, GoAwayIdentityMismatch,
		GoAwaySelfConnection, GoAwayWrongNode, GoAwayVersion, GoAwayDuplicateNode}
	for _, c := range config {
		if !c.ConfigError() {
			t.Errorf("%s should be a config error", c)
		}
	}
	for _, c := range []GoAwayCode{GoAwaySuperseded, GoAwayShutdown, GoAwayProtocol, GoAwayOffsetOutOfRange, GoAwayBusy} {
		if c.ConfigError() {
			t.Errorf("%s should not be a config error", c)
		}
	}
	if RetainedWinCCOA.String() != "WINCCOA" || RetainedClass(9).String() != "UNKNOWN" {
		t.Error("RetainedClass names")
	}
}

func TestMAC(t *testing.T) {
	if got := AppendLPString(nil, "ab"); !bytes.Equal(got, []byte{2, 0, 'a', 'b'}) {
		t.Fatalf("lp % x", got)
	}
	if got := AppendLP([]byte{9}, nil); !bytes.Equal(got, []byte{9, 0, 0}) {
		t.Fatalf("lp empty % x", got)
	}
	if got := AppendLP(nil, make([]byte, 70000)); len(got) != 2+0xffff || got[0] != 0xff || got[1] != 0xff {
		t.Fatalf("lp cap: %d", len(got))
	}
	ns, nc := nonce(10), nonce(20)
	exporter := bytes.Repeat([]byte{7}, ExporterLen)
	in := ConsumerMACInput(&ns, &nc, "oa-b", "oa-a", exporter)

	var want []byte
	for _, part := range [][]byte{[]byte("mmq-peer/1 C"), ns[:], nc[:], []byte("oa-b"), []byte("oa-a"), exporter} {
		want = binary.LittleEndian.AppendUint16(want, uint16(len(part)))
		want = append(want, part...)
	}
	if !bytes.Equal(in, want) {
		t.Fatalf("consumer MAC input\n got % x\nwant % x", in, want)
	}
	secret := []byte("s3cret")
	h := hmac.New(sha256.New, secret)
	h.Write(want)
	mac := MAC(secret, in)
	if !bytes.Equal(mac[:], h.Sum(nil)) {
		t.Fatal("MAC is not HMAC-SHA256")
	}

	sin := SourceMACInput(&nc, &ns, "oa-a", "oa-b", exporter)
	want = want[:0]
	for _, part := range [][]byte{[]byte("mmq-peer/1 S"), nc[:], ns[:], []byte("oa-a"), []byte("oa-b"), exporter} {
		want = binary.LittleEndian.AppendUint16(want, uint16(len(part)))
		want = append(want, part...)
	}
	if !bytes.Equal(sin, want) {
		t.Fatalf("source MAC input\n got % x\nwant % x", sin, want)
	}
	if macS := MAC(secret, sin); macS == mac {
		t.Fatal("consumer and source MACs must differ")
	}

	secrets := [][]byte{[]byte("old"), secret, []byte("other")}
	if i := MatchMAC(secrets, in, &mac); i != 1 {
		t.Fatalf("MatchMAC = %d, want 1", i)
	}
	if i := MatchMAC([][]byte{[]byte("old")}, in, &mac); i != -1 {
		t.Fatalf("MatchMAC wrong secret = %d", i)
	}
	relayed := ConsumerMACInput(&ns, &nc, "oa-b", "oa-a", bytes.Repeat([]byte{8}, ExporterLen))
	if i := MatchMAC(secrets, relayed, &mac); i != -1 {
		t.Fatal("a different exporter must not verify")
	}
	if a, b := NewNonce(), NewNonce(); a == b || a == ([32]byte{}) {
		t.Fatal("NewNonce")
	}
}

// A count larger than the records region can delimit must fail Decode before any caller sizes
// per-record state by it (one 73-byte frame must not make a consumer allocate gigabytes).
func TestBatchCountRange(t *testing.T) {
	for _, tc := range []struct {
		count, bytes uint32
		ok           bool
	}{{0, 0, true}, {1, 4, true}, {2, 8, true}, {2, 7, false}, {1, 0, false}, {0xffffffff, 0, false}, {0xffffffff, 1 << 20, false}} {
		region := make([]byte, tc.bytes)
		frame := (&Batch{Header: BatchHeader{Count: tc.count, RecordsBytes: tc.bytes}, Records: region}).AppendFrame(nil)
		var b Batch
		err := b.Decode(frame[FrameHeaderLen:])
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrBatchCountRange)) {
			t.Errorf("count %d bytes %d: err %v", tc.count, tc.bytes, err)
		}
	}
}

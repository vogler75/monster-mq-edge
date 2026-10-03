package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"

	"monstermq.io/edge/internal/mqtt/packets"
)

func fullRecord() Record {
	return Record{
		Flags:           1 | FlagRetain | FlagDup | FlagInline | FlagPayloadFormat,
		PublishWallNs:   1_759_500_000_123_456_789,
		CaptureMonoMs:   123_456,
		ExpirySec:       60,
		PayloadFormat:   1,
		Topic:           "plant/line1/temp",
		ClientID:        "client-1",
		Username:        []byte("alice"),
		ContentType:     "application/json",
		ResponseTopic:   "reply/here",
		CorrelationData: []byte{1, 2, 3, 0},
		User: []packets.UserProperty{
			{Key: "k", Val: "v1"}, {Key: "k", Val: "v2"}, {Key: "a", Val: ""}, {Key: "", Val: "x"}, {Key: "k", Val: "v1"},
		},
		Payload: []byte(`{"v":1}`),
	}
}

func encode(t testing.TB, r *Record) []byte {
	t.Helper()
	n := RecordSize(r)
	if n == 0 {
		t.Fatalf("RecordSize = 0")
	}
	b := make([]byte, n)
	if w := EncodeRecord(b, r); w != n {
		t.Fatalf("EncodeRecord wrote %d, RecordSize %d", w, n)
	}
	return b
}

func decode(t testing.TB, b []byte) RecordView {
	t.Helper()
	var v RecordView
	if err := DecodeRecord(b, &v); err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	return v
}

// normalized drops the distinction between nil and empty slices so records compare structurally.
func normalized(r Record) Record {
	if len(r.Username) == 0 {
		r.Username = nil
	}
	if len(r.CorrelationData) == 0 {
		r.CorrelationData = nil
	}
	if len(r.User) == 0 {
		r.User = nil
	}
	if len(r.Payload) == 0 {
		r.Payload = nil
	}
	return r
}

func TestRecordRoundTrip(t *testing.T) {
	cases := map[string]Record{
		"full":    fullRecord(),
		"minimal": {Topic: "a"},
		"qos2 will": {
			Flags: 2 | FlagWill, Topic: "clients/c1/state", ClientID: "c1", Payload: []byte("offline"),
			PublishWallNs: -5, CaptureMonoMs: 0,
		},
		"retained delete": {Flags: FlagRetain, Topic: "a/b", ClientID: "inline"},
		"snapshot":        {Flags: FlagSnapshot | FlagRetain, Topic: "x", Payload: []byte{0}, ExpirySec: 1},
		"pf zero present": {Flags: FlagPayloadFormat, PayloadFormat: 0, Topic: "t"},
		"utf8":            {Topic: "werk/größe/ü€", ClientID: "ç", Username: []byte("ñ"), User: []packets.UserProperty{{Key: "ä", Val: "😀"}}},
		"only user props": {Topic: "t", User: []packets.UserProperty{{Key: "a", Val: "b"}}},
		"max strings": {
			Topic: strings.Repeat("t", MaxStringLen), ClientID: strings.Repeat("c", MaxStringLen),
			Username: bytes.Repeat([]byte("u"), MaxStringLen), ContentType: strings.Repeat("x", MaxStringLen),
			User: []packets.UserProperty{{Key: strings.Repeat("k", MaxStringLen), Val: strings.Repeat("v", MaxStringLen)}},
		},
		"reserved flag bits": {Flags: 1<<6 | 0xfc00 | 1, Topic: "t"},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			b := encode(t, &r)
			if got := RecordFrameLen(b); got != len(b) {
				t.Fatalf("RecordFrameLen = %d, want %d", got, len(b))
			}
			v := decode(t, b)
			if v.Version != RecordVersion || v.HdrLen != RecordHeaderLen || v.UnknownProps != 0 {
				t.Fatalf("header: version %d hdrLen %d unknown %d", v.Version, v.HdrLen, v.UnknownProps)
			}
			if !bytes.Equal(v.Frame, b) {
				t.Fatalf("Frame view differs")
			}
			if v.QoS() != byte(r.Flags&FlagQoSMask) || v.Retain() != (r.Flags&FlagRetain != 0) ||
				v.Dup() != (r.Flags&FlagDup != 0) || v.Will() != (r.Flags&FlagWill != 0) ||
				v.Inline() != (r.Flags&FlagInline != 0) || v.Snapshot() != (r.Flags&FlagSnapshot != 0) ||
				v.Skipped() || v.HasPayloadFormat() != (r.Flags&FlagPayloadFormat != 0) {
				t.Fatalf("flag accessors wrong for %#x", v.Flags)
			}
			got := v.Record()
			if !reflect.DeepEqual(normalized(got), normalized(r)) {
				t.Fatalf("round trip mismatch\n got %+v\nwant %+v", got, r)
			}
			if a := AppendRecord([]byte("pre"), &r); !bytes.Equal(a[3:], b) || string(a[:3]) != "pre" {
				t.Fatalf("AppendRecord differs from EncodeRecord")
			}
		})
	}
}

// TestRecordLayout pins the byte layout of plan section 10 with a hand-written encoding.
func TestRecordLayout(t *testing.T) {
	r := Record{
		Flags: 1 | FlagRetain | FlagPayloadFormat, PublishWallNs: 0x0102030405060708, CaptureMonoMs: 0x1112131415161718,
		ExpirySec: 0x21222324, PayloadFormat: 1, Topic: "ab", ClientID: "c", Username: []byte("u"),
		ContentType: "j", User: []packets.UserProperty{{Key: "k", Val: "vv"}}, Payload: []byte("P"),
	}
	want := []byte{
		61, 0, 0, 0, // recLen = 65 - 4
		1, 44, // recVersion, hdrLen
		0x85, 0x00, // flags: qos1 | retain | payloadFormat
		8, 7, 6, 5, 4, 3, 2, 1, // publishWallNs
		0x18, 0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11, // captureMonoMs
		0x24, 0x23, 0x22, 0x21, // expirySec
		1, 0, // payloadFormat, reserved
		2, 0, 1, 0, 1, 0, // topicLen, clientIdLen, usernameLen
		6 + 10, 0, 0, 0, // propsLen
		1, 0, 0, 0, // payloadLen
		'a', 'b', 'c', 'u',
		0x03, 1, 0, 0, 0, 'j', // ContentType TLV
		0x26, 5, 0, 0, 0, 1, 0, 'k', 'v', 'v', // UserProperty TLV
		'P',
	}
	got := encode(t, &r)
	if !bytes.Equal(got, want) {
		t.Fatalf("layout\n got % x\nwant % x", got, want)
	}
}

func TestRecordSizeLimits(t *testing.T) {
	long := strings.Repeat("x", MaxStringLen+1)
	bad := []Record{
		{Topic: long},
		{Topic: "t", ClientID: long},
		{Topic: "t", Username: []byte(long)},
		{Topic: "t", ContentType: long},
		{Topic: "t", ResponseTopic: long},
		{Topic: "t", CorrelationData: []byte(long)},
		{Topic: "t", User: []packets.UserProperty{{Key: long}}},
		{Topic: "t", User: []packets.UserProperty{{Val: long}}},
	}
	for i, r := range bad {
		if n := RecordSize(&r); n != 0 {
			t.Errorf("case %d: RecordSize = %d, want 0", i, n)
		}
		if got := AppendRecord(nil, &r); len(got) != 0 {
			t.Errorf("case %d: AppendRecord appended %d bytes", i, len(got))
		}
	}
	r := fullRecord()
	want := RecordHeaderLen + len(r.Topic) + len(r.ClientID) + len(r.Username) + len(r.Payload) +
		(5 + len(r.ContentType)) + (5 + len(r.ResponseTopic)) + (5 + len(r.CorrelationData))
	for _, u := range r.User {
		want += 5 + 2 + len(u.Key) + len(u.Val)
	}
	if n := RecordSize(&r); n != want {
		t.Fatalf("RecordSize = %d, want %d", n, want)
	}
}

func TestSetPacket(t *testing.T) {
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: 2, Retain: true, Dup: true},
		TopicName:   "a/b",
		Payload:     []byte("p"),
		Properties: packets.Properties{
			PayloadFormat: 1, PayloadFormatFlag: true, MessageExpiryInterval: 500,
			ContentType: "ct", ResponseTopic: "rt", CorrelationData: []byte("cd"),
			User: []packets.UserProperty{{Key: "k", Val: "v"}}, TopicAlias: 7, SubscriptionIdentifier: []int{3},
		},
	}
	r := Record{Flags: FlagWill | FlagInline | FlagRetain}
	r.SetPacket(&pk, 100)
	want := FlagWill | FlagInline | FlagRetain | FlagDup | FlagPayloadFormat | 2
	if r.Flags != want || r.ExpirySec != 100 || r.PayloadFormat != 1 || r.Topic != "a/b" ||
		r.ContentType != "ct" || r.ResponseTopic != "rt" || string(r.CorrelationData) != "cd" || len(r.User) != 1 {
		t.Fatalf("SetPacket: %+v", r)
	}
	pk.FixedHeader = packets.FixedHeader{Qos: 0}
	pk.Properties.PayloadFormatFlag = false
	r.SetPacket(&pk, 0)
	if r.Flags != FlagWill|FlagInline || r.ExpirySec != 500 || r.PayloadFormat != 0 {
		t.Fatalf("SetPacket reset: flags %#x expiry %d pf %d", r.Flags, r.ExpirySec, r.PayloadFormat)
	}
}

func TestRecordViewProperties(t *testing.T) {
	r := fullRecord()
	v := decode(t, encode(t, &r))
	var p packets.Properties
	v.Properties(&p)
	if p.MessageExpiryInterval != 60 || !p.PayloadFormatFlag || p.PayloadFormat != 1 ||
		p.ContentType != r.ContentType || p.ResponseTopic != r.ResponseTopic ||
		!bytes.Equal(p.CorrelationData, r.CorrelationData) || !reflect.DeepEqual(p.User, r.User) {
		t.Fatalf("Properties: %+v", p)
	}
	var p2 packets.Properties
	v2 := decode(t, encode(t, &Record{Topic: "t"}))
	v2.Properties(&p2)
	if p2.PayloadFormatFlag || p2.User != nil || p2.ContentType != "" || p2.CorrelationData != nil {
		t.Fatalf("Properties of a plain record: %+v", p2)
	}
}

func TestValidContent(t *testing.T) {
	ok := fullRecord()
	if !ok.ValidContent() {
		t.Fatal("full record rejected")
	}
	bad := []func(r *Record){
		func(r *Record) { r.Topic = "" },
		func(r *Record) { r.Topic = "a/+" },
		func(r *Record) { r.Topic = "a/#" },
		func(r *Record) { r.Topic = "a\x00b" },
		func(r *Record) { r.Topic = "a\xffb" },
		func(r *Record) { r.ClientID = "c\x00" },
		func(r *Record) { r.Username = []byte{0xc0, 0x80} },
		func(r *Record) { r.ContentType = "\xff" },
		func(r *Record) { r.ResponseTopic = "\x00" },
		func(r *Record) { r.User = []packets.UserProperty{{Key: "\xfe", Val: "v"}} },
		func(r *Record) { r.User = []packets.UserProperty{{Key: "k", Val: "v\x00"}} },
		func(r *Record) { r.CorrelationData = make([]byte, MaxStringLen+1) },
	}
	for i, mut := range bad {
		r := fullRecord()
		mut(&r)
		if r.ValidContent() {
			t.Errorf("case %d accepted", i)
		}
	}
	if !ValidTopic("$SYS/x") || ValidTopic(strings.Repeat("a", MaxStringLen+1)) || !ValidString("") {
		t.Fatal("ValidTopic/ValidString edge cases")
	}
}

type rawRec struct {
	version uint8
	hdrLen  int
	flags   uint16
	mono    uint64
	topic   []byte
	client  []byte
	user    []byte
	props   []byte
	payload []byte
	extra   []byte // bytes appended after the variable part and counted in recLen
}

// build assembles a record by hand, independent of EncodeRecord.
func (r rawRec) build() []byte {
	if r.version == 0 {
		r.version = RecordVersion
	}
	if r.hdrLen == 0 {
		r.hdrLen = RecordHeaderLen
	}
	b := make([]byte, r.hdrLen)
	le := binary.LittleEndian
	b[4] = r.version
	b[5] = byte(r.hdrLen)
	le.PutUint16(b[6:], r.flags)
	le.PutUint64(b[16:], r.mono)
	le.PutUint16(b[30:], uint16(len(r.topic)))
	le.PutUint16(b[32:], uint16(len(r.client)))
	le.PutUint16(b[34:], uint16(len(r.user)))
	le.PutUint32(b[36:], uint32(len(r.props)))
	le.PutUint32(b[40:], uint32(len(r.payload)))
	for i := RecordHeaderLen; i < r.hdrLen; i++ {
		b[i] = 0xee
	}
	b = append(b, r.topic...)
	b = append(b, r.client...)
	b = append(b, r.user...)
	b = append(b, r.props...)
	b = append(b, r.payload...)
	b = append(b, r.extra...)
	le.PutUint32(b, uint32(len(b)-4))
	return b
}

func tlv(id uint8, val []byte) []byte {
	b := []byte{id, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[1:], uint32(len(val)))
	return append(b, val...)
}

func userTLV(k, v string) []byte {
	val := binary.LittleEndian.AppendUint16(nil, uint16(len(k)))
	val = append(val, k...)
	return tlv(PropUserProperty, append(val, v...))
}

func TestDecodeForwardCompatible(t *testing.T) {
	props := bytes.Join([][]byte{
		tlv(PropContentType, []byte("ct")),
		tlv(0x7f, []byte("future value")),
		userTLV("k", "v"),
		tlv(0x01, nil), // MQTT id not carried in TLVs: unknown here
		tlv(PropCorrelationData, []byte{0xff, 0}),
	}, nil)
	raw := rawRec{hdrLen: 60, flags: 1 | 1<<6 | 0xfc00, topic: []byte("t/1"), client: []byte("c"), props: props, payload: []byte("p")}.build()
	v := decode(t, raw)
	if v.HdrLen != 60 || string(v.Topic) != "t/1" || string(v.ClientID) != "c" || string(v.Payload) != "p" {
		t.Fatalf("extended header decode: %+v", v)
	}
	if v.UnknownProps != 2 {
		t.Fatalf("UnknownProps = %d, want 2", v.UnknownProps)
	}
	if v.QoS() != 1 {
		t.Fatalf("QoS with reserved bits = %d", v.QoS())
	}
	var p packets.Properties
	v.Properties(&p)
	if p.ContentType != "ct" || len(p.User) != 1 || p.User[0] != (packets.UserProperty{Key: "k", Val: "v"}) ||
		!bytes.Equal(p.CorrelationData, []byte{0xff, 0}) {
		t.Fatalf("known props around unknown ones: %+v", p)
	}
	n, err := ValidateProps(props)
	if err != nil || n != 2 {
		t.Fatalf("ValidateProps = %d, %v", n, err)
	}
}

func TestDecodeRejects(t *testing.T) {
	valid := rawRec{topic: []byte("a/b"), client: []byte("c"), payload: []byte("x")}
	mut := func(f func(b []byte) []byte) []byte { return f(valid.build()) }
	le := binary.LittleEndian
	cases := map[string][]byte{
		"empty":            {},
		"recLen beyond":    mut(func(b []byte) []byte { le.PutUint32(b, uint32(len(b))); return b }),
		"short header":     {10, 0, 0, 0, 1, 44, 0, 0, 0, 0, 0, 0, 0, 0},
		"version 0":        mut(func(b []byte) []byte { b[4] = 0; return b }),
		"version 2":        mut(func(b []byte) []byte { b[4] = 2; return b }),
		"hdrLen 43":        mut(func(b []byte) []byte { b[5] = 43; return b }),
		"hdrLen too big":   mut(func(b []byte) []byte { b[5] = 255; return b }),
		"topicLen +1":      mut(func(b []byte) []byte { le.PutUint16(b[30:], 4); return b }),
		"payloadLen +1":    mut(func(b []byte) []byte { le.PutUint32(b[40:], 2); return b }),
		"propsLen huge":    mut(func(b []byte) []byte { le.PutUint32(b[36:], 0xffffffff); return b }),
		"invariant extra":  rawRec{topic: []byte("a"), extra: []byte{1}}.build(),
		"qos 3":            rawRec{flags: 3, topic: []byte("a")}.build(),
		"tombstone qos 3":  rawRec{flags: 3 | FlagSkipped}.build(),
		"empty topic":      rawRec{}.build(),
		"topic plus":       rawRec{topic: []byte("a/+/b")}.build(),
		"topic hash":       rawRec{topic: []byte("a/#")}.build(),
		"topic nul":        rawRec{topic: []byte("a\x00")}.build(),
		"topic bad utf8":   rawRec{topic: []byte("a\xff")}.build(),
		"topic surrogate":  rawRec{topic: []byte("\xed\xa0\x80")}.build(),
		"client bad utf8":  rawRec{topic: []byte("a"), client: []byte("\xc3")}.build(),
		"client nul":       rawRec{topic: []byte("a"), client: []byte("\x00")}.build(),
		"username nul":     rawRec{topic: []byte("a"), user: []byte("u\x00")}.build(),
		"tlv header short": rawRec{topic: []byte("a"), props: []byte{PropContentType, 1, 0}}.build(),
		"tlv value short":  rawRec{topic: []byte("a"), props: []byte{PropContentType, 5, 0, 0, 0, 'x'}}.build(),
		"tlv unknown short": rawRec{topic: []byte("a"), props: append(tlv(0x55, []byte("ok")),
			0x56, 9, 0, 0, 0)}.build(),
		"content type utf8":   rawRec{topic: []byte("a"), props: tlv(PropContentType, []byte{0xff})}.build(),
		"response topic nul":  rawRec{topic: []byte("a"), props: tlv(PropResponseTopic, []byte{'a', 0})}.build(),
		"content type long":   rawRec{topic: []byte("a"), props: tlv(PropContentType, make([]byte, MaxStringLen+1))}.build(),
		"correlation long":    rawRec{topic: []byte("a"), props: tlv(PropCorrelationData, make([]byte, MaxStringLen+1))}.build(),
		"user short":          rawRec{topic: []byte("a"), props: tlv(PropUserProperty, []byte{5})}.build(),
		"user keyLen overrun": rawRec{topic: []byte("a"), props: tlv(PropUserProperty, []byte{9, 0, 'k'})}.build(),
		"user key utf8":       rawRec{topic: []byte("a"), props: userTLV("\xff", "v")}.build(),
		"user val nul":        rawRec{topic: []byte("a"), props: userTLV("k", "\x00")}.build(),
		"user val long":       rawRec{topic: []byte("a"), props: userTLV("k", strings.Repeat("v", MaxStringLen+1))}.build(),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			var v RecordView
			err := DecodeRecord(b, &v)
			if err == nil {
				t.Fatalf("accepted")
			}
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("error %v does not match ErrMalformed", err)
			}
		})
	}
	if v := decode(t, valid.build()); string(v.Topic) != "a/b" {
		t.Fatal("valid base record rejected")
	}
}

func TestDecodeIgnoresBytesAfterRecord(t *testing.T) {
	r := Record{Topic: "t", Payload: []byte("p")}
	b := append(encode(t, &r), 0xde, 0xad)
	v := decode(t, b)
	if len(v.Frame) != len(b)-2 || string(v.Payload) != "p" {
		t.Fatalf("frame %d payload %q", len(v.Frame), v.Payload)
	}
}

func TestTombstone(t *testing.T) {
	r := fullRecord()
	r.Flags |= FlagWill
	orig := encode(t, &r)
	ts := AppendTombstone(nil, orig)
	if len(ts) != TombstoneLen || RecordFrameLen(ts) != TombstoneLen {
		t.Fatalf("tombstone length %d", len(ts))
	}
	v := decode(t, ts)
	if !v.Skipped() || v.Flags != r.Flags|FlagSkipped || v.PublishWallNs != r.PublishWallNs ||
		v.CaptureMonoMs != r.CaptureMonoMs || v.ExpirySec != r.ExpirySec || v.PayloadFormat != r.PayloadFormat {
		t.Fatalf("tombstone header: %+v", v)
	}
	if len(v.Topic)+len(v.ClientID)+len(v.Username)+len(v.Props)+len(v.Payload) != 0 {
		t.Fatalf("tombstone has a variable part")
	}
	short := AppendTombstone([]byte{9}, []byte{1, 2})
	if v := decode(t, short[1:]); v.Flags != FlagSkipped || v.PublishWallNs != 0 {
		t.Fatalf("tombstone of a short original: %+v", v)
	}
	// A tombstone with an extended header and stray lengths still decodes as a tombstone.
	odd := rawRec{hdrLen: 48, flags: FlagSkipped | FlagRetain, topic: []byte("a\x00+")}.build()
	if v := decode(t, odd); !v.Skipped() || !v.Retain() {
		t.Fatalf("lenient tombstone: %+v", v)
	}
}

func recordsRegion(t testing.TB, recs ...[]byte) []byte {
	t.Helper()
	return bytes.Join(recs, nil)
}

func TestRecordIter(t *testing.T) {
	a := encode(t, &Record{Topic: "a", CaptureMonoMs: 10})
	b := rawRec{topic: []byte("bad+"), mono: 5}.build()
	c := encode(t, &Record{Topic: "c", CaptureMonoMs: 20})
	future := encode(t, &Record{Topic: "f", CaptureMonoMs: 21})

	type step struct {
		ok    bool
		err   error
		topic string
	}
	run := func(it *RecordIter) []step {
		var out []step
		for i := 0; i < 10; i++ {
			var v RecordView
			ok, err := it.Next(&v)
			out = append(out, step{ok, err, string(v.Topic)})
			if !ok {
				break
			}
		}
		return out
	}
	check := func(name string, got []step, want []step) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d steps, want %d: %+v", name, len(got), len(want), got)
		}
		for i := range want {
			g, w := got[i], want[i]
			if g.ok != w.ok || g.topic != w.topic || (w.err == nil) != (g.err == nil) ||
				(w.err != nil && !errors.Is(g.err, w.err)) {
				t.Fatalf("%s step %d: got %+v want %+v", name, i, g, w)
			}
		}
	}

	it := NewRecordIter(recordsRegion(t, a, b, c), 3)
	it.CheckMono(20)
	check("malformed middle", run(&it), []step{
		{true, nil, "a"}, {true, ErrMalformed, "bad+"}, {true, nil, "c"}, {false, nil, ""},
	})

	it = NewRecordIter(recordsRegion(t, a, future), 2)
	it.CheckMono(20)
	check("mono ahead", run(&it), []step{{true, nil, "a"}, {true, ErrMalformed, "f"}, {false, nil, ""}})

	it = NewRecordIter(recordsRegion(t, a, future), 2)
	check("mono unchecked", run(&it), []step{{true, nil, "a"}, {true, nil, "f"}, {false, nil, ""}})

	region := recordsRegion(t, a, c[:len(c)-1])
	it = NewRecordIter(region, 3)
	got := run(&it)
	check("overrun", got, []step{{true, nil, "a"}, {false, ErrRecordOverrun, ""}})
	if it.Remaining() != 2 {
		t.Fatalf("Remaining after overrun = %d, want 2", it.Remaining())
	}
	if ok, err := it.Next(&RecordView{}); ok || !errors.Is(err, ErrRecordOverrun) {
		t.Fatalf("Next after failure: %v %v", ok, err)
	}

	it = NewRecordIter(append(recordsRegion(t, a), 1, 0), 2)
	check("partial recLen", run(&it), []step{{true, nil, "a"}, {false, ErrRecordOverrun, ""}})

	it = NewRecordIter(recordsRegion(t, a, c), 3)
	check("count too high", run(&it), []step{{true, nil, "a"}, {true, nil, "c"}, {false, ErrBatchCount, ""}})
	if it.Remaining() != 1 {
		t.Fatalf("Remaining = %d, want 1", it.Remaining())
	}

	it = NewRecordIter(recordsRegion(t, a, c), 1)
	check("count too low", run(&it), []step{{true, nil, "a"}, {false, ErrBatchCount, ""}})
	if it.Remaining() != 0 {
		t.Fatalf("Remaining = %d, want 0", it.Remaining())
	}

	it = NewRecordIter(nil, 0)
	check("empty", run(&it), []step{{false, nil, ""}})
}

func TestPropIterTruncated(t *testing.T) {
	it := NewPropIter(append(tlv(PropContentType, []byte("a")), 0x03, 9))
	if id, val, ok := it.Next(); !ok || id != PropContentType || string(val) != "a" {
		t.Fatalf("first TLV: %d %q %v", id, val, ok)
	}
	if _, _, ok := it.Next(); ok {
		t.Fatal("truncated TLV returned")
	}
	if _, _, ok := it.Next(); ok {
		t.Fatal("iterator not exhausted")
	}
	if k, v, ok := SplitUserProperty([]byte{1, 0, 'k', 'v'}); !ok || string(k) != "k" || string(v) != "v" {
		t.Fatal("SplitUserProperty")
	}
}

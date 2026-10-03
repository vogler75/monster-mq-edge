package wire

import (
	"encoding/binary"
	"errors"
	"math"
	"unicode/utf8"

	"monstermq.io/edge/internal/mqtt/packets"
)

// Record layout (plan 10). All integers are little-endian.
//
//	off size field
//	  0   4  recLen          u32  bytes after this field
//	  4   1  recVersion      u8   1
//	  5   1  hdrLen          u8   44 in v1.0; decoders accept >= 44 and skip extra
//	  6   2  flags           u16
//	  8   8  publishWallNs   i64
//	 16   8  captureMonoMs   u64
//	 24   4  expirySec       u32
//	 28   1  payloadFormat   u8   valid if FlagPayloadFormat
//	 29   1  reserved        u8
//	 30   2  topicLen        u16
//	 32   2  clientIdLen     u16
//	 34   2  usernameLen     u16
//	 36   4  propsLen        u32
//	 40   4  payloadLen      u32
//	 44   …  topic | clientId | username | props | payload
const (
	RecordVersion   = 1
	RecordHeaderLen = 44
	// TombstoneLen is the size of an oversize-record tombstone: a header without a variable part.
	TombstoneLen = RecordHeaderLen
	// TLVHeaderLen is the size of a property TLV header (id u8, len u32).
	TLVHeaderLen = 5
	// MaxStringLen is the largest topic, client id, username or property string an MQTT packet can carry.
	MaxStringLen = math.MaxUint16

	offRecLen        = 0
	offVersion       = 4
	offHdrLen        = 5
	offFlags         = 6
	offWallNs        = 8
	offMonoMs        = 16
	offExpiry        = 24
	offPayloadFormat = 28
	offTopicLen      = 30
	offClientIDLen   = 32
	offUsernameLen   = 34
	offPropsLen      = 36
	offPayloadLen    = 40
)

// Record flags. b6 and b10-b15 are reserved and ignored by decoders.
const (
	FlagQoSMask       uint16 = 0x0003
	FlagRetain        uint16 = 1 << 2
	FlagDup           uint16 = 1 << 3
	FlagWill          uint16 = 1 << 4
	FlagInline        uint16 = 1 << 5
	FlagPayloadFormat uint16 = 1 << 7
	FlagSnapshot      uint16 = 1 << 8
	FlagSkipped       uint16 = 1 << 9
)

// Property TLV ids. The values are the MQTT 5 property identifiers.
const (
	PropContentType     uint8 = 0x03
	PropResponseTopic   uint8 = 0x08
	PropCorrelationData uint8 = 0x09
	PropUserProperty    uint8 = 0x26
)

// ErrMalformed matches (errors.Is) every per-record content error returned by DecodeRecord and RecordIter.
// Such a record counts as dropped{malformed}; the batch continues (plan 9.7).
var ErrMalformed = errors.New("peerlink/wire: malformed record")

// MalformedError is a per-record content error.
type MalformedError struct{ What string }

func (e *MalformedError) Error() string        { return "peerlink/wire: malformed record: " + e.What }
func (e *MalformedError) Is(target error) bool { return target == ErrMalformed }

var (
	errRecordTruncated = &MalformedError{"recLen beyond buffer"}
	errHeaderShort     = &MalformedError{"header shorter than 44 bytes"}
	errVersion         = &MalformedError{"unknown recVersion"}
	errHdrLen          = &MalformedError{"hdrLen out of range"}
	errInvariant       = &MalformedError{"recLen invariant broken"}
	errQoS             = &MalformedError{"qos > 2"}
	errTopic           = &MalformedError{"invalid topic"}
	errClientID        = &MalformedError{"invalid client id"}
	errUsername        = &MalformedError{"invalid username"}
	errTLV             = &MalformedError{"truncated property TLV"}
	errPropString      = &MalformedError{"invalid property string"}
	errPropLen         = &MalformedError{"property value too long"}
	errUserProp        = &MalformedError{"malformed user property"}
	errMonoAhead       = &MalformedError{"captureMonoMs after batch sourceMonoMs"}
)

// Batch-structural errors from RecordIter: the rest of the batch cannot be delimited (plan 9.7).
var (
	ErrRecordOverrun = errors.New("peerlink/wire: record overruns the batch records region")
	ErrBatchCount    = errors.New("peerlink/wire: record count does not match the batch count")
)

// Record is the input of the encoder. String fields are referenced, not copied, until EncodeRecord returns.
// ContentType, ResponseTopic and CorrelationData are carried only when non-empty, as in an MQTT PUBLISH.
type Record struct {
	Flags           uint16
	PublishWallNs   int64
	CaptureMonoMs   uint64
	ExpirySec       uint32
	PayloadFormat   uint8
	Topic           string
	ClientID        string
	Username        []byte
	ContentType     string
	ResponseTopic   string
	CorrelationData []byte
	User            []packets.UserProperty
	Payload         []byte
}

// SetPacket fills r from the publish pk, referencing its strings and slices: QoS, retain, dup, payload
// format, topic, payload, properties and the message expiry, capped by maxExpirySec (0 = no cap). Other
// flags already in r.Flags (FlagWill, FlagInline, FlagSnapshot) are kept. TopicAlias and
// SubscriptionIdentifier are never carried. ClientID, Username and the two times are set by the caller.
func (r *Record) SetPacket(pk *packets.Packet, maxExpirySec uint32) {
	f := r.Flags &^ (FlagQoSMask | FlagRetain | FlagDup | FlagPayloadFormat)
	f |= uint16(pk.FixedHeader.Qos) & FlagQoSMask
	if pk.FixedHeader.Retain {
		f |= FlagRetain
	}
	if pk.FixedHeader.Dup {
		f |= FlagDup
	}
	p := &pk.Properties
	r.PayloadFormat = 0
	if p.PayloadFormatFlag {
		f |= FlagPayloadFormat
		r.PayloadFormat = p.PayloadFormat
	}
	r.Flags = f
	r.ExpirySec = p.MessageExpiryInterval
	if maxExpirySec > 0 && r.ExpirySec > maxExpirySec {
		r.ExpirySec = maxExpirySec
	}
	r.Topic = pk.TopicName
	r.Payload = pk.Payload
	r.ContentType = p.ContentType
	r.ResponseTopic = p.ResponseTopic
	r.CorrelationData = p.CorrelationData
	r.User = p.User
}

// ValidContent reports whether every string of r passes the receiver's structural rules (plan 7.3, 12.2):
// a valid topic name, and valid UTF-8 without NUL in every other string. Capture runs it for inline
// publishes; network publishes were already validated by the MQTT decoder.
func (r *Record) ValidContent() bool {
	if !ValidTopic(r.Topic) || !ValidString(r.ClientID) || !validStringBytes(r.Username) {
		return false
	}
	if !ValidString(r.ContentType) || !ValidString(r.ResponseTopic) || len(r.CorrelationData) > MaxStringLen {
		return false
	}
	for i := range r.User {
		if !ValidString(r.User[i].Key) || !ValidString(r.User[i].Val) {
			return false
		}
	}
	return true
}

func propsSize(r *Record) (uint64, bool) {
	var n uint64
	if l := len(r.ContentType); l > 0 {
		if l > MaxStringLen {
			return 0, false
		}
		n += TLVHeaderLen + uint64(l)
	}
	if l := len(r.ResponseTopic); l > 0 {
		if l > MaxStringLen {
			return 0, false
		}
		n += TLVHeaderLen + uint64(l)
	}
	if l := len(r.CorrelationData); l > 0 {
		if l > MaxStringLen {
			return 0, false
		}
		n += TLVHeaderLen + uint64(l)
	}
	for i := range r.User {
		k, v := len(r.User[i].Key), len(r.User[i].Val)
		if k > MaxStringLen || v > MaxStringLen {
			return 0, false
		}
		n += TLVHeaderLen + 2 + uint64(k) + uint64(v)
	}
	return n, n <= math.MaxUint32
}

// RecordSize returns the exact encoded size of r, or 0 when a field does not fit its length field
// (a string above 65535 bytes, or a record above 4 GiB). Capture counts such a record as invalid.
func RecordSize(r *Record) int {
	if len(r.Topic) > MaxStringLen || len(r.ClientID) > MaxStringLen || len(r.Username) > MaxStringLen {
		return 0
	}
	props, ok := propsSize(r)
	if !ok {
		return 0
	}
	total := uint64(RecordHeaderLen) + uint64(len(r.Topic)) + uint64(len(r.ClientID)) +
		uint64(len(r.Username)) + props + uint64(len(r.Payload))
	if total-4 > math.MaxUint32 || total > math.MaxInt {
		return 0
	}
	return int(total)
}

// EncodeRecord writes r into dst and returns the number of bytes written. dst must hold at least
// RecordSize(r) bytes and RecordSize(r) must be non-zero; under that precondition it cannot fail.
func EncodeRecord(dst []byte, r *Record) int {
	props, _ := propsSize(r)
	tl, cl, ul, pl := len(r.Topic), len(r.ClientID), len(r.Username), len(r.Payload)
	size := RecordHeaderLen + tl + cl + ul + int(props) + pl
	b := dst[:size]
	le := binary.LittleEndian
	le.PutUint32(b[offRecLen:], uint32(size-4))
	b[offVersion] = RecordVersion
	b[offHdrLen] = RecordHeaderLen
	le.PutUint16(b[offFlags:], r.Flags)
	le.PutUint64(b[offWallNs:], uint64(r.PublishWallNs))
	le.PutUint64(b[offMonoMs:], r.CaptureMonoMs)
	le.PutUint32(b[offExpiry:], r.ExpirySec)
	b[offPayloadFormat] = r.PayloadFormat
	b[offPayloadFormat+1] = 0
	le.PutUint16(b[offTopicLen:], uint16(tl))
	le.PutUint16(b[offClientIDLen:], uint16(cl))
	le.PutUint16(b[offUsernameLen:], uint16(ul))
	le.PutUint32(b[offPropsLen:], uint32(props))
	le.PutUint32(b[offPayloadLen:], uint32(pl))
	o := RecordHeaderLen
	o += copy(b[o:], r.Topic)
	o += copy(b[o:], r.ClientID)
	o += copy(b[o:], r.Username)
	if props > 0 {
		o = putTLV(b, o, PropContentType, r.ContentType)
		o = putTLV(b, o, PropResponseTopic, r.ResponseTopic)
		o = putTLV(b, o, PropCorrelationData, r.CorrelationData)
		for i := range r.User {
			k, v := r.User[i].Key, r.User[i].Val
			b[o] = PropUserProperty
			le.PutUint32(b[o+1:], uint32(2+len(k)+len(v)))
			le.PutUint16(b[o+5:], uint16(len(k)))
			o += TLVHeaderLen + 2
			o += copy(b[o:], k)
			o += copy(b[o:], v)
		}
	}
	o += copy(b[o:], r.Payload)
	return o
}

func putTLV[T string | []byte](b []byte, o int, id uint8, v T) int {
	if len(v) == 0 {
		return o
	}
	b[o] = id
	binary.LittleEndian.PutUint32(b[o+1:], uint32(len(v)))
	o += TLVHeaderLen
	return o + copy(b[o:], v)
}

// AppendRecord appends the encoding of r to dst. It returns dst unchanged when RecordSize(r) is 0.
func AppendRecord(dst []byte, r *Record) []byte {
	n := RecordSize(r)
	if n == 0 {
		return dst
	}
	start := len(dst)
	dst = grow(dst, n)
	EncodeRecord(dst[start:], r)
	return dst
}

func grow(b []byte, n int) []byte {
	if cap(b)-len(b) < n {
		nb := make([]byte, len(b), len(b)+n)
		copy(nb, b)
		b = nb
	}
	return b[:len(b)+n]
}

// RecordFrameLen returns the total size (4 + recLen) of the record that starts at b, or -1 if b is
// shorter than the recLen field.
func RecordFrameLen(b []byte) int {
	if len(b) < 4 {
		return -1
	}
	n := 4 + uint64(binary.LittleEndian.Uint32(b))
	if n > math.MaxInt {
		return -1
	}
	return int(n)
}

// PutTombstone writes the 44-byte tombstone that replaces the record orig on the wire (plan 9.7): orig's
// flags plus FlagSkipped, its times, expiry and payload format, and no variable part. dst must hold
// TombstoneLen bytes. A short orig yields a tombstone with zero header fields.
func PutTombstone(dst []byte, orig []byte) {
	b := dst[:TombstoneLen]
	clear(b)
	le := binary.LittleEndian
	le.PutUint32(b[offRecLen:], TombstoneLen-4)
	b[offVersion] = RecordVersion
	b[offHdrLen] = RecordHeaderLen
	flags := FlagSkipped
	if len(orig) >= RecordHeaderLen {
		flags |= le.Uint16(orig[offFlags:])
		copy(b[offWallNs:offPayloadFormat+1], orig[offWallNs:offPayloadFormat+1])
	}
	le.PutUint16(b[offFlags:], flags)
}

// AppendTombstone appends the tombstone of orig to dst.
func AppendTombstone(dst []byte, orig []byte) []byte {
	start := len(dst)
	dst = grow(dst, TombstoneLen)
	PutTombstone(dst[start:], orig)
	return dst
}

// RecordView is a decoded record. Its byte slices are views into the decoded buffer; nothing is copied.
type RecordView struct {
	Frame         []byte // the whole record, including the recLen prefix
	Version       uint8
	HdrLen        uint8
	Flags         uint16
	PublishWallNs int64
	CaptureMonoMs uint64
	ExpirySec     uint32
	PayloadFormat uint8
	Topic         []byte
	ClientID      []byte
	Username      []byte
	Props         []byte // validated TLV block; iterate with PropIter
	Payload       []byte
	UnknownProps  int // TLVs with an unknown id, skipped
}

func (v *RecordView) QoS() byte              { return byte(v.Flags & FlagQoSMask) }
func (v *RecordView) Retain() bool           { return v.Flags&FlagRetain != 0 }
func (v *RecordView) Dup() bool              { return v.Flags&FlagDup != 0 }
func (v *RecordView) Will() bool             { return v.Flags&FlagWill != 0 }
func (v *RecordView) Inline() bool           { return v.Flags&FlagInline != 0 }
func (v *RecordView) Snapshot() bool         { return v.Flags&FlagSnapshot != 0 }
func (v *RecordView) Skipped() bool          { return v.Flags&FlagSkipped != 0 }
func (v *RecordView) HasPayloadFormat() bool { return v.Flags&FlagPayloadFormat != 0 }
func (v *RecordView) PropIter() PropIter     { return PropIter{b: v.Props} }

// DecodeRecord decodes the record at the start of frame into v without copying. It checks the
// structural rules of plan 12.2 step 1: the recLen invariant, recVersion, hdrLen, QoS <= 2, a non-empty
// topic that is valid UTF-8 without NUL, '+' or '#', valid UTF-8 without NUL in client id, username and
// string properties, and a well-formed TLV block. Bytes of frame beyond 4+recLen are ignored. A
// tombstone (FlagSkipped) is checked for the invariant and QoS only. Every error matches ErrMalformed.
// The batch-relative check captureMonoMs <= sourceMonoMs is done by RecordIter. On an error the fields
// decoded before the failing check stay set, e.g. the topic for a log line.
func DecodeRecord(frame []byte, v *RecordView) error {
	*v = RecordView{}
	if len(frame) < 4 {
		return errRecordTruncated
	}
	le := binary.LittleEndian
	n := 4 + uint64(le.Uint32(frame))
	if n > uint64(len(frame)) {
		return errRecordTruncated
	}
	b := frame[:n]
	v.Frame = b
	if len(b) < RecordHeaderLen {
		return errHeaderShort
	}
	v.Version = b[offVersion]
	v.HdrLen = b[offHdrLen]
	v.Flags = le.Uint16(b[offFlags:])
	v.PublishWallNs = int64(le.Uint64(b[offWallNs:]))
	v.CaptureMonoMs = le.Uint64(b[offMonoMs:])
	v.ExpirySec = le.Uint32(b[offExpiry:])
	v.PayloadFormat = b[offPayloadFormat]
	if v.Version != RecordVersion {
		return errVersion
	}
	hdr := uint64(v.HdrLen)
	if hdr < RecordHeaderLen {
		return errHdrLen
	}
	tl := uint64(le.Uint16(b[offTopicLen:]))
	cl := uint64(le.Uint16(b[offClientIDLen:]))
	ul := uint64(le.Uint16(b[offUsernameLen:]))
	pr := uint64(le.Uint32(b[offPropsLen:]))
	pl := uint64(le.Uint32(b[offPayloadLen:]))
	if hdr+tl+cl+ul+pr+pl != n {
		if hdr > n {
			return errHdrLen
		}
		return errInvariant
	}
	o := hdr
	v.Topic = b[o : o+tl : o+tl]
	o += tl
	v.ClientID = b[o : o+cl : o+cl]
	o += cl
	v.Username = b[o : o+ul : o+ul]
	o += ul
	v.Props = b[o : o+pr : o+pr]
	o += pr
	v.Payload = b[o:n:n]
	if v.Flags&FlagQoSMask == 3 {
		return errQoS
	}
	if v.Flags&FlagSkipped != 0 {
		return nil
	}
	if !validTopicBytes(v.Topic) {
		return errTopic
	}
	if !validStringBytes(v.ClientID) {
		return errClientID
	}
	if !validStringBytes(v.Username) {
		return errUsername
	}
	unknown, err := ValidateProps(v.Props)
	v.UnknownProps = unknown
	return err
}

// ValidateProps checks a props block: every TLV complete, known string properties valid UTF-8 without
// NUL, known values at most 65535 bytes, user properties with a consistent key length. TLVs with an
// unknown id are skipped and returned as unknown.
func ValidateProps(block []byte) (unknown int, err error) {
	it := PropIter{b: block}
	for len(it.b) > 0 {
		id, val, ok := it.Next()
		if !ok {
			return unknown, errTLV
		}
		switch id {
		case PropContentType, PropResponseTopic:
			if len(val) > MaxStringLen {
				return unknown, errPropLen
			}
			if !validStringBytes(val) {
				return unknown, errPropString
			}
		case PropCorrelationData:
			if len(val) > MaxStringLen {
				return unknown, errPropLen
			}
		case PropUserProperty:
			k, uv, ok := SplitUserProperty(val)
			if !ok {
				return unknown, errUserProp
			}
			if len(uv) > MaxStringLen {
				return unknown, errPropLen
			}
			if !validStringBytes(k) || !validStringBytes(uv) {
				return unknown, errPropString
			}
		default:
			unknown++
		}
	}
	return unknown, nil
}

// PropIter walks a TLV props block in order.
type PropIter struct{ b []byte }

// NewPropIter returns an iterator over block.
func NewPropIter(block []byte) PropIter { return PropIter{b: block} }

// Next returns the next TLV. ok is false at the end of the block or when the rest is truncated.
func (it *PropIter) Next() (id uint8, val []byte, ok bool) {
	if len(it.b) < TLVHeaderLen {
		it.b = nil
		return 0, nil, false
	}
	l := uint64(binary.LittleEndian.Uint32(it.b[1:]))
	end := TLVHeaderLen + l
	if end > uint64(len(it.b)) {
		it.b = nil
		return 0, nil, false
	}
	id = it.b[0]
	val = it.b[TLVHeaderLen:end:end]
	it.b = it.b[end:]
	return id, val, true
}

// SplitUserProperty splits the value of a PropUserProperty TLV (u16 keyLen | key | val).
func SplitUserProperty(val []byte) (key, value []byte, ok bool) {
	if len(val) < 2 {
		return nil, nil, false
	}
	k := int(binary.LittleEndian.Uint16(val))
	if 2+k > len(val) {
		return nil, nil, false
	}
	return val[2 : 2+k : 2+k], val[2+k:], true
}

// Properties fills the MQTT 5 publish properties of p from a validated view: MessageExpiryInterval,
// PayloadFormat, ContentType, ResponseTopic, CorrelationData and User in their original order.
// Strings are fresh heap copies; CorrelationData aliases the record buffer (plan 12.3 allows that,
// because every retaining engine path deep-copies it). For repeated ContentType, ResponseTopic or
// CorrelationData TLVs the last one wins.
func (v *RecordView) Properties(p *packets.Properties) {
	p.MessageExpiryInterval = v.ExpirySec
	if v.HasPayloadFormat() {
		p.PayloadFormat = v.PayloadFormat
		p.PayloadFormatFlag = true
	}
	it := v.PropIter()
	for {
		id, val, ok := it.Next()
		if !ok {
			return
		}
		switch id {
		case PropContentType:
			p.ContentType = string(val)
		case PropResponseTopic:
			p.ResponseTopic = string(val)
		case PropCorrelationData:
			p.CorrelationData = val
		case PropUserProperty:
			if k, uv, ok := SplitUserProperty(val); ok {
				p.User = append(p.User, packets.UserProperty{Key: string(k), Val: string(uv)})
			}
		}
	}
}

// Record returns an encoder input with copies of every field of v (unknown TLVs are dropped). It is
// meant for test peers and tooling that re-encode records, not for the receive path.
func (v *RecordView) Record() Record {
	r := Record{
		Flags:         v.Flags,
		PublishWallNs: v.PublishWallNs,
		CaptureMonoMs: v.CaptureMonoMs,
		ExpirySec:     v.ExpirySec,
		PayloadFormat: v.PayloadFormat,
		Topic:         string(v.Topic),
		ClientID:      string(v.ClientID),
		Payload:       append([]byte(nil), v.Payload...),
	}
	if len(v.Username) > 0 {
		r.Username = append([]byte(nil), v.Username...)
	}
	var p packets.Properties
	v.Properties(&p)
	r.ContentType = p.ContentType
	r.ResponseTopic = p.ResponseTopic
	if len(p.CorrelationData) > 0 {
		r.CorrelationData = append([]byte(nil), p.CorrelationData...)
	}
	r.User = p.User
	return r
}

// RecordIter walks the records region of a BATCH, which holds count records back to back.
type RecordIter struct {
	rest      []byte
	remaining uint32
	maxMonoMs uint64
	checkMono bool
	err       error
}

// NewRecordIter returns an iterator over count records in region. Without a CheckMono call the
// captureMonoMs bound is not checked.
func NewRecordIter(region []byte, count uint32) RecordIter {
	return RecordIter{rest: region, remaining: count}
}

// CheckMono makes Next reject records whose captureMonoMs is above the batch's sourceMonoMs.
func (it *RecordIter) CheckMono(sourceMonoMs uint64) {
	it.maxMonoMs = sourceMonoMs
	it.checkMono = true
}

// Remaining returns the number of records the iterator has not returned yet. After a structural
// error these are the records to count as dropped{malformed}.
func (it *RecordIter) Remaining() uint32 { return it.remaining }

// Next decodes the next record into v.
//
//   - (true, nil): v is a valid record.
//   - (true, err): the record is malformed (err matches ErrMalformed); it was consumed, continue.
//   - (false, nil): all count records were returned and the region is used up exactly.
//   - (false, err): batch-structural fault (ErrRecordOverrun or ErrBatchCount); stop. Remaining()
//     records could not be delimited. With ErrBatchCount and Remaining() == 0 every record was
//     returned but the region holds extra bytes.
func (it *RecordIter) Next(v *RecordView) (bool, error) {
	if it.err != nil {
		return false, it.err
	}
	if it.remaining == 0 {
		if len(it.rest) != 0 {
			it.rest = nil
			it.err = ErrBatchCount
			return false, it.err
		}
		return false, nil
	}
	if len(it.rest) == 0 {
		it.err = ErrBatchCount
		return false, it.err
	}
	n := RecordFrameLen(it.rest)
	if n < 0 || n > len(it.rest) {
		it.rest = nil
		it.err = ErrRecordOverrun
		return false, it.err
	}
	frame := it.rest[:n:n]
	it.rest = it.rest[n:]
	it.remaining--
	err := DecodeRecord(frame, v)
	if err == nil && it.checkMono && v.CaptureMonoMs > it.maxMonoMs {
		err = errMonoAhead
	}
	return true, err
}

// ValidTopic reports whether s is a valid topic name for a forwarded record: 1..65535 bytes of valid
// UTF-8 with no NUL, '+' or '#'.
func ValidTopic(s string) bool {
	if len(s) == 0 || len(s) > MaxStringLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == 0 || c == '+' || c == '#' {
			return false
		}
	}
	return utf8.ValidString(s)
}

func validTopicBytes(b []byte) bool {
	if len(b) == 0 || len(b) > MaxStringLen {
		return false
	}
	for _, c := range b {
		if c == 0 || c == '+' || c == '#' {
			return false
		}
	}
	return utf8.Valid(b)
}

// ValidString reports whether s is a valid MQTT UTF-8 string: at most 65535 bytes, valid UTF-8, no NUL.
func ValidString(s string) bool {
	if len(s) > MaxStringLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return false
		}
	}
	return utf8.ValidString(s)
}

func validStringBytes(b []byte) bool {
	if len(b) > MaxStringLen {
		return false
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return utf8.Valid(b)
}

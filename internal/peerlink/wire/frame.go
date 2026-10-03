// Package wire is the codec of the PeerLink protocol mmq-peer/1 (plan-peerlink.md sections 9.3-9.5 and
// 10): the preamble, the frames, the record format shared by the in-memory log and the wire, and the
// handshake MAC helpers. It has no state and does no I/O beyond reading and writing frames. It is
// exported so that test peers can speak the protocol against a real broker.
//
// Byte order is little-endian throughout. str8 is u8 len + bytes, str16 is u16 len + bytes.
//
// Forward compatibility: decoders ignore bytes after the fields they know, both in frame bodies and in
// record headers (hdrLen); unknown frame types are reported as ErrUnknownFrame for the caller to skip;
// unknown property TLVs are skipped and counted; unknown capability and flag bits are ignored.
package wire

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"unicode/utf8"
)

// Protocol constants.
const (
	Magic        = "MMQP"
	VersionMajor = 1
	VersionMinor = 0
	ALPN         = "mmq-peer/1"
	PreambleLen  = 8

	// FrameHeaderLen is u32 frameLen + u8 type. frameLen counts the bytes after the frameLen field.
	FrameHeaderLen = 5

	// MaxPreAuthFrame caps frameLen before HELLO_OK.
	MaxPreAuthFrame = 4 << 10
	// MaxConsumerFrame caps frameLen of frames a source accepts after authentication.
	MaxConsumerFrame = 64 << 10
	// FrameSlack is added to the largest batch or record size to get the consumer's frame cap.
	FrameSlack = 64 << 10
	// DefaultMaxFrameBytes is the default Receive.MaxFrameBytes.
	DefaultMaxFrameBytes = 16<<20 + FrameSlack

	// BatchHeaderLen is the fixed BATCH body before the records.
	BatchHeaderLen = 68
	// BatchPrefixLen is the frame header plus the BATCH header: everything before the records.
	BatchPrefixLen = FrameHeaderLen + BatchHeaderLen
	// batchCRCCovered is the part of the BATCH header the CRC covers: every field before crc32c.
	batchCRCCovered = BatchHeaderLen - 4

	NonceLen = 32
	MACLen   = 32
)

// FrameType is the u8 frame type.
type FrameType uint8

const (
	FrameServerHello FrameType = 0x01
	FrameHello       FrameType = 0x02
	FrameHelloOK     FrameType = 0x03
	FrameGoAway      FrameType = 0x04
	FrameFetch       FrameType = 0x10
	FrameBatch       FrameType = 0x11
	FrameCommit      FrameType = 0x12
	FramePing        FrameType = 0x13
	FramePong        FrameType = 0x14
)

func (t FrameType) String() string {
	switch t {
	case FrameServerHello:
		return "SERVER_HELLO"
	case FrameHello:
		return "HELLO"
	case FrameHelloOK:
		return "HELLO_OK"
	case FrameGoAway:
		return "GOAWAY"
	case FrameFetch:
		return "FETCH"
	case FrameBatch:
		return "BATCH"
	case FrameCommit:
		return "COMMIT"
	case FramePing:
		return "PING"
	case FramePong:
		return "PONG"
	}
	return "UNKNOWN"
}

// Known reports whether t is a frame type of this version.
func (t FrameType) Known() bool {
	switch t {
	case FrameServerHello, FrameHello, FrameHelloOK, FrameGoAway, FrameFetch, FrameBatch, FrameCommit,
		FramePing, FramePong:
		return true
	}
	return false
}

// Capability bits (v1.0). Bits that are not understood are ignored; the agreed set is the intersection.
const (
	CapBatchCRC     uint64 = 1 << 0
	CapSnapshotFill uint64 = 1 << 1
	CapResyncNewer  uint64 = 1 << 2
	CapTombstone    uint64 = 1 << 3

	CapsV1 = CapBatchCRC | CapSnapshotFill | CapResyncNewer | CapTombstone
)

// SERVER_HELLO authModes bits.
const (
	AuthClientCertRequested uint8 = 1 << 0
	AuthSharedSecret        uint8 = 1 << 1
)

// HELLO flags.
const HelloFlagMAC uint16 = 1 << 0

// HELLO_OK flags.
const (
	HelloOKSourceReset       uint16 = 1 << 0
	HelloOKConsumerStateUsed uint16 = 1 << 1
	HelloOKSnapshotAvailable uint16 = 1 << 2
)

// FETCH flags.
const FetchFlagSnapshot uint16 = 1 << 0

// BATCH flags.
const (
	BatchFlagGap         uint16 = 1 << 0
	BatchFlagEmpty       uint16 = 1 << 1
	BatchFlagCRC         uint16 = 1 << 2
	BatchFlagSnapshot    uint16 = 1 << 3
	BatchFlagSnapshotEnd uint16 = 1 << 4
	BatchFlagTruncated   uint16 = 1 << 5
)

// RetainedClass is the retained store class announced in HELLO and HELLO_OK.
type RetainedClass uint8

const (
	RetainedMemory  RetainedClass = 0
	RetainedDB      RetainedClass = 1
	RetainedWinCCOA RetainedClass = 2
)

func (c RetainedClass) String() string {
	switch c {
	case RetainedMemory:
		return "MEMORY"
	case RetainedDB:
		return "DB"
	case RetainedWinCCOA:
		return "WINCCOA"
	}
	return "UNKNOWN"
}

// GoAwayCode is the GOAWAY code (plan 9.4).
type GoAwayCode uint16

const (
	GoAwayVersion          GoAwayCode = 1
	GoAwayUnknownPeer      GoAwayCode = 2
	GoAwayNotAllowed       GoAwayCode = 3
	GoAwayAuthFailed       GoAwayCode = 4
	GoAwayIdentityMismatch GoAwayCode = 5
	GoAwaySelfConnection   GoAwayCode = 6
	GoAwayWrongNode        GoAwayCode = 7
	GoAwaySuperseded       GoAwayCode = 8
	GoAwayShutdown         GoAwayCode = 9
	GoAwayProtocol         GoAwayCode = 10
	GoAwayOffsetOutOfRange GoAwayCode = 11
	GoAwayBusy             GoAwayCode = 12
	GoAwayDuplicateNode    GoAwayCode = 13
)

func (c GoAwayCode) String() string {
	switch c {
	case GoAwayVersion:
		return "version"
	case GoAwayUnknownPeer:
		return "unknown_peer"
	case GoAwayNotAllowed:
		return "not_allowed"
	case GoAwayAuthFailed:
		return "auth_failed"
	case GoAwayIdentityMismatch:
		return "identity_mismatch"
	case GoAwaySelfConnection:
		return "self_connection"
	case GoAwayWrongNode:
		return "wrong_node"
	case GoAwaySuperseded:
		return "superseded"
	case GoAwayShutdown:
		return "shutdown"
	case GoAwayProtocol:
		return "protocol"
	case GoAwayOffsetOutOfRange:
		return "offset_out_of_range"
	case GoAwayBusy:
		return "busy"
	case GoAwayDuplicateNode:
		return "duplicate_node"
	}
	return "unknown"
}

// ConfigError reports whether the code indicates a configuration error, for which the consumer backs
// off to the cap and logs an ERROR (plan 9.10).
func (c GoAwayCode) ConfigError() bool {
	switch c {
	case GoAwayUnknownPeer, GoAwayNotAllowed, GoAwayAuthFailed, GoAwayIdentityMismatch,
		GoAwaySelfConnection, GoAwayWrongNode, GoAwayVersion, GoAwayDuplicateNode:
		return true
	}
	return false
}

// Framing errors. All of them are transport-level faults (GOAWAY(protocol)), except ErrUnknownFrame,
// which the caller skips.
var (
	ErrBadMagic      = errors.New("peerlink/wire: bad preamble magic")
	ErrFrameTooLarge = errors.New("peerlink/wire: frame exceeds the size cap")
	ErrFrameEmpty    = errors.New("peerlink/wire: frame without a type byte")
	ErrShortFrame    = errors.New("peerlink/wire: frame body shorter than its fields")
	ErrUnknownFrame  = errors.New("peerlink/wire: unknown frame type")
	ErrBatchRecords  = errors.New("peerlink/wire: recordsBytes exceeds the frame body")
	// ErrBatchCountRange: the batch count exceeds the number of record frames (at least 4 bytes
	// each) its records region can hold. No source sends this; it is a protocol fault.
	ErrBatchCountRange = errors.New("peerlink/wire: batch count exceeds what the records region can hold")
)

// MinRecordFrame is the smallest record frame a records region can delimit: the u32 recLen alone.
const MinRecordFrame = 4

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// AppendPreamble appends the preamble of this version: "MMQP" | u16 major | u16 minor.
func AppendPreamble(dst []byte) []byte {
	return AppendPreambleVersion(dst, VersionMajor, VersionMinor)
}

// AppendPreambleVersion appends a preamble with an explicit version (test peers).
func AppendPreambleVersion(dst []byte, major, minor uint16) []byte {
	dst = append(dst, Magic...)
	dst = binary.LittleEndian.AppendUint16(dst, major)
	return binary.LittleEndian.AppendUint16(dst, minor)
}

// WritePreamble writes the preamble of this version to w.
func WritePreamble(w io.Writer) error {
	var b [PreambleLen]byte
	_, err := w.Write(AppendPreamble(b[:0]))
	return err
}

// ParsePreamble parses an 8-byte preamble. The caller checks the major version.
func ParsePreamble(b []byte) (major, minor uint16, err error) {
	if len(b) < PreambleLen {
		return 0, 0, io.ErrUnexpectedEOF
	}
	if string(b[:4]) != Magic {
		return 0, 0, ErrBadMagic
	}
	return binary.LittleEndian.Uint16(b[4:]), binary.LittleEndian.Uint16(b[6:]), nil
}

// ReadPreamble reads and parses the preamble from r.
func ReadPreamble(r io.Reader) (major, minor uint16, err error) {
	var b [PreambleLen]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, 0, err
	}
	return ParsePreamble(b[:])
}

// Frame is a decoded or to-be-encoded frame.
type Frame interface {
	Type() FrameType
	// AppendFrame appends the whole frame (frameLen, type, body) to dst.
	AppendFrame(dst []byte) []byte
	// Decode parses a frame body. Bytes after the known fields are ignored.
	Decode(body []byte) error
}

// DecodeFrame decodes a frame body of type t into a new frame value. Unknown types return
// ErrUnknownFrame, which a receiver of the same major version ignores.
func DecodeFrame(t FrameType, body []byte) (Frame, error) {
	var f Frame
	switch t {
	case FrameServerHello:
		f = new(ServerHello)
	case FrameHello:
		f = new(Hello)
	case FrameHelloOK:
		f = new(HelloOK)
	case FrameGoAway:
		f = new(GoAway)
	case FrameFetch:
		f = new(Fetch)
	case FrameBatch:
		f = new(Batch)
	case FrameCommit:
		f = new(Commit)
	case FramePing:
		f = new(Ping)
	case FramePong:
		f = new(Pong)
	default:
		return nil, ErrUnknownFrame
	}
	if err := f.Decode(body); err != nil {
		return nil, err
	}
	return f, nil
}

// WriteFrame encodes f and writes it to w in one Write call.
func WriteFrame(w io.Writer, f Frame) error {
	_, err := w.Write(f.AppendFrame(make([]byte, 0, 128)))
	return err
}

// FrameReader reads frames from a stream. It is not safe for concurrent use.
type FrameReader struct {
	r io.Reader
	// Max caps frameLen; a larger frame is ErrFrameTooLarge. It may be changed between frames.
	Max uint32
	// Progress, if set, is called after every chunk of at most 64 KiB of body bytes read, so the
	// caller can push a progress deadline forward (plan 9.9).
	Progress func()
	hdr      [FrameHeaderLen]byte
	buf      []byte
}

const readChunk = 64 << 10

// NewFrameReader returns a reader with the frame cap max.
func NewFrameReader(r io.Reader, max uint32) *FrameReader {
	return &FrameReader{r: r, Max: max}
}

// ReadHeader reads the next frame header and returns its type and body length. The body must then be
// consumed with ReadBody, Body or Discard before the next ReadHeader.
func (fr *FrameReader) ReadHeader() (FrameType, int, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return 0, 0, err
	}
	n := binary.LittleEndian.Uint32(fr.hdr[:4])
	if n == 0 {
		return 0, 0, ErrFrameEmpty
	}
	if n > fr.Max {
		return 0, 0, ErrFrameTooLarge
	}
	return FrameType(fr.hdr[4]), int(n - 1), nil
}

// ReadBody reads exactly len(dst) body bytes into dst, in chunks that each call Progress.
func (fr *FrameReader) ReadBody(dst []byte) error {
	for len(dst) > 0 {
		n := min(len(dst), readChunk)
		if _, err := io.ReadFull(fr.r, dst[:n]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		dst = dst[n:]
		if fr.Progress != nil {
			fr.Progress()
		}
	}
	return nil
}

// Body reads an n-byte body into the reader's reusable buffer. The result is valid until the next call.
func (fr *FrameReader) Body(n int) ([]byte, error) {
	if cap(fr.buf) < n {
		fr.buf = make([]byte, n)
	}
	b := fr.buf[:n]
	return b, fr.ReadBody(b)
}

// Discard skips an n-byte body, e.g. of an unknown frame type.
func (fr *FrameReader) Discard(n int) error {
	for n > 0 {
		c := min(n, readChunk)
		if _, err := fr.Body(c); err != nil {
			return err
		}
		n -= c
	}
	return nil
}

// ReadFrame reads a whole frame into the reusable buffer. The body is valid until the next call.
func (fr *FrameReader) ReadFrame() (FrameType, []byte, error) {
	t, n, err := fr.ReadHeader()
	if err != nil {
		return 0, nil, err
	}
	b, err := fr.Body(n)
	return t, b, err
}

func beginFrame(dst []byte, t FrameType, bodyHint int) ([]byte, int) {
	if cap(dst)-len(dst) < FrameHeaderLen+bodyHint {
		nb := make([]byte, len(dst), len(dst)+FrameHeaderLen+bodyHint)
		copy(nb, dst)
		dst = nb
	}
	start := len(dst)
	return append(dst, 0, 0, 0, 0, byte(t)), start
}

func endFrame(dst []byte, start int) []byte {
	binary.LittleEndian.PutUint32(dst[start:], uint32(len(dst)-start-4))
	return dst
}

// truncUTF8 shortens s to at most max bytes without splitting a UTF-8 sequence.
func truncUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// appendStr8 appends a str8. Longer strings are truncated to 255 bytes; NodeIds are at most 64 bytes
// after config validation, so truncation only makes an identity check fail closed.
func appendStr8(b []byte, s string) []byte {
	s = truncUTF8(s, 0xff)
	b = append(b, byte(len(s)))
	return append(b, s...)
}

func appendStr16(b []byte, s string) []byte {
	s = truncUTF8(s, 0xffff)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

// dec reads little-endian fields; a read past the end sets short and returns zero values.
type dec struct {
	b     []byte
	short bool
}

func (d *dec) take(n int) []byte {
	if d.short || len(d.b) < n {
		d.short = true
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}

func (d *dec) u8() uint8 {
	if v := d.take(1); v != nil {
		return v[0]
	}
	return 0
}

func (d *dec) u16() uint16 {
	if v := d.take(2); v != nil {
		return binary.LittleEndian.Uint16(v)
	}
	return 0
}

func (d *dec) u32() uint32 {
	if v := d.take(4); v != nil {
		return binary.LittleEndian.Uint32(v)
	}
	return 0
}

func (d *dec) u64() uint64 {
	if v := d.take(8); v != nil {
		return binary.LittleEndian.Uint64(v)
	}
	return 0
}

func (d *dec) arr32(dst *[32]byte) {
	if v := d.take(32); v != nil {
		copy(dst[:], v)
	}
}

func (d *dec) str8() string {
	n := int(d.u8())
	return string(d.take(n))
}

func (d *dec) str16() string {
	n := int(d.u16())
	return string(d.take(n))
}

func (d *dec) err() error {
	if d.short {
		return ErrShortFrame
	}
	return nil
}

// ServerHello is SERVER_HELLO (0x01, S→C), sent before authentication.
type ServerHello struct {
	VersionMajor uint16
	VersionMinor uint16
	Capabilities uint64
	AuthModes    uint8 // AuthClientCertRequested | AuthSharedSecret
	NonceS       [NonceLen]byte
}

func (*ServerHello) Type() FrameType { return FrameServerHello }

func (m *ServerHello) AppendFrame(dst []byte) []byte {
	dst, s := beginFrame(dst, FrameServerHello, 45)
	le := binary.LittleEndian
	dst = le.AppendUint16(dst, m.VersionMajor)
	dst = le.AppendUint16(dst, m.VersionMinor)
	dst = le.AppendUint64(dst, m.Capabilities)
	dst = append(dst, m.AuthModes)
	dst = append(dst, m.NonceS[:]...)
	return endFrame(dst, s)
}

func (m *ServerHello) Decode(body []byte) error {
	d := dec{b: body}
	m.VersionMajor = d.u16()
	m.VersionMinor = d.u16()
	m.Capabilities = d.u64()
	m.AuthModes = d.u8()
	d.arr32(&m.NonceS)
	return d.err()
}

// Hello is HELLO (0x02, C→S).
type Hello struct {
	Flags                uint16 // HelloFlagMAC
	Capabilities         uint64
	InstanceID           uint64
	LastEpoch            uint64
	ResumeOffset         uint64 // consumer appliedNext, 0 = none
	LastSeenLeo          uint64
	MaxRecordBytes       uint32 // largest record the consumer accepts
	RetainedClass        RetainedClass
	NonceC               [NonceLen]byte
	MAC                  [MACLen]byte // zero without a MAC
	ConsumerNodeID       string       // str8
	ExpectedSourceNodeID string       // str8
	TopicRoot            string       // str16
	OASystem             string       // str8; empty unless embedded in WinCC OA with native mode on
}

func (*Hello) Type() FrameType { return FrameHello }

func (m *Hello) AppendFrame(dst []byte) []byte {
	dst, s := beginFrame(dst, FrameHello, 111+4+len(m.ConsumerNodeID)+len(m.ExpectedSourceNodeID)+
		len(m.TopicRoot)+len(m.OASystem)+1)
	le := binary.LittleEndian
	dst = le.AppendUint16(dst, m.Flags)
	dst = le.AppendUint64(dst, m.Capabilities)
	dst = le.AppendUint64(dst, m.InstanceID)
	dst = le.AppendUint64(dst, m.LastEpoch)
	dst = le.AppendUint64(dst, m.ResumeOffset)
	dst = le.AppendUint64(dst, m.LastSeenLeo)
	dst = le.AppendUint32(dst, m.MaxRecordBytes)
	dst = append(dst, byte(m.RetainedClass))
	dst = append(dst, m.NonceC[:]...)
	dst = append(dst, m.MAC[:]...)
	dst = appendStr8(dst, m.ConsumerNodeID)
	dst = appendStr8(dst, m.ExpectedSourceNodeID)
	dst = appendStr16(dst, m.TopicRoot)
	dst = appendStr8(dst, m.OASystem)
	return endFrame(dst, s)
}

func (m *Hello) Decode(body []byte) error {
	d := dec{b: body}
	m.Flags = d.u16()
	m.Capabilities = d.u64()
	m.InstanceID = d.u64()
	m.LastEpoch = d.u64()
	m.ResumeOffset = d.u64()
	m.LastSeenLeo = d.u64()
	m.MaxRecordBytes = d.u32()
	m.RetainedClass = RetainedClass(d.u8())
	d.arr32(&m.NonceC)
	d.arr32(&m.MAC)
	m.ConsumerNodeID = d.str8()
	m.ExpectedSourceNodeID = d.str8()
	m.TopicRoot = d.str16()
	m.OASystem = d.str8()
	return d.err()
}

// HelloOK is HELLO_OK (0x03, S→C), sent after authentication.
type HelloOK struct {
	Flags          uint16 // HelloOKSourceReset | HelloOKConsumerStateUsed | HelloOKSnapshotAvailable
	Capabilities   uint64 // agreed set
	Epoch          uint64
	ResumeAt       uint64
	LogStart       uint64
	Leo            uint64
	Committed      uint64
	LostOnResume   uint64
	WallNowMs      int64
	MonoNowMs      uint64
	MaxRecordBytes uint32 // source capture cap
	RetainedClass  RetainedClass
	MACS           [MACLen]byte // zero without a MAC
	SourceNodeID   string       // str8
	TopicRoot      string       // str16; empty unless the source runs WinCC OA native mode
	OASystem       string       // str8; as in HELLO
}

func (*HelloOK) Type() FrameType { return FrameHelloOK }

func (m *HelloOK) AppendFrame(dst []byte) []byte {
	dst, s := beginFrame(dst, FrameHelloOK, 111+4+len(m.SourceNodeID)+len(m.TopicRoot)+len(m.OASystem))
	le := binary.LittleEndian
	dst = le.AppendUint16(dst, m.Flags)
	dst = le.AppendUint64(dst, m.Capabilities)
	dst = le.AppendUint64(dst, m.Epoch)
	dst = le.AppendUint64(dst, m.ResumeAt)
	dst = le.AppendUint64(dst, m.LogStart)
	dst = le.AppendUint64(dst, m.Leo)
	dst = le.AppendUint64(dst, m.Committed)
	dst = le.AppendUint64(dst, m.LostOnResume)
	dst = le.AppendUint64(dst, uint64(m.WallNowMs))
	dst = le.AppendUint64(dst, m.MonoNowMs)
	dst = le.AppendUint32(dst, m.MaxRecordBytes)
	dst = append(dst, byte(m.RetainedClass))
	dst = append(dst, m.MACS[:]...)
	dst = appendStr8(dst, m.SourceNodeID)
	dst = appendStr16(dst, m.TopicRoot)
	dst = appendStr8(dst, m.OASystem)
	return endFrame(dst, s)
}

func (m *HelloOK) Decode(body []byte) error {
	d := dec{b: body}
	m.Flags = d.u16()
	m.Capabilities = d.u64()
	m.Epoch = d.u64()
	m.ResumeAt = d.u64()
	m.LogStart = d.u64()
	m.Leo = d.u64()
	m.Committed = d.u64()
	m.LostOnResume = d.u64()
	m.WallNowMs = int64(d.u64())
	m.MonoNowMs = d.u64()
	m.MaxRecordBytes = d.u32()
	m.RetainedClass = RetainedClass(d.u8())
	d.arr32(&m.MACS)
	m.SourceNodeID = d.str8()
	m.TopicRoot = d.str16()
	m.OASystem = d.str8()
	return d.err()
}

// GoAway is GOAWAY (0x04, both directions). The sender closes afterwards.
type GoAway struct {
	Code   GoAwayCode
	Reason string // str16; empty before authentication when authentication is configured
}

func (*GoAway) Type() FrameType { return FrameGoAway }

func (m *GoAway) AppendFrame(dst []byte) []byte {
	dst, s := beginFrame(dst, FrameGoAway, 4+len(m.Reason))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(m.Code))
	dst = appendStr16(dst, m.Reason)
	return endFrame(dst, s)
}

func (m *GoAway) Decode(body []byte) error {
	d := dec{b: body}
	m.Code = GoAwayCode(d.u16())
	m.Reason = d.str16()
	return d.err()
}

// Fetch is FETCH (0x10, C→S).
type Fetch struct {
	FetchID    uint32
	Flags      uint16 // FetchFlagSnapshot
	LingerMs   uint16
	Offset     uint64
	Commit     uint64 // 0 = unchanged
	MaxRecords uint32
	MaxBytes   uint32
	MinRecords uint32
	MaxWaitMs  uint32
}

func (*Fetch) Type() FrameType { return FrameFetch }

func (m *Fetch) AppendFrame(dst []byte) []byte {
	dst, s := beginFrame(dst, FrameFetch, 40)
	le := binary.LittleEndian
	dst = le.AppendUint32(dst, m.FetchID)
	dst = le.AppendUint16(dst, m.Flags)
	dst = le.AppendUint16(dst, m.LingerMs)
	dst = le.AppendUint64(dst, m.Offset)
	dst = le.AppendUint64(dst, m.Commit)
	dst = le.AppendUint32(dst, m.MaxRecords)
	dst = le.AppendUint32(dst, m.MaxBytes)
	dst = le.AppendUint32(dst, m.MinRecords)
	dst = le.AppendUint32(dst, m.MaxWaitMs)
	return endFrame(dst, s)
}

func (m *Fetch) Decode(body []byte) error {
	d := dec{b: body}
	m.FetchID = d.u32()
	m.Flags = d.u16()
	m.LingerMs = d.u16()
	m.Offset = d.u64()
	m.Commit = d.u64()
	m.MaxRecords = d.u32()
	m.MaxBytes = d.u32()
	m.MinRecords = d.u32()
	m.MaxWaitMs = d.u32()
	return d.err()
}

// BatchHeader is the fixed 68-byte part of a BATCH body.
type BatchHeader struct {
	FetchID      uint32
	Flags        uint16 // BatchFlag*
	Reserved     uint16
	BaseOffset   uint64
	Count        uint32
	RecordsBytes uint32
	LogStart     uint64
	Leo          uint64
	Lost         uint64
	SourceMonoMs uint64
	SourceWallMs int64
	CRC32C       uint32 // valid if BatchFlagCRC
}

func (h *BatchHeader) put(b []byte) {
	le := binary.LittleEndian
	le.PutUint32(b[0:], h.FetchID)
	le.PutUint16(b[4:], h.Flags)
	le.PutUint16(b[6:], h.Reserved)
	le.PutUint64(b[8:], h.BaseOffset)
	le.PutUint32(b[16:], h.Count)
	le.PutUint32(b[20:], h.RecordsBytes)
	le.PutUint64(b[24:], h.LogStart)
	le.PutUint64(b[32:], h.Leo)
	le.PutUint64(b[40:], h.Lost)
	le.PutUint64(b[48:], h.SourceMonoMs)
	le.PutUint64(b[56:], uint64(h.SourceWallMs))
	le.PutUint32(b[64:], h.CRC32C)
}

func (h *BatchHeader) parse(b []byte) {
	le := binary.LittleEndian
	h.FetchID = le.Uint32(b[0:])
	h.Flags = le.Uint16(b[4:])
	h.Reserved = le.Uint16(b[6:])
	h.BaseOffset = le.Uint64(b[8:])
	h.Count = le.Uint32(b[16:])
	h.RecordsBytes = le.Uint32(b[20:])
	h.LogStart = le.Uint64(b[24:])
	h.Leo = le.Uint64(b[32:])
	h.Lost = le.Uint64(b[40:])
	h.SourceMonoMs = le.Uint64(b[48:])
	h.SourceWallMs = int64(le.Uint64(b[56:]))
	h.CRC32C = le.Uint32(b[64:])
}

// EncodeBatchPrefix writes everything before the records of a BATCH: frameLen (from h.RecordsBytes),
// the type and the header with h.CRC32C as given. The serve path writes it followed by the record
// frames, e.g. as one net.Buffers. It does not allocate.
func EncodeBatchPrefix(dst *[BatchPrefixLen]byte, h *BatchHeader) {
	binary.LittleEndian.PutUint32(dst[0:], uint32(1+BatchHeaderLen)+h.RecordsBytes)
	dst[4] = byte(FrameBatch)
	h.put(dst[FrameHeaderLen:])
}

// SetBatchCRC computes the batch CRC32C (Castagnoli) over the header fields before crc32c in an
// encoded prefix, followed by every record in order, stores it in the prefix and returns it. The
// prefix flags must already include BatchFlagCRC. It does not allocate.
func SetBatchCRC(prefix *[BatchPrefixLen]byte, records [][]byte) uint32 {
	crc := crc32.Update(0, castagnoli, prefix[FrameHeaderLen:FrameHeaderLen+batchCRCCovered])
	for _, r := range records {
		crc = crc32.Update(crc, castagnoli, r)
	}
	binary.LittleEndian.PutUint32(prefix[BatchPrefixLen-4:], crc)
	return crc
}

// Batch is BATCH (0x11, S→C): the header and a view of the records region.
type Batch struct {
	Header  BatchHeader
	Records []byte // RecordsBytes bytes of back-to-back records; a view into the decoded body
	raw     []byte // the decoded header bytes, so CRC checks need no re-encoding
}

func (*Batch) Type() FrameType { return FrameBatch }

// AppendFrame appends the header as given (RecordsBytes and CRC32C are not recomputed, so test peers
// can craft faulty frames) followed by Records.
func (m *Batch) AppendFrame(dst []byte) []byte {
	dst, s := beginFrame(dst, FrameBatch, BatchHeaderLen+len(m.Records))
	o := len(dst)
	dst = grow(dst, BatchHeaderLen)
	m.Header.put(dst[o:])
	dst = append(dst, m.Records...)
	return endFrame(dst, s)
}

// Decode parses the header and sets Records to a view of the records region. Bytes after the region
// are ignored. A region larger than the body is ErrBatchRecords and a count the region cannot hold is
// ErrBatchCountRange; both are transport-level faults. Callers may size per-record state by Count
// only after Decode succeeded.
func (m *Batch) Decode(body []byte) error {
	m.raw = nil
	if len(body) < BatchHeaderLen {
		return ErrShortFrame
	}
	m.Header.parse(body)
	end := uint64(BatchHeaderLen) + uint64(m.Header.RecordsBytes)
	if end > uint64(len(body)) {
		m.Records = nil
		return ErrBatchRecords
	}
	m.raw = body[:BatchHeaderLen:BatchHeaderLen]
	m.Records = body[BatchHeaderLen:end:end]
	if uint64(m.Header.Count)*MinRecordFrame > uint64(m.Header.RecordsBytes) {
		return ErrBatchCountRange
	}
	return nil
}

// ComputeCRC returns the CRC32C over the header fields before crc32c and Records. For a decoded batch
// it reads the received header bytes and does not allocate.
func (m *Batch) ComputeCRC() uint32 {
	hb := m.raw
	if hb == nil {
		hb = make([]byte, BatchHeaderLen)
		m.Header.put(hb)
	}
	crc := crc32.Update(0, castagnoli, hb[:batchCRCCovered])
	return crc32.Update(crc, castagnoli, m.Records)
}

// CRCValid reports whether the batch carries no CRC or a matching one.
func (m *Batch) CRCValid() bool {
	return m.Header.Flags&BatchFlagCRC == 0 || m.ComputeCRC() == m.Header.CRC32C
}

// Iter returns an iterator over the records that also checks captureMonoMs against SourceMonoMs.
func (m *Batch) Iter() RecordIter {
	it := NewRecordIter(m.Records, m.Header.Count)
	it.CheckMono(m.Header.SourceMonoMs)
	return it
}

// Commit is COMMIT (0x12, C→S): every record below Commit was applied.
type Commit struct{ Commit uint64 }

func (*Commit) Type() FrameType { return FrameCommit }

func (m *Commit) AppendFrame(dst []byte) []byte {
	dst, s := beginFrame(dst, FrameCommit, 8)
	dst = binary.LittleEndian.AppendUint64(dst, m.Commit)
	return endFrame(dst, s)
}

func (m *Commit) Decode(body []byte) error {
	d := dec{b: body}
	m.Commit = d.u64()
	return d.err()
}

// Ping is PING (0x13, C→S).
type Ping struct{ Token uint64 }

func (*Ping) Type() FrameType { return FramePing }

func (m *Ping) AppendFrame(dst []byte) []byte {
	dst, s := beginFrame(dst, FramePing, 8)
	dst = binary.LittleEndian.AppendUint64(dst, m.Token)
	return endFrame(dst, s)
}

func (m *Ping) Decode(body []byte) error {
	d := dec{b: body}
	m.Token = d.u64()
	return d.err()
}

// Pong is PONG (0x14, S→C), echoing the PING token.
type Pong struct{ Token uint64 }

func (*Pong) Type() FrameType { return FramePong }

func (m *Pong) AppendFrame(dst []byte) []byte {
	dst, s := beginFrame(dst, FramePong, 8)
	dst = binary.LittleEndian.AppendUint64(dst, m.Token)
	return endFrame(dst, s)
}

func (m *Pong) Decode(body []byte) error {
	d := dec{b: body}
	m.Token = d.u64()
	return d.err()
}

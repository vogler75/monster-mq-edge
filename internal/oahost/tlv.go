// Package oahost is the Go side of the WinCC OA embedding contract
// (winccoa/plans/spec-winccoa-native.md). It is pure Go: the cgo exports in
// embed/cabi adapt a C host to the Host interface defined here.
package oahost

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"
)

const (
	TagOp       byte = 1
	TagName     byte = 2
	TagQuery    byte = 3
	TagFlags    byte = 4
	TagValue    byte = 5
	TagRef      byte = 6
	TagError    byte = 7
	TagRow      byte = 8
	TagTypeName byte = 9
	TagElemType byte = 10
	TagSysName  byte = 11
	TagExists   byte = 12
	TagTime     byte = 13
	TagCount    byte = 14

	// SYS_INFO answer, redundancy facts (absent from older hosts).
	TagRedundant byte = 15
	TagReplica   byte = 16
	TagHost      byte = 17 // repeated: event host 1, event host 2
	TagLocalHost byte = 18
)

// MaxMessage bounds any single ABI message (spec section 7).
const MaxMessage = 1 << 20

var ErrMalformed = errors.New("oahost: malformed message")

type Field struct {
	Tag  byte
	Data []byte
}

type Writer struct{ buf []byte }

func (w *Writer) Bytes() []byte { return w.buf }
func (w *Writer) Len() int      { return len(w.buf) }

func (w *Writer) Raw(tag byte, data []byte) {
	var hdr [5]byte
	hdr[0] = tag
	binary.LittleEndian.PutUint32(hdr[1:], uint32(len(data)))
	w.buf = append(w.buf, hdr[:]...)
	w.buf = append(w.buf, data...)
}

func (w *Writer) String(tag byte, s string) { w.Raw(tag, []byte(s)) }

func (w *Writer) U32(tag byte, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.Raw(tag, b[:])
}

func (w *Writer) U64(tag byte, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.Raw(tag, b[:])
}

func (w *Writer) I64(tag byte, v int64) { w.U64(tag, uint64(v)) }

func (w *Writer) Bool(tag byte, v bool) {
	if v {
		w.Raw(tag, []byte{1})
	} else {
		w.Raw(tag, []byte{0})
	}
}

func (w *Writer) Value(tag byte, v Value) { w.Raw(tag, EncodeValue(v)) }

// Parse splits a message into fields. It never retains b beyond the returned
// slices, which alias b.
func Parse(b []byte) ([]Field, error) {
	var out []Field
	for len(b) > 0 {
		if len(b) < 5 {
			return nil, ErrMalformed
		}
		tag := b[0]
		n := binary.LittleEndian.Uint32(b[1:5])
		b = b[5:]
		if uint64(n) > uint64(len(b)) {
			return nil, ErrMalformed
		}
		out = append(out, Field{Tag: tag, Data: b[:n]})
		b = b[n:]
	}
	return out, nil
}

type Message struct{ Fields []Field }

func ParseMessage(b []byte) (Message, error) {
	f, err := Parse(b)
	return Message{Fields: f}, err
}

func (m Message) First(tag byte) ([]byte, bool) {
	for _, f := range m.Fields {
		if f.Tag == tag {
			return f.Data, true
		}
	}
	return nil, false
}

func (m Message) All(tag byte) [][]byte {
	var out [][]byte
	for _, f := range m.Fields {
		if f.Tag == tag {
			out = append(out, f.Data)
		}
	}
	return out
}

func (m Message) String(tag byte) string {
	b, _ := m.First(tag)
	return string(b)
}

func (m Message) U32(tag byte) (uint32, bool) {
	b, ok := m.First(tag)
	if !ok || len(b) != 4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b), true
}

func (m Message) U64(tag byte) (uint64, bool) {
	b, ok := m.First(tag)
	if !ok || len(b) != 8 {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b), true
}

func (m Message) Bool(tag byte) bool {
	b, ok := m.First(tag)
	return ok && len(b) == 1 && b[0] != 0
}

// Kind is the typed-value discriminator of spec section 3.2.
type Kind byte

const (
	KindNull     Kind = 0
	KindBool     Kind = 1
	KindInt      Kind = 2
	KindUint     Kind = 3
	KindFloat    Kind = 4
	KindString   Kind = 5
	KindTime     Kind = 6
	KindBytes    Kind = 7
	KindDyn      Kind = 8
	KindLangText Kind = 9
	KindBit32    Kind = 10
)

type Value struct {
	Kind  Kind
	Bool  bool
	Int   int64
	Uint  uint64
	Float float64
	Str   string
	Time  time.Time
	Bytes []byte
	Dyn   []Value
}

func EncodeValue(v Value) []byte {
	out := []byte{byte(v.Kind)}
	switch v.Kind {
	case KindNull:
	case KindBool:
		if v.Bool {
			out = append(out, 1)
		} else {
			out = append(out, 0)
		}
	case KindInt:
		out = binary.LittleEndian.AppendUint64(out, uint64(v.Int))
	case KindUint:
		out = binary.LittleEndian.AppendUint64(out, v.Uint)
	case KindBit32:
		out = binary.LittleEndian.AppendUint32(out, uint32(v.Uint))
	case KindFloat:
		out = binary.LittleEndian.AppendUint64(out, math.Float64bits(v.Float))
	case KindString, KindLangText:
		out = append(out, v.Str...)
	case KindTime:
		out = binary.LittleEndian.AppendUint64(out, uint64(v.Time.UnixMilli()))
	case KindBytes:
		out = append(out, v.Bytes...)
	case KindDyn:
		var w Writer
		for _, item := range v.Dyn {
			w.Value(TagValue, item)
		}
		out = append(out, w.Bytes()...)
	}
	return out
}

func DecodeValue(b []byte) (Value, error) {
	if len(b) == 0 {
		return Value{}, ErrMalformed
	}
	k := Kind(b[0])
	body := b[1:]
	v := Value{Kind: k}
	need := func(n int) error {
		if len(body) != n {
			return fmt.Errorf("%w: value kind %d needs %d bytes, got %d", ErrMalformed, k, n, len(body))
		}
		return nil
	}
	switch k {
	case KindNull:
		if err := need(0); err != nil {
			return v, err
		}
	case KindBool:
		if err := need(1); err != nil {
			return v, err
		}
		v.Bool = body[0] != 0
	case KindInt:
		if err := need(8); err != nil {
			return v, err
		}
		v.Int = int64(binary.LittleEndian.Uint64(body))
	case KindUint:
		if err := need(8); err != nil {
			return v, err
		}
		v.Uint = binary.LittleEndian.Uint64(body)
	case KindBit32:
		if err := need(4); err != nil {
			return v, err
		}
		v.Uint = uint64(binary.LittleEndian.Uint32(body))
	case KindFloat:
		if err := need(8); err != nil {
			return v, err
		}
		v.Float = math.Float64frombits(binary.LittleEndian.Uint64(body))
	case KindString, KindLangText:
		v.Str = string(body)
	case KindTime:
		if err := need(8); err != nil {
			return v, err
		}
		v.Time = time.UnixMilli(int64(binary.LittleEndian.Uint64(body))).UTC()
	case KindBytes:
		v.Bytes = append([]byte(nil), body...)
	case KindDyn:
		fields, err := Parse(body)
		if err != nil {
			return v, err
		}
		v.Dyn = make([]Value, 0, len(fields))
		for _, f := range fields {
			if f.Tag != TagValue {
				continue
			}
			item, err := DecodeValue(f.Data)
			if err != nil {
				return v, err
			}
			v.Dyn = append(v.Dyn, item)
		}
	default:
		return v, fmt.Errorf("%w: unknown value kind %d", ErrMalformed, k)
	}
	return v, nil
}

// JSON converts a value to the representation used by the WinCC OA GraphQL
// server, so native and GraphQL bridge output stay identical: times are
// JavaScript-style ISO strings in UTC with millisecond precision and blobs
// are Node Buffer objects.
func (v Value) JSON() any {
	switch v.Kind {
	case KindNull:
		return nil
	case KindBool:
		return v.Bool
	case KindInt:
		return v.Int
	case KindUint, KindBit32:
		return v.Uint
	case KindFloat:
		if math.IsNaN(v.Float) || math.IsInf(v.Float, 0) {
			return nil
		}
		return v.Float
	case KindString, KindLangText:
		if !utf8.ValidString(v.Str) {
			return string([]rune(v.Str))
		}
		return v.Str
	case KindTime:
		return v.Time.UTC().Format("2006-01-02T15:04:05.000Z")
	case KindBytes:
		data := make([]any, len(v.Bytes))
		for i, c := range v.Bytes {
			data[i] = int(c)
		}
		return map[string]any{"type": "Buffer", "data": data}
	case KindDyn:
		out := make([]any, len(v.Dyn))
		for i, item := range v.Dyn {
			out[i] = item.JSON()
		}
		return out
	}
	return nil
}

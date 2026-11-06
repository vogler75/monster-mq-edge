// Package wire is a minimal hand-rolled codec for the subset of the Kafka wire
// protocol the MonsterMQ Kafka server speaks (non-flexible request versions).
package wire

import (
	"encoding/binary"
	"errors"
)

// ErrShort is returned (sticky) when a Reader runs past the end of its data.
var ErrShort = errors.New("kafka wire: short buffer")

// ErrInvalid is returned for out-of-range lengths.
var ErrInvalid = errors.New("kafka wire: invalid length")

// Reader decodes big-endian Kafka primitives from a byte slice. The first
// error sticks; later reads return zero values, so handlers check Err once.
type Reader struct {
	b   []byte
	pos int
	err error
}

func NewReader(b []byte) *Reader { return &Reader{b: b} }

func (r *Reader) Err() error { return r.err }

// Remaining is the number of unread bytes.
func (r *Reader) Remaining() int { return len(r.b) - r.pos }

func (r *Reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.pos+n > len(r.b) {
		r.err = ErrShort
		return nil
	}
	s := r.b[r.pos : r.pos+n]
	r.pos += n
	return s
}

func (r *Reader) Int8() int8 {
	s := r.take(1)
	if s == nil {
		return 0
	}
	return int8(s[0])
}

func (r *Reader) Int16() int16 {
	s := r.take(2)
	if s == nil {
		return 0
	}
	return int16(binary.BigEndian.Uint16(s))
}

func (r *Reader) Int32() int32 {
	s := r.take(4)
	if s == nil {
		return 0
	}
	return int32(binary.BigEndian.Uint32(s))
}

func (r *Reader) Int64() int64 {
	s := r.take(8)
	if s == nil {
		return 0
	}
	return int64(binary.BigEndian.Uint64(s))
}

// NullableString reads an int16-length string; nil for length -1.
func (r *Reader) NullableString() *string {
	n := r.Int16()
	if r.err != nil || n < 0 {
		return nil
	}
	s := r.take(int(n))
	if s == nil && n > 0 {
		return nil
	}
	str := string(s)
	return &str
}

// String reads an int16-length string, null becoming "".
func (r *Reader) String() string {
	if p := r.NullableString(); p != nil {
		return *p
	}
	return ""
}

// Bytes reads an int32-length byte array; nil for length -1. The result
// aliases the underlying buffer.
func (r *Reader) Bytes() []byte {
	n := r.Int32()
	if r.err != nil || n < 0 {
		return nil
	}
	s := r.take(int(n))
	if s == nil {
		return nil
	}
	return s
}

// Raw reads n bytes (aliasing the buffer).
func (r *Reader) Raw(n int) []byte { return r.take(n) }

// ArrayLen reads an int32 array length and rejects values that cannot fit in
// the remaining data (each element needs at least minElem bytes), so a hostile
// count cannot trigger a huge allocation. Null arrays (-1) return 0.
func (r *Reader) ArrayLen(minElem int) int {
	n := int(r.Int32())
	if r.err != nil {
		return 0
	}
	if n < 0 {
		return 0
	}
	if minElem < 1 {
		minElem = 1
	}
	if int64(n)*int64(minElem) > int64(r.Remaining()) {
		r.err = ErrInvalid
		return 0
	}
	return n
}

// Varint reads a zigzag-encoded int32.
func (r *Reader) Varint() int32 { return int32(r.Varlong()) }

// Varlong reads a zigzag-encoded int64.
func (r *Reader) Varlong() int64 {
	var u uint64
	var shift uint
	for i := 0; i < 10; i++ {
		s := r.take(1)
		if s == nil {
			return 0
		}
		b := s[0]
		u |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return int64(u>>1) ^ -int64(u&1)
		}
		shift += 7
	}
	r.err = ErrInvalid
	return 0
}

// SkipString reads and discards a string.
func (r *Reader) SkipString() { _ = r.String() }

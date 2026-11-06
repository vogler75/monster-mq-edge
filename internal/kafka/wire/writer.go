package wire

import "encoding/binary"

// Writer appends big-endian Kafka primitives to a growing buffer.
type Writer struct{ b []byte }

func NewWriter() *Writer { return &Writer{b: make([]byte, 0, 256)} }

// Bytes returns the encoded data.
func (w *Writer) Bytes() []byte { return w.b }

func (w *Writer) Len() int { return len(w.b) }

func (w *Writer) Int8(v int8) { w.b = append(w.b, byte(v)) }

func (w *Writer) Int16(v int16) { w.b = binary.BigEndian.AppendUint16(w.b, uint16(v)) }

func (w *Writer) Int32(v int32) { w.b = binary.BigEndian.AppendUint32(w.b, uint32(v)) }

func (w *Writer) Int64(v int64) { w.b = binary.BigEndian.AppendUint64(w.b, uint64(v)) }

// String writes an int16-length string.
func (w *Writer) String(s string) {
	w.Int16(int16(len(s)))
	w.b = append(w.b, s...)
}

// NullString writes a nullable string (nil = length -1).
func (w *Writer) NullString(s *string) {
	if s == nil {
		w.Int16(-1)
		return
	}
	w.String(*s)
}

// Bytes32 writes an int32-length byte array; nil writes length -1.
func (w *Writer) Bytes32(p []byte) {
	if p == nil {
		w.Int32(-1)
		return
	}
	w.Int32(int32(len(p)))
	w.b = append(w.b, p...)
}

// Raw appends bytes without a length prefix.
func (w *Writer) Raw(p []byte) { w.b = append(w.b, p...) }

// Varlong appends a zigzag varint.
func (w *Writer) Varlong(v int64) {
	u := uint64(v<<1) ^ uint64(v>>63)
	for u >= 0x80 {
		w.b = append(w.b, byte(u)|0x80)
		u >>= 7
	}
	w.b = append(w.b, byte(u))
}

func (w *Writer) Varint(v int32) { w.Varlong(int64(v)) }

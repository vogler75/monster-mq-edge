package wire

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// Record is one decoded produce record. Key is nil for a null key.
type Record struct {
	Key, Value []byte
}

// UnsupportedCodecError reports a compression codec other than none/GZIP.
type UnsupportedCodecError struct{ Codec int }

func (e *UnsupportedCodecError) Error() string {
	return fmt.Sprintf("unsupported compression codec %d", e.Codec)
}

// maxDecompressed bounds gzip expansion (zip-bomb guard).
const maxDecompressed = 64 << 20

func gunzip(p []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(p))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxDecompressed+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxDecompressed {
		return nil, fmt.Errorf("decompressed record set exceeds %d bytes", maxDecompressed)
	}
	return out, nil
}

// DecodeRecords decodes a produce partition record set: one or more RecordBatch
// v2 batches or a legacy MessageSet (magic 0/1, GZIP-wrapped sets allowed).
// Records without a value (null or empty) are skipped; headers, timestamps and
// CRCs are ignored. An incomplete trailing batch is ignored.
func DecodeRecords(set []byte) ([]Record, error) {
	if len(set) >= 17 && set[16] == 2 {
		return decodeBatches(set, nil)
	}
	return decodeLegacy(set, nil, 0)
}

func decodeBatches(set []byte, out []Record) ([]Record, error) {
	for len(set) >= 61 {
		batchLen := int(int32(binary.BigEndian.Uint32(set[8:12])))
		if batchLen < 49 || 12+batchLen > len(set) {
			break // truncated trailing batch
		}
		batch := set[12 : 12+batchLen]
		set = set[12+batchLen:]
		if batch[4] != 2 { // magic
			return out, fmt.Errorf("unexpected magic %d in record batch", batch[4])
		}
		attrs := int16(binary.BigEndian.Uint16(batch[9:11]))
		count := int(int32(binary.BigEndian.Uint32(batch[45:49])))
		body := batch[49:]
		switch codec := int(attrs & 0x07); codec {
		case 0:
		case 1:
			var err error
			if body, err = gunzip(body); err != nil {
				return out, fmt.Errorf("gzip record batch: %w", err)
			}
		default:
			return out, &UnsupportedCodecError{Codec: codec}
		}
		r := NewReader(body)
		for i := 0; i < count && r.Remaining() > 0; i++ {
			n := int(r.Varint())
			rec := r.Raw(n)
			if r.Err() != nil || n <= 0 {
				return out, fmt.Errorf("record %d: %w", i, ErrShort)
			}
			rr := NewReader(rec)
			rr.Int8()    // attributes
			rr.Varlong() // timestamp delta
			rr.Varint()  // offset delta
			var key, val []byte
			if kl := int(rr.Varint()); kl >= 0 {
				key = rr.Raw(kl)
				if key == nil {
					key = []byte{}
				}
			}
			if vl := int(rr.Varint()); vl >= 0 {
				val = rr.Raw(vl)
			}
			if rr.Err() != nil {
				return out, fmt.Errorf("record %d: %w", i, rr.Err())
			}
			if len(val) > 0 {
				out = append(out, Record{Key: key, Value: val})
			}
		}
	}
	return out, nil
}

func decodeLegacy(set []byte, out []Record, depth int) ([]Record, error) {
	r := NewReader(set)
	for r.Remaining() >= 12 {
		r.Int64() // offset
		size := int(r.Int32())
		if size <= 0 || size > r.Remaining() {
			break // truncated trailing message
		}
		m := NewReader(r.Raw(size))
		m.Int32() // crc
		magic := m.Int8()
		attrs := m.Int8()
		if magic == 1 {
			m.Int64() // timestamp
		}
		key := m.Bytes()
		val := m.Bytes()
		if m.Err() != nil {
			return out, m.Err()
		}
		switch codec := int(attrs & 0x03); codec {
		case 0:
			if len(val) > 0 {
				out = append(out, Record{Key: key, Value: val})
			}
		case 1:
			if depth > 0 {
				return out, fmt.Errorf("nested compressed message sets")
			}
			inner, err := gunzip(val)
			if err != nil {
				return out, fmt.Errorf("gzip message set: %w", err)
			}
			if out, err = decodeLegacy(inner, out, depth+1); err != nil {
				return out, err
			}
		default:
			return out, &UnsupportedCodecError{Codec: codec}
		}
	}
	return out, nil
}

// MessageV0 is one entry of an encoded MessageSet v0.
type MessageV0 struct {
	Offset     int64
	Key, Value []byte
}

// EncodeMessageSetV0 encodes entries as a MessageSet v0 (magic 0, no
// compression) with a CRC32 (IEEE) per message.
func EncodeMessageSetV0(entries []MessageV0) []byte {
	w := NewWriter()
	for _, e := range entries {
		body := NewWriter()
		body.Int8(0) // magic
		body.Int8(0) // attributes
		body.Bytes32(e.Key)
		body.Bytes32(e.Value)
		w.Int64(e.Offset)
		w.Int32(int32(4 + body.Len()))
		w.Int32(int32(crc32.ChecksumIEEE(body.Bytes())))
		w.Raw(body.Bytes())
	}
	return w.Bytes()
}

package h264

import "fmt"

// SplitAnnexB splits a complete Annex B byte stream into NAL units. The returned
// slices alias data and exclude start codes and trailing_zero_8bits. For RTSP,
// use Depacketizer instead; RTP carries NAL units without Annex B start codes.
func SplitAnnexB(data []byte) ([][]byte, error) {
	var out [][]byte
	start := -1
	zeros := 0
	for i, v := range data {
		if v == 0 {
			zeros++
			continue
		}
		if v == 1 && zeros >= 2 {
			if start >= 0 {
				if i-zeros == start {
					return nil, fmt.Errorf("%w: empty Annex B NAL", ErrMalformed)
				}
				out = append(out, data[start:i-zeros])
			} else if i-zeros != 0 {
				return nil, fmt.Errorf("%w: bytes before Annex B start code", ErrMalformed)
			}
			start = i + 1
		}
		zeros = 0
	}
	if start < 0 || len(data)-zeros <= start {
		return nil, fmt.Errorf("%w: missing Annex B NAL", ErrMalformed)
	}
	out = append(out, data[start:len(data)-zeros])
	return out, nil
}

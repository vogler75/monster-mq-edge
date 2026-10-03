package wire

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
)

// Handshake MAC for shared secrets over TLS (plan 9.5, 17.3).
//
//	mac  = HMAC-SHA256(secret, lp("mmq-peer/1 C") | lp(nonceS) | lp(nonceC) | lp(consumerId) | lp(sourceId) | lp(exporter))
//	macS = HMAC-SHA256(secret, lp("mmq-peer/1 S") | lp(nonceC) | lp(nonceS) | lp(sourceId) | lp(consumerId) | lp(exporter))
//
// lp(x) = u16 len(x) little-endian + x. exporter is
// tls.ConnectionState.ExportKeyingMaterial(ExporterLabel, nil, ExporterLen).
const (
	ExporterLabel = "monstermq-peer/1"
	ExporterLen   = 32

	macLabelConsumer = "mmq-peer/1 C"
	macLabelSource   = "mmq-peer/1 S"
)

// AppendLP appends lp(x). Inputs longer than 65535 bytes are cut to 65535 on both sides alike; every
// MAC input (nonces, NodeIds, exporter) is far shorter.
func AppendLP(dst, x []byte) []byte {
	x = x[:min(len(x), 0xffff)]
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(x)))
	return append(dst, x...)
}

// AppendLPString appends lp(s).
func AppendLPString(dst []byte, s string) []byte {
	s = s[:min(len(s), 0xffff)]
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(s)))
	return append(dst, s...)
}

// ConsumerMACInput builds the HMAC input of HELLO.mac.
func ConsumerMACInput(nonceS, nonceC *[NonceLen]byte, consumerID, sourceID string, exporter []byte) []byte {
	b := make([]byte, 0, 6*2+len(macLabelConsumer)+2*NonceLen+len(consumerID)+len(sourceID)+len(exporter))
	b = AppendLPString(b, macLabelConsumer)
	b = AppendLP(b, nonceS[:])
	b = AppendLP(b, nonceC[:])
	b = AppendLPString(b, consumerID)
	b = AppendLPString(b, sourceID)
	return AppendLP(b, exporter)
}

// SourceMACInput builds the HMAC input of HELLO_OK.macS.
func SourceMACInput(nonceC, nonceS *[NonceLen]byte, sourceID, consumerID string, exporter []byte) []byte {
	b := make([]byte, 0, 6*2+len(macLabelSource)+2*NonceLen+len(consumerID)+len(sourceID)+len(exporter))
	b = AppendLPString(b, macLabelSource)
	b = AppendLP(b, nonceC[:])
	b = AppendLP(b, nonceS[:])
	b = AppendLPString(b, sourceID)
	b = AppendLPString(b, consumerID)
	return AppendLP(b, exporter)
}

// MAC returns HMAC-SHA256(secret, input).
func MAC(secret, input []byte) [MACLen]byte {
	h := hmac.New(sha256.New, secret)
	h.Write(input)
	var out [MACLen]byte
	h.Sum(out[:0])
	return out
}

// MatchMAC returns the index of the first secret whose MAC over input equals mac, or -1. The
// comparison is constant-time per secret (hmac.Equal). Secret lists allow rotation (plan 17.5).
func MatchMAC(secrets [][]byte, input []byte, mac *[MACLen]byte) int {
	for i, s := range secrets {
		m := MAC(s, input)
		if hmac.Equal(m[:], mac[:]) {
			return i
		}
	}
	return -1
}

// NewNonce returns 32 bytes from crypto/rand.
func NewNonce() [NonceLen]byte {
	var n [NonceLen]byte
	_, _ = rand.Read(n[:])
	return n
}

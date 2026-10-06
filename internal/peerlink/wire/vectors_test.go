package wire

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"monstermq.io/edge/internal/mqtt/packets"
)

type VectorJSON struct {
	PreambleHex string         `json:"preambleHex"`
	Frames      []FrameVector  `json:"frames"`
	Records     []RecordVector `json:"records"`
	Batches     []BatchVector  `json:"batches"`
	MACs        []MACVector    `json:"macs"`
	Invalid     []InvalidCase  `json:"invalid"`
}

type FrameVector struct {
	Name    string `json:"name"`
	Type    uint8  `json:"type"`
	Hex     string `json:"hex"`
	Payload string `json:"payload"`
}

type RecordVector struct {
	Name            string                 `json:"name"`
	Hex             string                 `json:"hex"`
	Flags           uint16                 `json:"flags"`
	PublishWallNs   int64                  `json:"publishWallNs"`
	CaptureMonoMs   uint64                 `json:"captureMonoMs"`
	ExpirySec       uint32                 `json:"expirySec"`
	PayloadFormat   uint8                  `json:"payloadFormat"`
	Topic           string                 `json:"topic"`
	ClientID        string                 `json:"clientId"`
	Username        string                 `json:"username"`
	ContentType     string                 `json:"contentType"`
	ResponseTopic   string                 `json:"responseTopic"`
	CorrelationData string                 `json:"correlationDataHex"`
	UserProperties  []packets.UserProperty `json:"userProperties"`
	PayloadHex      string                 `json:"payloadHex"`
	PayloadString   string                 `json:"payloadString"`
}

type BatchVector struct {
	Name         string `json:"name"`
	Hex          string `json:"hex"`
	FetchID      uint64 `json:"fetchId"`
	Flags        uint16 `json:"flags"`
	BaseOffset   uint64 `json:"baseOffset"`
	Count        uint32 `json:"count"`
	RecordsBytes uint32 `json:"recordsBytes"`
	LogStart     uint64 `json:"logStart"`
	Leo          uint64 `json:"leo"`
	Lost         uint64 `json:"lost"`
	SourceMonoMs uint64 `json:"sourceMonoMs"`
	SourceWallMs int64  `json:"sourceWallMs"`
	CRC32C       uint32 `json:"crc32c"`
}

type MACVector struct {
	Name         string `json:"name"`
	IsConsumer   bool   `json:"isConsumer"`
	SecretHex    string `json:"secretHex"`
	NonceSHex    string `json:"nonceSHex"`
	NonceCHex    string `json:"nonceCHex"`
	ConsumerID   string `json:"consumerId"`
	SourceID     string `json:"sourceId"`
	ExporterHex  string `json:"exporterHex"`
	InputHex     string `json:"inputHex"`
	ExpectedMACHex string `json:"expectedMacHex"`
}

type InvalidCase struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Hex      string `json:"hex"`
	Expected string `json:"expected"`
}

func buildVectors() VectorJSON {
	var vj VectorJSON

	vj.PreambleHex = hex.EncodeToString(AppendPreamble(nil))

	// Frames
	for _, f := range sampleFrames() {
		raw := f.AppendFrame(nil)
		vj.Frames = append(vj.Frames, FrameVector{
			Name: f.Type().String(),
			Type: uint8(f.Type()),
			Hex:  hex.EncodeToString(raw),
		})
	}

	// Records
	recCases := []struct {
		name string
		rec  Record
	}{
		{"full", fullRecord()},
		{"minimal", Record{Topic: "sensors/temp"}},
		{"qos2_will", Record{
			Flags: 2 | FlagWill, Topic: "clients/c1/state", ClientID: "c1", Payload: []byte("offline"),
			PublishWallNs: -5, CaptureMonoMs: 0,
		}},
		{"qos1_retain_dup", Record{
			Flags: 1 | FlagRetain | FlagDup, Topic: "status/node", ClientID: "node-a", Payload: []byte("active"),
			PublishWallNs: 1700000000000000000, CaptureMonoMs: 500, ExpirySec: 3600,
		}},
	}

	for _, rc := range recCases {
		r := rc.rec
		sz := RecordSize(&r)
		buf := make([]byte, sz)
		EncodeRecord(buf, &r)

		vj.Records = append(vj.Records, RecordVector{
			Name:            rc.name,
			Hex:             hex.EncodeToString(buf),
			Flags:           r.Flags,
			PublishWallNs:   r.PublishWallNs,
			CaptureMonoMs:   r.CaptureMonoMs,
			ExpirySec:       r.ExpirySec,
			PayloadFormat:   r.PayloadFormat,
			Topic:           r.Topic,
			ClientID:        r.ClientID,
			Username:        string(r.Username),
			ContentType:     r.ContentType,
			ResponseTopic:   r.ResponseTopic,
			CorrelationData: hex.EncodeToString(r.CorrelationData),
			UserProperties:  r.User,
			PayloadHex:      hex.EncodeToString(r.Payload),
			PayloadString:   string(r.Payload),
		})

		// Tombstone version
		var tomb [TombstoneLen]byte
		PutTombstone(tomb[:], buf)
		vj.Records = append(vj.Records, RecordVector{
			Name:          rc.name + "_tombstone",
			Hex:           hex.EncodeToString(tomb[:]),
			Flags:         r.Flags | FlagSkipped,
			PublishWallNs: r.PublishWallNs,
			CaptureMonoMs: r.CaptureMonoMs,
			ExpirySec:     r.ExpirySec,
			PayloadFormat: r.PayloadFormat,
		})
	}

	// Batches
	b1 := &Batch{
		Header: BatchHeader{
			FetchID: 42, Flags: BatchFlagCRC, BaseOffset: 100, Count: 2,
			RecordsBytes: uint32(len(vj.Records[0].Hex)/2 + len(vj.Records[1].Hex)/2),
			LogStart: 10, Leo: 102, Lost: 0, SourceMonoMs: 123456, SourceWallMs: 1759500000000,
		},
	}
	rec1, _ := hex.DecodeString(vj.Records[0].Hex)
	rec2, _ := hex.DecodeString(vj.Records[1].Hex)
	b1.Records = append(append([]byte(nil), rec1...), rec2...)
	b1.Header.RecordsBytes = uint32(len(b1.Records))
	b1.Header.CRC32C = b1.ComputeCRC()
	rawB1 := b1.AppendFrame(nil)

	vj.Batches = append(vj.Batches, BatchVector{
		Name:         "two_records_crc",
		Hex:          hex.EncodeToString(rawB1),
		FetchID:      uint64(b1.Header.FetchID),
		Flags:        b1.Header.Flags,
		BaseOffset:   b1.Header.BaseOffset,
		Count:        b1.Header.Count,
		RecordsBytes: b1.Header.RecordsBytes,
		LogStart:     b1.Header.LogStart,
		Leo:          b1.Header.Leo,
		Lost:         b1.Header.Lost,
		SourceMonoMs: b1.Header.SourceMonoMs,
		SourceWallMs: b1.Header.SourceWallMs,
		CRC32C:       b1.Header.CRC32C,
	})

	// MACs
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	nS := nonce(10)
	nC := nonce(20)
	exporter := []byte("tls13-exporter-32-bytes-test!!!!")

	cInput := ConsumerMACInput(&nS, &nC, "node-consumer", "node-source", exporter)
	cMAC := MAC(secret, cInput)
	vj.MACs = append(vj.MACs, MACVector{
		Name:           "consumer_mac",
		IsConsumer:     true,
		SecretHex:      hex.EncodeToString(secret),
		NonceSHex:      hex.EncodeToString(nS[:]),
		NonceCHex:      hex.EncodeToString(nC[:]),
		ConsumerID:     "node-consumer",
		SourceID:       "node-source",
		ExporterHex:    hex.EncodeToString(exporter),
		InputHex:       hex.EncodeToString(cInput),
		ExpectedMACHex: hex.EncodeToString(cMAC[:]),
	})

	sInput := SourceMACInput(&nC, &nS, "node-source", "node-consumer", exporter)
	sMAC := MAC(secret, sInput)
	vj.MACs = append(vj.MACs, MACVector{
		Name:           "source_mac",
		IsConsumer:     false,
		SecretHex:      hex.EncodeToString(secret),
		NonceSHex:      hex.EncodeToString(nS[:]),
		NonceCHex:      hex.EncodeToString(nC[:]),
		ConsumerID:     "node-consumer",
		SourceID:       "node-source",
		ExporterHex:    hex.EncodeToString(exporter),
		InputHex:       hex.EncodeToString(sInput),
		ExpectedMACHex: hex.EncodeToString(sMAC[:]),
	})

	// Invalid frames / records
	vj.Invalid = append(vj.Invalid,
		InvalidCase{"frame", "short_header", "02000000", "ErrShortFrame"},
		InvalidCase{"frame", "empty_frame", "00000000", "ErrFrameEmpty"},
		InvalidCase{"record", "short_record", "0500000001", "ErrRecordShort"},
	)

	return vj
}

func TestVectors(t *testing.T) {
	vj := buildVectors()
	data, err := json.MarshalIndent(vj, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	target := filepath.Join("testdata", "vectors.json")
	if os.Getenv("PEERLINK_UPDATE_VECTORS") == "1" {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(target, data, 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Logf("Updated %s (%d bytes)", target, len(data))
	} else {
		existing, err := os.ReadFile(target)
		if err != nil {
			t.Skipf("No existing vectors file (run with PEERLINK_UPDATE_VECTORS=1): %v", err)
		}
		if string(existing) != string(data) {
			t.Fatalf("Vectors mismatch! Run with PEERLINK_UPDATE_VECTORS=1 to update")
		}
	}
}

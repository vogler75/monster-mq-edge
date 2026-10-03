package packets

import (
	"bytes"
	"testing"
)

func TestCopyDropsForwardAndWill(t *testing.T) {
	pk := Packet{
		FixedHeader: FixedHeader{Type: Publish, Qos: 1, Retain: true},
		TopicName:   "a/b",
		Payload:     []byte("v"),
		Origin:      "c1",
		Created:     100,
		Forward:     &Forward{SourceNode: "oa-a", ClientID: "c1", Epoch: 2, Offset: 7, Will: true},
		Will:        true,
	}

	for _, allowTransfer := range []bool{false, true} {
		out := pk.Copy(allowTransfer)
		if out.Forward != nil {
			t.Errorf("Copy(%v) copied Forward", allowTransfer)
		}
		if out.Will {
			t.Errorf("Copy(%v) copied Will", allowTransfer)
		}
		if out.TopicName != pk.TopicName || out.Origin != pk.Origin || out.Created != pk.Created {
			t.Errorf("Copy(%v) lost publish fields: %+v", allowTransfer, out)
		}
	}
	if pk.Forward == nil || !pk.Will {
		t.Fatal("Copy modified its source")
	}
}

func TestPublishEncodeIgnoresForwardAndWill(t *testing.T) {
	base := Packet{
		FixedHeader:     FixedHeader{Type: Publish, Qos: 1},
		TopicName:       "a/b",
		Payload:         []byte("v"),
		PacketID:        5,
		ProtocolVersion: 5,
	}
	marked := base
	marked.Forward = &Forward{SourceNode: "oa-a", ClientID: "c1", TimeNs: 1, Epoch: 1, Offset: 1, Dup: true, Will: true, Snapshot: true}
	marked.Will = true

	var a, b bytes.Buffer
	if err := base.PublishEncode(&a); err != nil {
		t.Fatal(err)
	}
	if err := marked.PublishEncode(&b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("Forward/Will changed the wire encoding:\n%x\n%x", a.Bytes(), b.Bytes())
	}
}

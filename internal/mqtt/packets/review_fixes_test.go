package packets

import (
	"bytes"
	"errors"
	"testing"
)

// Regression tests for dev/plans/plan-mqtt-code-review-findings.md.

func TestSubscribeDecodeTruncatedV5DoesNotPanic(t *testing.T) {
	// packet id 1, property length 0, filter "a/b", then no subscription options byte
	buf := []byte{0x00, 0x01, 0x00, 0x00, 0x03, 'a', '/', 'b'}
	pk := Packet{ProtocolVersion: 5, FixedHeader: FixedHeader{Type: Subscribe, Qos: 1, Remaining: len(buf)}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SubscribeDecode panicked: %v", r)
		}
	}()
	if err := pk.SubscribeDecode(buf); err == nil {
		t.Fatal("SubscribeDecode accepted a truncated packet")
	}
}

func TestDisconnectDecodeReasonOnly(t *testing.T) {
	buf := []byte{CodeDisconnectWillMessage.Code}
	pk := Packet{ProtocolVersion: 5, FixedHeader: FixedHeader{Type: Disconnect, Remaining: len(buf)}}
	if err := pk.DisconnectDecode(buf); err != nil {
		t.Fatal(err)
	}
	if pk.ReasonCode != CodeDisconnectWillMessage.Code {
		t.Fatalf("ReasonCode = %#x, want %#x", pk.ReasonCode, CodeDisconnectWillMessage.Code)
	}
}

func TestAuthDecodeEmptyAndReasonOnly(t *testing.T) {
	pk := Packet{ProtocolVersion: 5, FixedHeader: FixedHeader{Type: Auth}}
	if err := pk.AuthDecode(nil); err != nil {
		t.Fatalf("empty AUTH: %v", err)
	}
	if pk.ReasonCode != CodeSuccess.Code {
		t.Fatalf("empty AUTH ReasonCode = %#x, want success", pk.ReasonCode)
	}

	pk = Packet{ProtocolVersion: 5, FixedHeader: FixedHeader{Type: Auth, Remaining: 1}}
	if err := pk.AuthDecode([]byte{CodeContinueAuthentication.Code}); err != nil {
		t.Fatalf("reason-only AUTH: %v", err)
	}
	if pk.ReasonCode != CodeContinueAuthentication.Code {
		t.Fatalf("reason-only AUTH ReasonCode = %#x", pk.ReasonCode)
	}
}

func TestDecodeLengthRejectsFifthByte(t *testing.T) {
	if _, _, err := DecodeLength(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0x7f})); err != nil {
		t.Fatalf("4-byte maximum rejected: %v", err)
	}
	_, _, err := DecodeLength(bytes.NewReader([]byte{0x80, 0x80, 0x80, 0x80, 0x00}))
	if !errors.Is(err, ErrMalformedVariableByteInteger) {
		t.Fatalf("5-byte encoding: err = %v, want ErrMalformedVariableByteInteger", err)
	}
}

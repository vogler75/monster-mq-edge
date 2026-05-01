package mqttclient

import "testing"

func TestOutboundFilterTreatsFixedLocalTopicAsPrefix(t *testing.T) {
	if got := outboundFilter("opcua/factory"); got != "opcua/factory/#" {
		t.Fatalf("outboundFilter = %q", got)
	}
	if got := outboundFilter("opcua/factory/#"); got != "opcua/factory/#" {
		t.Fatalf("outboundFilter wildcard = %q", got)
	}
}

func TestMapOutboundTopicFixedLocalPrefix(t *testing.T) {
	addr := Address{
		Mode:        "PUBLISH",
		LocalTopic:  "opcua/factory",
		RemoteTopic: "cloud/factory",
		RemovePath:  true,
	}
	if got := mapOutboundTopic(addr, "opcua/factory"); got != "cloud/factory" {
		t.Fatalf("exact topic mapped to %q", got)
	}
	if got := mapOutboundTopic(addr, "opcua/factory/line1/temp"); got != "cloud/factory/line1/temp" {
		t.Fatalf("child topic mapped to %q", got)
	}
}

func TestMapOutboundTopicWildcardLocalPrefix(t *testing.T) {
	addr := Address{
		Mode:        "PUBLISH",
		LocalTopic:  "opcua/factory/#",
		RemoteTopic: "cloud/factory",
		RemovePath:  true,
	}
	if got := mapOutboundTopic(addr, "opcua/factory/line1/temp"); got != "cloud/factory/line1/temp" {
		t.Fatalf("wildcard topic mapped to %q", got)
	}
}

func TestMapOutboundTopicRemoteWildcardUsesLiteralPrefix(t *testing.T) {
	addr := Address{
		Mode:        "PUBLISH",
		LocalTopic:  "opcua/factory/#",
		RemoteTopic: "cloud/factory/#",
		RemovePath:  true,
	}
	if got := mapOutboundTopic(addr, "opcua/factory/line1/temp"); got != "cloud/factory/line1/temp" {
		t.Fatalf("remote wildcard topic mapped to %q", got)
	}
}

func TestMapInboundTopicLocalWildcardUsesLiteralPrefix(t *testing.T) {
	addr := Address{
		Mode:        "SUBSCRIBE",
		RemoteTopic: "cloud/factory/#",
		LocalTopic:  "opcua/factory/#",
		RemovePath:  true,
	}
	if got := mapInboundTopic(addr, "cloud/factory/line1/temp"); got != "opcua/factory/line1/temp" {
		t.Fatalf("local wildcard topic mapped to %q", got)
	}
}

func TestMapInboundTopicWithoutRemovePathAppendsFullRemoteTopic(t *testing.T) {
	addr := Address{
		Mode:        "SUBSCRIBE",
		RemoteTopic: "cloud/factory/#",
		LocalTopic:  "opcua/factory",
		RemovePath:  false,
	}
	if got := mapInboundTopic(addr, "cloud/factory/line1/temp"); got != "opcua/factory/cloud/factory/line1/temp" {
		t.Fatalf("full remote topic mapped to %q", got)
	}
}

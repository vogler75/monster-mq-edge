package winccoanative

import (
	"errors"
	"testing"
)

func TestParseResolutionForms(t *testing.T) {
	cases := []struct {
		topic, read, write string
		cmd                bool
	}{
		{"winccoa/local/tags/Pump101/speed", "System1:Pump101.speed:_online.._value", "System1:Pump101.speed:_original.._value", false},
		{"winccoa/local/types/AnalogDrive/Pump101/speed", "System1:Pump101.speed:_online.._value", "", false},
		{"winccoa/remote/SubstationA/tags/Feeder1/voltage", "SubstationA:Feeder1.voltage:_online.._value", "", false},
		{"winccoa/remote/SubstationA/types/Feeder/Feeder1/voltage", "SubstationA:Feeder1.voltage:_online.._value", "", false},
		{"winccoa/local/tags/ScalarTag", "System1:ScalarTag.:_online.._value", "System1:ScalarTag.:_original.._value", false},
		{"winccoa/remote/SubstationA/tags/ScalarTag", "SubstationA:ScalarTag.:_online.._value", "", false},
		{"winccoa/local/tags/Pump101/speed/_online.._value", "System1:Pump101.speed:_online.._value", "", false},
		{"winccoa/local/tags/Pump101/speed/_online.._stime", "System1:Pump101.speed:_online.._stime", "", false},
		{"winccoa/local/tags/Pump101/a/b/c", "System1:Pump101.a.b.c:_online.._value", "", false},
		{"winccoa/local/tags/Pump1/speed/set", "", "System1:Pump1.speed:_original.._value", true},
		{"winccoa/remote/SubstationA/tags/Pump1/speed/set", "", "SubstationA:Pump1.speed:_original.._value", true},
		{"winccoa/local/tags/Pump1/%73et", "System1:Pump1.set:_online.._value", "", false},
		{"winccoa/local/tags/A%2FB/x", "System1:A/B.x:_online.._value", "", false},
	}
	for _, c := range cases {
		tg, err := Parse(c.topic)
		if err != nil {
			t.Fatalf("%s: %v", c.topic, err)
		}
		sys := "System1"
		if tg.Remote {
			sys = tg.System
		}
		if tg.Command != c.cmd {
			t.Errorf("%s: command=%v", c.topic, tg.Command)
		}
		if c.read != "" && tg.ReadAddress(sys) != c.read {
			t.Errorf("%s: read %q want %q", c.topic, tg.ReadAddress(sys), c.read)
		}
		if c.write != "" && tg.WriteAddress(sys) != c.write {
			t.Errorf("%s: write %q want %q", c.topic, tg.WriteAddress(sys), c.write)
		}
		if tg.Topic() != c.topic {
			t.Errorf("round trip %q -> %q", c.topic, tg.Topic())
		}
	}
}

func TestParseNeverDoubleDot(t *testing.T) {
	tg, err := Parse("winccoa/local/tags/Pump101/speed")
	if err != nil {
		t.Fatal(err)
	}
	if got := tg.DPE("S"); got != "S:Pump101.speed" {
		t.Fatalf("DPE %q", got)
	}
	if got := tg.DPName("S"); got != "S:Pump101" {
		t.Fatalf("DPName %q", got)
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"winccoa/tags/Pump101/speed",             // former unscoped form
		"winccoa/types/AnalogDrive/Pump101",      // former unscoped form
		"winccoa/System1/Pump101/speed",          // former direct-system form (not native)
		"winccoa/remote/tags/Pump101",            // missing remote system -> "tags" is the system, then missing tags/types
		"winccoa/remote",                         // missing system
		"winccoa/local",                          // missing tags/types
		"winccoa/local/tags",                     // missing dp
		"winccoa/local/tags/Pump101/_config.._x", // attribute not allowlisted
		"winccoa/local/tags/Pump101/_original.._value",
		"winccoa/local/tags/Pump%2e1/x", // lowercase hex
		"winccoa/local/tags/Pump%41/x",  // unnecessary escape
		"winccoa/local/tags/Pu.mp/x",    // dot in name
		"winccoa/local/tags/Pu:mp/x",    // colon in name
		"winccoa/local/tags//x",         // empty segment
		"winccoa/local/tags/Pump1/+",    // wildcard
		"winccoa/local/tags/set",        // command with no dp
		"winccoa/local/tags/Pump%2",     // truncated escape
	}
	for _, b := range bad {
		if _, err := Parse(b); err == nil {
			t.Errorf("%s: expected error", b)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"winccoa/local/tags/a":       KindNative,
		"winccoa/remote/S/tags/a":    KindNative,
		"winccoa/node/this/status":   KindStatus,
		"winccoa/node/edge-1/status": KindStatus,
		"winccoa/node/+/status":      KindNode,
		"winccoa/node/this/other":    KindNode,
		"winccoa/cns/view/a":         KindCNS,
		"winccoa/System1/a":          KindOther,
		"winccoaX/local/a":           KindOther,
		"plant/winccoa/local/tags/a": KindOther,
	}
	for topic, want := range cases {
		if got := Classify(topic); got != want {
			t.Errorf("%s: got %v want %v", topic, got, want)
		}
	}
	if f, ok := SplitShared("$share/g/winccoa/local/tags/a"); !ok || f != "winccoa/local/tags/a" {
		t.Errorf("SplitShared %q %v", f, ok)
	}
}

func TestErrorKinds(t *testing.T) {
	_, err := Parse("winccoa/local/tags/Pump%41/x")
	if !errors.Is(err, ErrNoncanonical) {
		t.Fatalf("got %v", err)
	}
	_, err = Parse("winccoa/local/tags/Pump/_config.._x")
	if !errors.Is(err, ErrAttribute) {
		t.Fatalf("got %v", err)
	}
}

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
		{"winccoa/System1/tags/Pump101/speed", "System1:Pump101.speed:_online.._value", "System1:Pump101.speed:_original.._value", false},
		{"winccoa/System1/types/AnalogDrive/Pump101/speed", "System1:Pump101.speed:_online.._value", "", false},
		{"winccoa/SubstationA/tags/Feeder1/voltage", "SubstationA:Feeder1.voltage:_online.._value", "", false},
		{"winccoa/SubstationA/types/Feeder/Feeder1/voltage", "SubstationA:Feeder1.voltage:_online.._value", "", false},
		{"winccoa/System1/tags/ScalarTag", "System1:ScalarTag.:_online.._value", "System1:ScalarTag.:_original.._value", false},
		{"winccoa/SubstationA/tags/ScalarTag", "SubstationA:ScalarTag.:_online.._value", "", false},
		{"winccoa/System1/tags/Pump101/speed/_online.._value", "System1:Pump101.speed:_online.._value", "", false},
		{"winccoa/System1/tags/Pump101/speed/_online.._stime", "System1:Pump101.speed:_online.._stime", "", false},
		{"winccoa/System1/tags/Pump101/a/b/c", "System1:Pump101.a.b.c:_online.._value", "", false},
		{"winccoa/System1/tags/Pump1/speed/set", "", "System1:Pump1.speed:_original.._value", true},
		{"winccoa/SubstationA/tags/Pump1/speed/set", "", "SubstationA:Pump1.speed:_original.._value", true},
		{"winccoa/System1/tags/Pump1/%73et", "System1:Pump1.set:_online.._value", "", false},
		{"winccoa/System1/tags/A%2FB/x", "System1:A/B.x:_online.._value", "", false},
	}
	for _, c := range cases {
		tg, err := Parse(c.topic)
		if err != nil {
			t.Fatalf("%s: %v", c.topic, err)
		}
		sys := tg.System
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
	tg, err := Parse("winccoa/System1/tags/Pump101/speed")
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
		"winccoa/tags/Pump101/speed",               // system missing: "tags" is the system, then no tags/types
		"winccoa/System1/Pump101/speed",            // missing tags/types
		"winccoa/System1",                          // status topic, not a tag
		"winccoa/System1/tags",                     // missing dp
		"winccoa/Sys%41/tags/Pump101",              // noncanonical system name
		"winccoa/System1/tags/Pump101/_config.._x", // attribute not allowlisted
		"winccoa/System1/tags/Pump101/_original.._value",
		"winccoa/System1/tags/Pump%2e1/x", // lowercase hex
		"winccoa/System1/tags/Pump%41/x",  // unnecessary escape
		"winccoa/System1/tags/Pu.mp/x",    // dot in name
		"winccoa/System1/tags/Pu:mp/x",    // colon in name
		"winccoa/System1/tags//x",         // empty segment
		"winccoa/System1/tags/Pump1/+",    // wildcard
		"winccoa/System1/tags/set",        // command with no dp
		"winccoa/System1/tags/Pump%2",     // truncated escape
	}
	for _, b := range bad {
		if _, err := Parse(b); err == nil {
			t.Errorf("%s: expected error", b)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"winccoa/System1/tags/a":       KindNative,
		"winccoa/SubstationA/tags/a":   KindNative,
		"winccoa/System1/other":        KindNative,
		"winccoa/System1":              KindStatus,
		"winccoa/System1/cns":          KindCNS,
		"winccoa/System1/cns/view/a":   KindCNS,
		"winccoa":                      KindOther,
		"winccoa/#":                    KindOther,
		"winccoa/+/tags/a":             KindOther,
		"winccoaX/System1/a":           KindOther,
		"plant/winccoa/System1/tags/a": KindOther,
	}
	for topic, want := range cases {
		if got := Classify(topic); got != want {
			t.Errorf("%s: got %v want %v", topic, got, want)
		}
	}
	if f, ok := SplitShared("$share/g/winccoa/System1/tags/a"); !ok || f != "winccoa/System1/tags/a" {
		t.Errorf("SplitShared %q %v", f, ok)
	}
}

func TestErrorKinds(t *testing.T) {
	_, err := Parse("winccoa/System1/tags/Pump%41/x")
	if !errors.Is(err, ErrNoncanonical) {
		t.Fatalf("got %v", err)
	}
	_, err = Parse("winccoa/System1/tags/Pump/_config.._x")
	if !errors.Is(err, ErrAttribute) {
		t.Fatalf("got %v", err)
	}
}

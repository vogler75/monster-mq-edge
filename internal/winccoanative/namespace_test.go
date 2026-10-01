package winccoanative

import (
	"errors"
	"strings"
	"testing"
)

func TestParseResolutionForms(t *testing.T) {
	cases := []struct {
		topic, read, write string
		cmd                bool
	}{
		{"winccoa/systems/System1/tags/Pump101/speed", "System1:Pump101.speed:_online.._value", "System1:Pump101.speed:_original.._value", false},
		{"winccoa/systems/System1/types/AnalogDrive/Pump101/speed", "System1:Pump101.speed:_online.._value", "", false},
		{"winccoa/systems/SubstationA/tags/Feeder1/voltage", "SubstationA:Feeder1.voltage:_online.._value", "", false},
		{"winccoa/systems/SubstationA/types/Feeder/Feeder1/voltage", "SubstationA:Feeder1.voltage:_online.._value", "", false},
		{"winccoa/systems/System1/tags/ScalarTag", "System1:ScalarTag.:_online.._value", "System1:ScalarTag.:_original.._value", false},
		{"winccoa/systems/SubstationA/tags/ScalarTag", "SubstationA:ScalarTag.:_online.._value", "", false},
		{"winccoa/systems/System1/tags/Pump101/speed/_online.._value", "System1:Pump101.speed:_online.._value", "", false},
		{"winccoa/systems/System1/tags/Pump101/speed/_online.._stime", "System1:Pump101.speed:_online.._stime", "", false},
		{"winccoa/systems/System1/tags/Pump101/a/b/c", "System1:Pump101.a.b.c:_online.._value", "", false},
		{"winccoa/systems/System1/tags/Pump1/speed/set", "", "System1:Pump1.speed:_original.._value", true},
		{"winccoa/systems/SubstationA/tags/Pump1/speed/set", "", "SubstationA:Pump1.speed:_original.._value", true},
		{"winccoa/systems/System1/tags/Pump1/%73et", "System1:Pump1.set:_online.._value", "", false},
		{"winccoa/systems/System1/tags/A%2FB/x", "System1:A/B.x:_online.._value", "", false},
	}
	for _, c := range cases {
		tg, err := DefaultNames.Parse(c.topic)
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
	tg, err := DefaultNames.Parse("winccoa/systems/System1/tags/Pump101/speed")
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
		"winccoa/System1/tags/Pump101/speed",               // system without systems/
		"winccoa/systems/tags/Pump101",                     // "tags" is the system, then no tags/types
		"winccoa/systems/System1/Pump101/speed",            // missing tags/types
		"winccoa/systems/System1",                          // status topic, not a tag
		"winccoa/systems/System1/tags",                     // missing dp
		"winccoa/systems/Sys%41/tags/Pump101",              // noncanonical system name
		"winccoa/systems/System1/tags/Pump101/_config.._x", // attribute not allowlisted
		"winccoa/systems/System1/tags/Pump101/_original.._value",
		"winccoa/systems/System1/tags/Pump%2e1/x", // lowercase hex
		"winccoa/systems/System1/tags/Pump%41/x",  // unnecessary escape
		"winccoa/systems/System1/tags/Pu.mp/x",    // dot in name
		"winccoa/systems/System1/tags/Pu:mp/x",    // colon in name
		"winccoa/systems/System1/tags//x",         // empty segment
		"winccoa/systems/System1/tags/Pump1/+",    // wildcard
		"winccoa/systems/System1/tags/set",        // command with no dp
		"winccoa/systems/System1/tags/Pump%2",     // truncated escape
	}
	for _, b := range bad {
		if _, err := DefaultNames.Parse(b); err == nil {
			t.Errorf("%s: expected error", b)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"winccoa/systems/System1/tags/a":       KindNative,
		"winccoa/systems/SubstationA/tags/a":   KindNative,
		"winccoa/systems/System1/other":        KindNative,
		"winccoa/systems/System1":              KindStatus,
		"winccoa/systems/System1/cns":          KindCNS,
		"winccoa/systems/System1/cns/view/a":   KindCNS,
		"winccoa":                              KindStatus, // local shortcut
		"winccoa/#":                            KindOther,
		"winccoa/+/tags/a":                     KindOther,
		"winccoaX/System1/a":                   KindOther,
		"plant/winccoa/systems/System1/tags/a": KindOther,
	}
	for topic, want := range cases {
		if got := DefaultNames.Classify(topic); got != want {
			t.Errorf("%s: got %v want %v", topic, got, want)
		}
	}
	if f, ok := SplitShared("$share/g/winccoa/systems/System1/tags/a"); !ok || f != "winccoa/systems/System1/tags/a" {
		t.Errorf("SplitShared %q %v", f, ok)
	}
}

func TestErrorKinds(t *testing.T) {
	_, err := DefaultNames.Parse("winccoa/systems/System1/tags/Pump%41/x")
	if !errors.Is(err, ErrNoncanonical) {
		t.Fatalf("got %v", err)
	}
	_, err = DefaultNames.Parse("winccoa/systems/System1/tags/Pump/_config.._x")
	if !errors.Is(err, ErrAttribute) {
		t.Fatalf("got %v", err)
	}
}

func TestCustomNames(t *testing.T) {
	n := Names{Root: "plant/oa", Tags: "t", Types: "dpt", Systems: "sys", Topics: "mq"}
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}
	tg, err := n.Parse("plant/oa/sys/System1/dpt/Pump/Pump1/speed")
	if err != nil {
		t.Fatal(err)
	}
	if tg.TypeName != "Pump" || tg.ReadAddress(tg.System) != "System1:Pump1.speed:_online.._value" {
		t.Fatalf("parsed %+v", tg)
	}
	if got := tg.Topic(); got != "plant/oa/sys/System1/dpt/Pump/Pump1/speed" {
		t.Fatalf("round trip %q", got)
	}
	if c, ok := n.CanonicalOf("plant/oa/sys/System1/dpt/Pump/Pump1/speed"); !ok || c != "plant/oa/sys/System1/t/Pump1/speed" {
		t.Fatalf("canonical %q %v", c, ok)
	}
	w, err := n.ParseWildcard("plant/oa/sys/System1/t/Pump1/#")
	if err != nil {
		t.Fatal(err)
	}
	if rt, ok := w.RowTarget("System1:Pump1.speed", ""); !ok || rt.Topic() != "plant/oa/sys/System1/t/Pump1/speed" {
		t.Fatalf("row topic %q", rt.Topic())
	}
	for topic, want := range map[string]Kind{
		"plant/oa/sys/System1":               KindStatus,
		"plant/oa/sys/System1/t/Pump1/x":     KindNative,
		"plant/oa/sys/System1/cns/v":         KindCNS,
		"plant/oa/sys/System1/mq/a/b":        KindTopics,
		"plant/oa/mq/a":                      KindTopics,
		"plant/oa/#":                         KindOther,
		"winccoa/systems/System1/tags/Pump1": KindOther,
		"plant/oax/System1/t/Pump1/x":        KindOther,
	} {
		if got := n.Classify(topic); got != want {
			t.Errorf("%s: got %v want %v", topic, got, want)
		}
	}
	if _, err := n.Parse("plant/oa/sys/System1/tags/Pump1/speed"); err == nil {
		t.Error("default tags name accepted with custom names")
	}
	if got := n.StatusTopic("System1"); got != "plant/oa/sys/System1" {
		t.Errorf("status topic %q", got)
	}
	for _, bad := range []Names{
		{Root: "", Tags: "t", Types: "y"},
		{Root: "a//b", Tags: "t", Types: "y"},
		{Root: "a/+", Tags: "t", Types: "y"},
		{Root: "$oa", Tags: "t", Types: "y"},
		{Root: "oa", Tags: "t/x", Types: "y"},
		{Root: "oa", Tags: "same", Types: "same"},
		{Root: "oa", Tags: "cns", Types: "y"},
		{Root: "oa", Tags: "t", Types: ""},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v: expected a validation error", bad)
		}
	}
}

func TestShortcutNames(t *testing.T) {
	n := DefaultNames
	for topic, want := range map[string]Kind{
		"winccoa":                              KindStatus,
		"winccoa/tags/Pump1/speed":             KindNative,
		"winccoa/types/Pump/Pump1/speed":       KindNative,
		"winccoa/cns/View/a":                   KindCNS,
		"winccoa/systems/SubA":                 KindStatus,
		"winccoa/systems/System1":              KindStatus,
		"winccoa/systems/SubA/tags/Feeder1/v":  KindNative,
		"winccoa/systems/System1/tags/Pump1/v": KindNative,
		"winccoa/systems/SubA/cns/v":           KindCNS,
		"winccoa/systems/+/tags/#":             KindOther,
		"winccoa/#":                            KindOther,
		"winccoa/+/Pump1":                      KindOther,
		"winccoa/System1/tags/Pump1/speed":     KindNative, // parse error: no such level
		"winccoa/systems":                      KindNative, // parse error: missing system
		"winccoax/tags/Pump1":                  KindOther,
	} {
		if got := n.Classify(topic); got != want {
			t.Errorf("%s: got %v want %v", topic, got, want)
		}
	}
	local, err := n.Parse("winccoa/tags/Pump1/speed")
	if err != nil || local.System != "" || local.DPE("System1") != "System1:Pump1.speed" {
		t.Fatalf("local %+v %v", local, err)
	}
	if local.Topic() != "winccoa/tags/Pump1/speed" {
		t.Fatalf("local round trip %q", local.Topic())
	}
	explicit, err := n.Parse("winccoa/systems/System1/tags/Pump1/speed")
	if err != nil || explicit.System != "System1" || explicit.Topic() != "winccoa/systems/System1/tags/Pump1/speed" {
		t.Fatalf("explicit local %+v %v", explicit, err)
	}
	remote, err := n.Parse("winccoa/systems/SubA/types/Feeder/Feeder1/voltage/set")
	if err != nil || remote.System != "SubA" || remote.TypeName != "Feeder" || !remote.Command {
		t.Fatalf("remote %+v %v", remote, err)
	}
	if remote.Topic() != "winccoa/systems/SubA/types/Feeder/Feeder1/voltage/set" {
		t.Fatalf("remote round trip %q", remote.Topic())
	}
	for _, bad := range []string{"winccoa/System1/tags/Pump1/speed", "winccoa/systems/SubA", "winccoa/systems//tags/X"} {
		if _, err := n.Parse(bad); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
	w, err := n.ParseWildcard("winccoa/tags/Pump1/#")
	if err != nil || w.System != "" {
		t.Fatalf("local wildcard %+v %v", w, err)
	}
	if rt, ok := w.RowTarget("System1:Pump1.speed", ""); !ok || rt.Topic() != "winccoa/tags/Pump1/speed" {
		t.Fatalf("local row topic %q", rt.Topic())
	}
	we, _ := n.ParseWildcard("winccoa/systems/System1/tags/Pump1/#")
	if we.Query() != w.Query() || we.Key() == w.Key() {
		t.Fatal("shortcut and explicit filter must share the query but not the publishing form")
	}
	if n.StatusTopic("") != "winccoa" || n.StatusTopic("SubA") != "winccoa/systems/SubA" {
		t.Fatalf("status topics %q %q", n.StatusTopic(""), n.StatusTopic("SubA"))
	}
	// The canonical form of a shortcut topic is the explicit one.
	n.Local = "System1"
	for alias, want := range map[string]string{
		"winccoa/tags/Pump1/speed":                    "winccoa/systems/System1/tags/Pump1/speed",
		"winccoa/types/Pump/Pump1/speed":              "winccoa/systems/System1/tags/Pump1/speed",
		"winccoa/systems/System1/types/P/Pump1/speed": "winccoa/systems/System1/tags/Pump1/speed",
	} {
		if c, ok := n.CanonicalOf(alias); !ok || c != want {
			t.Errorf("canonical of %s: %q", alias, c)
		}
	}

	// Without the shortcut only the explicit form exists.
	n = DefaultNames
	n.NoShortcut = true
	if _, err := n.Parse("winccoa/tags/Pump1/speed"); err == nil {
		t.Error("shortcut accepted with NoShortcut")
	}
	if _, err := n.Parse("winccoa/systems/System1/tags/Pump1/speed"); err != nil {
		t.Errorf("explicit form: %v", err)
	}
	if n.Classify("winccoa") == KindStatus || n.Classify("winccoa/cns/x") == KindCNS {
		t.Error("shortcut status/cns classified with NoShortcut")
	}
}

func TestProtectedStoreDatapoints(t *testing.T) {
	for _, dp := range []string{"_Users", "MMQConfigs_k1", "MMQSessions_k1", "MMQRetained_k1", "MMQUsers_k1"} {
		if !Protected(dp) {
			t.Errorf("%s not protected", dp)
		}
	}
	if Protected("Pump1") {
		t.Error("Pump1 protected")
	}
}

func TestTopicsBranch(t *testing.T) {
	n := DefaultNames
	for topic, want := range map[string]Kind{
		"winccoa/topics/a/b":                 KindTopics,
		"winccoa/topics":                     KindTopics,
		"winccoa/topics/#":                   KindTopics,
		"winccoa/systems/System1/topics/a":   KindTopics,
		"winccoa/systems/System1/topics":     KindTopics,
		"winccoa/systems/System1/topicsx/a":  KindNative,
		"winccoa/systems/+/topics/a":         KindOther,
		"winccoa/systems/System1/tags/topic": KindNative,
	} {
		if got := n.Classify(topic); got != want {
			t.Errorf("%s: got %v want %v", topic, got, want)
		}
	}
	tt, err := n.ParseTopic("winccoa/systems/System2/topics/plant/line 1//x")
	if err != nil || tt.System != "System2" || tt.Topic != "plant/line 1//x" {
		t.Fatalf("parsed %+v %v", tt, err)
	}
	if got := tt.MQTTTopic(); got != "winccoa/systems/System2/topics/plant/line 1//x" {
		t.Fatalf("round trip %q", got)
	}
	tt, err = n.ParseTopic("winccoa/topics/a")
	if err != nil || tt.System != "" || tt.Topic != "a" {
		t.Fatalf("shortcut %+v %v", tt, err)
	}
	for _, bad := range []string{"winccoa/topics", "winccoa/topics/", "winccoa/systems/System1/topics", "winccoa/topics/a/#", "winccoa/topics/+"} {
		if _, err := n.ParseTopic(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	n.Local = "System1"
	if c, ok := n.CanonicalOf("winccoa/topics/a/b"); !ok || c != "winccoa/systems/System1/topics/a/b" {
		t.Fatalf("canonical %q %v", c, ok)
	}
	if TopicDP("a/b") == TopicDP("a/c") || !strings.HasPrefix(TopicDP("a/b"), "MMQTopic_k") || len(TopicDP("a/b")) != len("MMQTopic_k")+24 {
		t.Fatalf("dp name %q", TopicDP("a/b"))
	}
	if !Protected(TopicDP("a")) {
		t.Fatal("topic datapoint exposed as tag")
	}
	nt := Names{Root: "oa", Tags: "t", Types: "y", Systems: "s", Topics: "t"}
	if nt.Validate() == nil {
		t.Fatal("equal tags and topics names accepted")
	}
	ns := DefaultNames
	ns.NoShortcut = true
	if ns.Classify("winccoa/topics/a") == KindTopics {
		t.Fatal("topics shortcut classified without LocalShortcut")
	}
}

package winccoanative

import "testing"

func TestWildcardQueries(t *testing.T) {
	cases := []struct {
		filter, query string
		root          bool
	}{
		{"winccoa/systems/System1/tags/#", `SELECT '_online.._value', '_online.._stime' FROM '*.**'`, true},
		{"winccoa/systems/System1/tags/Pump1/#", `SELECT '_online.._value', '_online.._stime' FROM 'Pump1.**'`, false},
		{"winccoa/systems/System1/tags/Pump1/value/#", `SELECT '_online.._value', '_online.._stime' FROM '{Pump1.value,Pump1.value.**}'`, false},
		{"winccoa/systems/System1/tags/+/speed", `SELECT '_online.._value', '_online.._stime' FROM '*.speed'`, true},
		{"winccoa/systems/System1/tags/+", `SELECT '_online.._value', '_online.._stime' FROM '*.'`, true},
		{"winccoa/systems/System1/tags/Pump1/+/a", `SELECT '_online.._value', '_online.._stime' FROM 'Pump1.*.a'`, false},
		{"winccoa/systems/System1/types/Pump/#", `SELECT '_online.._value', '_online.._stime' FROM '*.**' WHERE _DPT = "Pump"`, false},
		{"winccoa/systems/System1/types/Pump/+/value/#", `SELECT '_online.._value', '_online.._stime' FROM '{*.value,*.value.**}' WHERE _DPT = "Pump"`, false},
		{"winccoa/systems/System1/types/#", `SELECT '_online.._value', '_online.._stime' FROM '*.**'`, true},
		{"winccoa/systems/System1/types/+/+/speed", `SELECT '_online.._value', '_online.._stime' FROM '*.speed'`, true},
		{"winccoa/systems/SubA/tags/Feeder1/#", `SELECT '_online.._value', '_online.._stime' FROM 'Feeder1.**' REMOTE 'SubA'`, false},
		// REMOTE comes directly after FROM, before WHERE (WinCC OA syntax).
		{"winccoa/systems/SubA/types/Pump/#", `SELECT '_online.._value', '_online.._stime' FROM '*.**' REMOTE 'SubA' WHERE _DPT = "Pump"`, false},
	}
	for _, c := range cases {
		w, err := DefaultNames.ParseWildcard(c.filter)
		if err != nil {
			t.Fatalf("%s: %v", c.filter, err)
		}
		w.Remote = w.System != "System1" // bound by the service
		if w.Query() != c.query {
			t.Errorf("%s:\n got %s\nwant %s", c.filter, w.Query(), c.query)
		}
		if w.IsRoot() != c.root {
			t.Errorf("%s: root=%v", c.filter, w.IsRoot())
		}
	}
	for _, bad := range []string{
		"winccoa/systems/System1/tags/Pump1/#/x", "winccoa/systems/Sys%41/tags/#",
		"winccoa/systems/System1/tags/_Users/#", "winccoa/systems/System1/tags/Pump1/+/_online.._stime",
		"winccoa/systems/System1/tags/Pu*mp/#", "winccoa/systems/System1/#", "winccoa/systems/System1/tags/Pump1/+/set",
	} {
		if _, err := DefaultNames.ParseWildcard(bad); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
	w, _ := DefaultNames.ParseWildcard("winccoa/systems/System1/types/Pump/#")
	if tg, ok := w.RowTarget("System1:Pump7.value.speed", "Pump"); !ok || tg.Topic() != "winccoa/systems/System1/types/Pump/Pump7/value/speed" {
		t.Fatalf("row topic %v %q", ok, tg.Topic())
	}
	w, _ = DefaultNames.ParseWildcard("winccoa/systems/System1/tags/#")
	if tg, ok := w.RowTarget("System1:Scalar.", ""); !ok || tg.Topic() != "winccoa/systems/System1/tags/Scalar" {
		t.Fatalf("scalar row %q", tg.Topic())
	}
	if _, ok := w.RowTarget("System1:_Users.x", ""); ok {
		t.Fatal("internal datapoint published")
	}
}

func TestTopicDirQuery(t *testing.T) {
	local := (&topicDir{system: "System1"}).query("System1")
	if want := `SELECT '_online.._value' FROM 'MMQTopic_*.topic' WHERE _DPT = "MMQTopic"`; local != want {
		t.Errorf("local:\n got %s\nwant %s", local, want)
	}
	remote := (&topicDir{system: "Vienna"}).query("System1")
	if want := `SELECT '_online.._value' FROM 'MMQTopic_*.topic' REMOTE 'Vienna' WHERE _DPT = "MMQTopic"`; remote != want {
		t.Errorf("remote:\n got %s\nwant %s", remote, want)
	}
}

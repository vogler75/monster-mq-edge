package winccoanative

import "testing"

func TestWildcardQueries(t *testing.T) {
	cases := []struct {
		filter, query string
		root          bool
	}{
		{"winccoa/local/tags/#", `SELECT '_online.._value', '_online.._stime' FROM '*.**'`, true},
		{"winccoa/local/tags/Pump1/#", `SELECT '_online.._value', '_online.._stime' FROM 'Pump1.**'`, false},
		{"winccoa/local/tags/Pump1/value/#", `SELECT '_online.._value', '_online.._stime' FROM '{Pump1.value,Pump1.value.**}'`, false},
		{"winccoa/local/tags/+/speed", `SELECT '_online.._value', '_online.._stime' FROM '*.speed'`, true},
		{"winccoa/local/tags/+", `SELECT '_online.._value', '_online.._stime' FROM '*.'`, true},
		{"winccoa/local/tags/Pump1/+/a", `SELECT '_online.._value', '_online.._stime' FROM 'Pump1.*.a'`, false},
		{"winccoa/local/types/Pump/#", `SELECT '_online.._value', '_online.._stime' FROM '*.**' WHERE _DPT = "Pump"`, false},
		{"winccoa/local/types/Pump/+/value/#", `SELECT '_online.._value', '_online.._stime' FROM '{*.value,*.value.**}' WHERE _DPT = "Pump"`, false},
		{"winccoa/local/types/#", `SELECT '_online.._value', '_online.._stime' FROM '*.**'`, true},
		{"winccoa/local/types/+/+/speed", `SELECT '_online.._value', '_online.._stime' FROM '*.speed'`, true},
		{"winccoa/remote/SubA/tags/Feeder1/#", `SELECT '_online.._value', '_online.._stime' FROM 'Feeder1.**' REMOTE 'SubA'`, false},
	}
	for _, c := range cases {
		w, err := ParseWildcard(c.filter)
		if err != nil {
			t.Fatalf("%s: %v", c.filter, err)
		}
		if w.Query() != c.query {
			t.Errorf("%s:\n got %s\nwant %s", c.filter, w.Query(), c.query)
		}
		if w.IsRoot() != c.root {
			t.Errorf("%s: root=%v", c.filter, w.IsRoot())
		}
	}
	for _, bad := range []string{
		"winccoa/local/tags/Pump1/#/x", "winccoa/remote/+/tags/#", "winccoa/remote/#",
		"winccoa/local/tags/_Users/#", "winccoa/local/tags/Pump1/+/_online.._stime",
		"winccoa/local/tags/Pu*mp/#", "winccoa/local/#", "winccoa/local/tags/Pump1/+/set",
	} {
		if _, err := ParseWildcard(bad); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
	w, _ := ParseWildcard("winccoa/local/types/Pump/#")
	if tg, ok := w.RowTarget("System1:Pump7.value.speed", "Pump"); !ok || tg.Topic() != "winccoa/local/types/Pump/Pump7/value/speed" {
		t.Fatalf("row topic %v %q", ok, tg.Topic())
	}
	w, _ = ParseWildcard("winccoa/local/tags/#")
	if tg, ok := w.RowTarget("System1:Scalar.", ""); !ok || tg.Topic() != "winccoa/local/tags/Scalar" {
		t.Fatalf("scalar row %q", tg.Topic())
	}
	if _, ok := w.RowTarget("System1:_Users.x", ""); ok {
		t.Fatal("internal datapoint published")
	}
}

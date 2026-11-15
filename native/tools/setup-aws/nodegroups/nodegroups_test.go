package nodegroups

import (
	"slices"
	"strings"
	"testing"
)

// Every row of the rule: which groups a selection's sandbox groups make,
// with and without Auto Mode, and that For's answer always passes Check.
func TestForEveryRow(t *testing.T) {
	for _, c := range []struct {
		name    string
		auto    bool
		sandbox []string
		want    []string
	}{
		{"default: Auto Mode, gvisor and microvm", true, []string{GVisor, MicroVM}, []string{System, GVisor, MicroVM}},
		{"Auto Mode, gvisor", true, []string{GVisor}, []string{System, GVisor}},
		{"Auto Mode, microvm", true, []string{MicroVM}, []string{System, MicroVM}},
		{"Auto Mode, no sandbox group", true, nil, nil},
		{"no Auto Mode, gvisor", false, []string{GVisor}, []string{System, GVisor}},
		{"no Auto Mode, both", false, []string{MicroVM, GVisor}, []string{System, GVisor, MicroVM}},
		// Without Auto Mode the system group is always made; Check then
		// refuses the missing sandbox group.
		{"no Auto Mode, no sandbox group", false, nil, []string{System}},
	} {
		got := For(c.auto, c.sandbox)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: For = %v, want %v", c.name, got, c.want)
		}
		if err := Check(c.auto, got); (err == nil) != (c.auto || len(c.sandbox) > 0) {
			t.Errorf("%s: Check(%v) = %v", c.name, got, err)
		}
	}
}

func TestCheckRefusals(t *testing.T) {
	for _, c := range []struct {
		auto   bool
		groups []string
		want   string
	}{
		{false, []string{System}, "needs the gvisor or microvm node group"},
		{false, nil, "needs the gvisor or microvm node group"},
		{false, []string{GVisor}, "needs the system node group"},
		{true, []string{MicroVM}, "need the system node group too"},
		{true, []string{System}, "comes only with the gvisor or microvm node group"},
		{true, []string{System, "kata"}, `unknown node group "kata"`},
		{true, []string{System, GVisor, GVisor}, "named twice"},
	} {
		err := Check(c.auto, c.groups)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Check(%v, %v) = %v, want %q", c.auto, c.groups, err, c.want)
		}
	}
	for _, ok := range [][]string{{}, {System, GVisor}, {System, MicroVM}, {System, GVisor, MicroVM}} {
		if err := Check(true, ok); err != nil {
			t.Errorf("Check(true, %v) = %v", ok, err)
		}
	}
}

func TestParseAndFormat(t *testing.T) {
	if g, err := Parse(None); err != nil || g == nil || len(g) != 0 {
		t.Errorf("Parse(none) = %v, %v; want an empty list", g, err)
	}
	if g, err := Parse(" system, gvisor "); err != nil || !slices.Equal(g, []string{System, GVisor}) {
		t.Errorf("Parse = %v, %v", g, err)
	}
	if _, err := Parse(""); err == nil {
		t.Error("Parse accepted an empty value, which would read as no group")
	}
	if Format(nil) != None || Format([]string{System, GVisor}) != "system,gvisor" || Default != "system,gvisor,microvm" {
		t.Errorf("Format: %q %q %q", Format(nil), Format([]string{System, GVisor}), Default)
	}
}

package nodegroups

import (
	"slices"
	"strings"
	"testing"
)

// Every row of the rule: which groups a selection makes from its Auto
// Mode, database, sandbox groups and whether a chosen plugin is pinned to
// the system group, and that For's answer always passes Check.
func TestForEveryRow(t *testing.T) {
	const rds, bundled, pinned, unpinned = true, false, true, false
	for _, c := range []struct {
		name      string
		auto, rds bool
		pinned    bool
		sandbox   []string
		want      []string
	}{
		// Auto Mode off: the system group always (required).
		{"no Auto Mode, RDS, gvisor", false, rds, unpinned, []string{GVisor}, []string{System, GVisor}},
		{"no Auto Mode, bundled, gvisor", false, bundled, unpinned, []string{GVisor}, []string{System, GVisor}},
		{"no Auto Mode, bundled, both, pinned", false, bundled, pinned, []string{MicroVM, GVisor}, []string{System, GVisor, MicroVM}},
		// Without Auto Mode the system group is always made; Check then
		// refuses the missing sandbox group.
		{"no Auto Mode, no sandbox group", false, bundled, unpinned, nil, []string{System}},
		// Auto Mode and RDS with a sandbox group: the control plane is
		// cordoned onto the system group (the default, unchanged).
		{"default: Auto Mode, RDS, gvisor and microvm", true, rds, unpinned, []string{GVisor, MicroVM}, []string{System, GVisor, MicroVM}},
		{"Auto Mode, RDS, gvisor", true, rds, unpinned, []string{GVisor}, []string{System, GVisor}},
		{"Auto Mode, RDS, microvm", true, rds, unpinned, []string{MicroVM}, []string{System, MicroVM}},
		// Auto Mode and the bundled PostgreSQL, nothing pinned: no system
		// group (the change).
		{"Auto Mode, bundled, gvisor", true, bundled, unpinned, []string{GVisor}, []string{GVisor}},
		{"Auto Mode, bundled, microvm", true, bundled, unpinned, []string{MicroVM}, []string{MicroVM}},
		{"Auto Mode, bundled, both", true, bundled, unpinned, []string{MicroVM, GVisor}, []string{GVisor, MicroVM}},
		// Auto Mode, no sandbox group, nothing pinned: no group at all (the
		// minimal profile, unchanged).
		{"Auto Mode, RDS, no sandbox group", true, rds, unpinned, nil, nil},
		{"Auto Mode, bundled, no sandbox group", true, bundled, unpinned, nil, nil},
		// A plugin pinned to the system group makes it.
		{"Auto Mode, bundled, gvisor, pinned", true, bundled, pinned, []string{GVisor}, []string{System, GVisor}},
		{"Auto Mode, bundled, no sandbox group, pinned", true, bundled, pinned, nil, []string{System}},
		{"Auto Mode, RDS, no sandbox group, pinned", true, rds, pinned, nil, []string{System}},
		// The nitro group (NITRO_COMPUTE=node-group) is a sandbox group like
		// the other two: with Auto Mode and RDS it pins the control plane to
		// the system group, without Auto Mode it needs the system group.
		{"Auto Mode, RDS, nitro", true, rds, unpinned, []string{Nitro}, []string{System, Nitro}},
		{"Auto Mode, bundled, nitro", true, bundled, unpinned, []string{Nitro}, []string{Nitro}},
		{"no Auto Mode, bundled, nitro", false, bundled, unpinned, []string{Nitro}, []string{System, Nitro}},
		{"no Auto Mode, RDS, all three sandboxes", false, rds, unpinned, []string{Nitro, MicroVM, GVisor}, []string{System, GVisor, MicroVM, Nitro}},
	} {
		got := For(c.auto, c.rds, c.pinned, c.sandbox)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: For = %v, want %v", c.name, got, c.want)
		}
		if err := Check(c.auto, got); (err == nil) != (c.auto || len(c.sandbox) > 0) {
			t.Errorf("%s: Check(%v) = %v", c.name, got, err)
		}
	}
}

// Check refuses, without Auto Mode, a list missing the system group or a
// sandbox group, and any unknown or repeated name; with Auto Mode it
// accepts every list of known groups, a sandbox group without the system
// group and the system group alone among them.
func TestCheckRefusals(t *testing.T) {
	for _, c := range []struct {
		auto   bool
		groups []string
		want   string
	}{
		{false, []string{System}, "needs the gvisor, microvm or nitro node group"},
		{false, nil, "needs the gvisor, microvm or nitro node group"},
		{false, []string{Nitro}, "needs the system node group"},
		{true, []string{System, Nitro, Nitro}, "named twice"},
		{false, []string{GVisor}, "needs the system node group"},
		{false, []string{GVisor, MicroVM}, "needs the system node group"},
		{true, []string{System, "kata"}, `unknown node group "kata"`},
		{true, []string{System, GVisor, GVisor}, "named twice"},
	} {
		err := Check(c.auto, c.groups)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Check(%v, %v) = %v, want %q", c.auto, c.groups, err, c.want)
		}
	}
	for _, ok := range [][]string{{}, {GVisor}, {MicroVM}, {GVisor, MicroVM}, {System},
		{System, GVisor}, {System, MicroVM}, {System, GVisor, MicroVM}} {
		if err := Check(true, ok); err != nil {
			t.Errorf("Check(true, %v) = %v", ok, err)
		}
	}
	for _, ok := range [][]string{{System, GVisor}, {System, MicroVM}, {System, GVisor, MicroVM}, {System, Nitro}, {Nitro}} {
		if err := Check(true, ok); err != nil {
			t.Errorf("Check(true, %v) = %v", ok, err)
		}
	}
	for _, ok := range [][]string{{System, GVisor}, {System, MicroVM}, {System, GVisor, MicroVM}, {System, Nitro}, {System, GVisor, MicroVM, Nitro}} {
		if err := Check(false, ok); err != nil {
			t.Errorf("Check(false, %v) = %v", ok, err)
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

// CheckType takes the two types as setup-aws spells them, and nothing else.
func TestCheckType(t *testing.T) {
	for typ, ok := range map[string]bool{Managed: true, SelfManaged: true, "": false, "Managed": false, "self_managed": false, "fargate": false} {
		if err := CheckType(typ); (err == nil) != ok {
			t.Errorf("CheckType(%q) = %v, want ok=%v", typ, err, ok)
		}
	}
	if Default != "system,gvisor,microvm" || Types[0] != Managed {
		t.Errorf("Default %q, Types %q", Default, Types)
	}
}

// The default stays the three groups it was before the nitro group: All
// grew, NODE_GROUPS's default did not, and For makes nitro only when asked.
func TestDefaultLeavesOutNitro(t *testing.T) {
	if Default != "system,gvisor,microvm" || !slices.Equal(All, []string{System, GVisor, MicroVM, Nitro}) {
		t.Errorf("Default %q, All %q", Default, All)
	}
	if g, err := Parse(Default); err != nil || slices.Contains(g, Nitro) {
		t.Errorf("Parse(Default) = %v, %v", g, err)
	}
	if g := For(true, true, false, []string{GVisor, MicroVM}); slices.Contains(g, Nitro) {
		t.Errorf("For made %v without the nitro group asked for", g)
	}
}

// Both directions: whole counts, with a decimal suffix too; anything
// fractional, negative or not a number counts as none.
func TestSandboxCount(t *testing.T) {
	for q, want := range map[string]int64{"0": 0, "1": 1, "16": 16, "1k": 1000, "2M": 2000000} {
		if n, ok := SandboxCount(q); !ok || n != want {
			t.Errorf("SandboxCount(%q) = %d, %v; want %d", q, n, ok, want)
		}
	}
	for _, q := range []string{"", "500m", "-1", "1.5", "x", "1Ki", "9223372036854775807k"} {
		if n, ok := SandboxCount(q); ok || n != 0 {
			t.Errorf("SandboxCount(%q) = %d, %v; want none", q, n, ok)
		}
	}
	if b := NitroAMIBuild("1.37"); !strings.Contains(b, "make k8s=1.37 os_distro=al2023 enable_pod_isolation=true") || !strings.Contains(b, "awslabs/amazon-eks-ami") {
		t.Errorf("NitroAMIBuild = %q", b)
	}
}

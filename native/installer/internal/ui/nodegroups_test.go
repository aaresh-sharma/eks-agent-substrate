package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// groupRows are the node groups Settings has rows for.
func groupRows(m Model) []string {
	var out []string
	for _, f := range m.set.fields {
		if f.group != "" && !slices.Contains(out, f.group) {
			out = append(out, f.group)
		}
	}
	return out
}

// Settings shows rows only for the node groups the new cluster gets, and
// they follow the classes and Auto Mode as they are toggled: the default
// three; gVisor alone, system and gvisor; the AWS Nitro sandbox alone on
// Auto Mode, none, with the line that says so.
func TestSettingsShowsOnlyTheNodeGroupsTheClusterGets(t *testing.T) {
	m := toSettings(t, testDeps(&recorder{}), newSetup("fresh"))
	if got := groupRows(m); !slices.Equal(got, []string{"system", "gvisor", "microvm"}) {
		t.Fatalf("default rows %v", got)
	}
	m = cursorTo(t, m, state.ClassMicroVM)
	m = press(m, "enter")
	if got := groupRows(m); !slices.Equal(got, []string{"system", "gvisor"}) || strings.Contains(m.View(), "MICROVM") || strings.Contains(m.View(), "nested virt") {
		t.Errorf("gVisor alone: rows %v\n%s", got, m.View())
	}
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter")
	m = cursorTo(t, m, state.ClassGVisor)
	m = press(m, "enter")
	if got := groupRows(m); len(got) != 0 || !strings.Contains(m.View(), noGroupsLine) {
		t.Errorf("Nitro alone: rows %v\n%s", got, m.View())
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || m.s.SandboxClasses != "nitro" || len(m.s.NodeGroups()) != 0 {
		t.Errorf("at %s, classes %q, groups %v", m.m.Current().Title(), m.s.SandboxClasses, m.s.NodeGroups())
	}
}

// m sets the minimal profile: Auto Mode on, the AWS Nitro sandbox alone,
// the bundled PostgreSQL; the run then makes no node group and has no RDS
// step, and Review says everything runs on Auto Mode nodes. Its cost is the
// control plane, the NAT gateway and the class's Auto Mode node, with no
// node group; the default's is unchanged (TestReviewShowsThePlanTimesAndCost).
func TestMinimalProfileMakesNoNodeGroup(t *testing.T) {
	r := &recorder{}
	s := newSetup("fresh")
	m := toSettings(t, testDeps(r), s)
	m = cursorTo(t, m, "AUTO_MODE")
	m = press(m, "enter") // off: m turns it back on
	m = press(m, "m")
	if v := m.set.values; v["AUTO_MODE"] != "on" || v[classKey] != "nitro" || v[dbKey] != state.DatabaseBundled {
		t.Fatalf("minimal values %v", v)
	}
	m = press(m, "n", "enter")
	if m.m.Current() != state.Review {
		t.Fatalf("at %s", m.m.Current().Title())
	}
	v := m.View()
	for _, want := range []string{"No node groups: everything runs on EKS Auto Mode nodes", "classes nitro", "$0.15", "$0.12"} {
		if !strings.Contains(v, want) {
			t.Errorf("review lacks %q:\n%s", want, v)
		}
	}
	if slices.Contains(m.m.Order(), state.RDS) || !slices.Contains(m.m.Order(), state.NitroSandbox) {
		t.Errorf("order %v", m.m.Order())
	}
	if usd, _ := clusterHourly(s.NewClusterConfig, nil); usd != eksHourly+natHourly {
		t.Errorf("no node group: $%.4f", usd)
	}
}

// The Plugins screen refuses, with the reason, a plugin pinned to a node
// group the run does not make, and drops a preselected one: on the minimal
// profile the default gateway runs on the system group, which comes only
// with a gVisor or microVM group, so it is dropped; choosing t-kata, which
// adds the microvm group (and so the system group), makes it available
// again. t-claw's gvisor group needs the gVisor class.
func TestPluginsScreenRefusesAPluginWhoseNodesTheRunDoesNotMake(t *testing.T) {
	s := newSetup("fresh")
	s.Plugins = nil // the defaults
	m := toSettings(t, testDeps(&recorder{}), s)
	m = press(m, "m", "n")
	if v := m.View(); m.m.Current() != state.Plugins || m.pl.chosen["t-gateway"] || !strings.Contains(v, "not selected: t-gateway") ||
		!strings.Contains(v, "Not available: runs on the system node group") {
		t.Fatalf("the default gateway kept on the minimal profile:\n%s", v)
	}
	at := func(id string) Model {
		for i, p := range choosable() {
			if p.ID == id {
				m.pl.cursor = i
			}
		}
		return m
	}
	m = press(at("t-gateway"), " ")
	if m.pl.chosen["t-gateway"] || !strings.Contains(m.notice, "an Auto Mode cluster gets only with the gVisor or microVM sandbox class") {
		t.Errorf("gateway chosen without the system group:\n%s", m.View())
	}
	m = press(at("t-kata"), " ")
	m = press(at("t-gateway"), " ")
	if !m.pl.chosen["t-kata"] || !m.pl.chosen["t-gateway"] {
		t.Errorf("with t-kata's microvm group the gateway is still refused: %v\n%s", m.pl.chosen, m.View())
	}
	// Clearing t-kata takes the system group away, and the gateway with it.
	m = press(at("t-kata"), " ")
	if m.pl.chosen["t-gateway"] {
		t.Errorf("gateway kept after its node group went: %v", m.pl.chosen)
	}
	m = press(at("t-claw"), " ")
	if m.pl.chosen["t-claw"] || m.notice != "Claw agent runs on the gvisor node group: turn on its sandbox class in Settings" {
		t.Errorf("t-claw chosen without the gvisor group:\n%s", m.View())
	}
	m = press(m, "enter")
	if m.m.Current() != state.Review || len(s.Plugins) != 0 || len(s.NodeGroups()) != 0 {
		t.Errorf("at %s, plugins %v, groups %v", m.m.Current().Title(), s.Plugins, s.NodeGroups())
	}
}

// A connected cluster: no node group is made, its own (the probe's) decide.
// A class whose node group it lacks is refused in Settings, naming it, and
// a plugin pinned to such a group is refused on the Plugins screen.
func TestConnectedClusterRefusesWhatItsNodesLack(t *testing.T) {
	r := &recorder{probes: map[string]string{"new": "SUBSTRATE_EKS_INSTALLED false pools=system,microvm"}}
	s := newSetup("new")
	m := toStep(t, New(testDeps(r), s), state.Settings)
	if !slices.Equal(s.ClusterPools, []string{"microvm", "system"}) || !strings.Contains(m.View(), "node groups: system, microvm") || len(groupRows(m)) != 0 {
		t.Fatalf("pools %v:\n%s", s.ClusterPools, m.View())
	}
	// gvisor is on by default: n names the missing group.
	m = press(m, "n")
	if m.m.Current() != state.Settings || m.set.errs[classKey] != "gVisor: no gvisor node group" {
		t.Fatalf("at %s, errs %v", m.m.Current().Title(), m.set.errs)
	}
	m = cursorTo(t, m, state.ClassGVisor)
	m = press(m, "enter", "enter") // off, and refused on again
	if m.set.classOn(state.ClassGVisor) || !strings.Contains(m.View(), "Not available: new has") || m.notice != "gVisor new has no gvisor node group (no node labeled ate.dev/pool=gvisor)" {
		t.Errorf("gvisor on without its nodes:\n%s", m.View())
	}
	m = press(m, "n")
	for i, p := range choosable() {
		if p.ID == "t-claw" {
			m.pl.cursor = i
		}
	}
	m = press(m, " ")
	if m.m.Current() != state.Plugins || m.pl.chosen["t-claw"] || m.notice != "Claw agent runs on the gvisor node group, which new does not have" {
		t.Errorf("t-claw on a cluster without gvisor nodes:\n%s", m.View())
	}
}

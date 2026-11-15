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
// they follow the classes, the database and Auto Mode as they are toggled:
// the default three; gVisor alone, system and gvisor with RDS and gvisor
// alone with the bundled PostgreSQL; the AWS Nitro sandbox alone on Auto
// Mode, none, with the line that says so.
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
	// The bundled PostgreSQL takes the system row away on Auto Mode; RDS
	// brings it back.
	m = cursorTo(t, m, dbKey)
	m = press(m, "enter")
	if got := groupRows(m); !slices.Equal(got, []string{"gvisor"}) {
		t.Errorf("gVisor alone, bundled: rows %v\n%s", got, m.View())
	}
	m = press(m, "enter")
	if got := groupRows(m); !slices.Equal(got, []string{"system", "gvisor"}) {
		t.Errorf("gVisor alone, RDS again: rows %v\n%s", got, m.View())
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

// pluginAt puts the Plugins screen's cursor on id.
func pluginAt(m Model, id string) Model {
	for i, p := range choosable() {
		if p.ID == id {
			m.pl.cursor = i
		}
	}
	return m
}

// The Plugins screen refuses, with the reason, a plugin pinned to a sandbox
// group whose class is off (t-claw on gvisor), and never one on the system
// group: choosing it makes that group. On the minimal profile the default
// gateway would add the system group, so it is not preselected, and the
// screen says so; chosen, it makes the system group, and stays when t-kata
// (the microvm group) comes and goes. Review then shows the system group.
func TestPluginsScreenRefusesAPluginWhoseNodesTheRunDoesNotMake(t *testing.T) {
	s := newSetup("fresh")
	s.Plugins = nil // the defaults
	m := toSettings(t, testDeps(&recorder{}), s)
	m = press(m, "m", "n")
	if v := m.View(); m.m.Current() != state.Plugins || m.pl.chosen["t-gateway"] || m.notice != "not preselected: t-gateway (adds the system node group)" ||
		!strings.Contains(v, "Adds the system node group to the new cluster") || strings.Contains(v, "Not available") {
		t.Fatalf("the default gateway on the minimal profile: notice %q\n%s", m.notice, v)
	}
	m = press(pluginAt(m, "t-gateway"), " ")
	if !m.pl.chosen["t-gateway"] {
		t.Errorf("gateway refused on the minimal profile:\n%s", m.View())
	}
	m = press(pluginAt(m, "t-kata"), " ")
	m = press(pluginAt(m, "t-kata"), " ")
	if m.pl.chosen["t-kata"] || !m.pl.chosen["t-gateway"] {
		t.Errorf("t-kata on and off: %v", m.pl.chosen)
	}
	m = press(pluginAt(m, "t-claw"), " ")
	if m.pl.chosen["t-claw"] || m.notice != "Claw agent runs on the gvisor node group: turn on its sandbox class in Settings" {
		t.Errorf("t-claw chosen without the gvisor group:\n%s", m.View())
	}
	m = press(m, "enter")
	if m.m.Current() != state.Review || !slices.Equal(s.Plugins, []string{"t-gateway"}) || !slices.Equal(s.NodeGroups(), []string{"system"}) ||
		!strings.Contains(m.View(), "Node groups: system 2×m8i.xlarge") {
		t.Errorf("at %s, plugins %v, groups %v\n%s", m.m.Current().Title(), s.Plugins, s.NodeGroups(), m.View())
	}
}

// The default gateway is preselected where the run makes the system group
// anyway (the default: Auto Mode, RDS, gvisor and microvm), and not with the
// bundled PostgreSQL and the gVisor class, which then make only the gvisor
// group; Review shows exactly that group and its cost.
func TestDefaultGatewayIsPreselectedOnlyWhereItAddsNoNodeGroup(t *testing.T) {
	s := newSetup("fresh")
	s.Plugins = nil
	m := press(toSettings(t, testDeps(&recorder{}), s), "n")
	if m.m.Current() != state.Plugins || !m.pl.chosen["t-gateway"] || strings.Contains(m.notice, "not preselected") ||
		strings.Contains(m.View(), "Adds the system node group") {
		t.Fatalf("default run, at %s: chosen %v, notice %q\n%s", m.m.Current().Title(), m.pl.chosen, m.notice, m.View())
	}

	s = newSetup("fresh")
	s.Plugins = nil
	m = toSettings(t, testDeps(&recorder{}), s)
	m = cursorTo(t, m, state.ClassMicroVM)
	m = press(m, "enter")
	m = cursorTo(t, m, dbKey)
	m = press(m, "enter")
	if got := groupRows(m); !slices.Equal(got, []string{"gvisor"}) || m.set.values[dbKey] != state.DatabaseBundled {
		t.Fatalf("bundled, gVisor: rows %v, database %q\n%s", got, m.set.values[dbKey], m.View())
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || m.pl.chosen["t-gateway"] || m.notice != "not preselected: t-gateway (adds the system node group)" {
		t.Fatalf("bundled, gVisor, at %s: chosen %v, notice %q\n%s", m.m.Current().Title(), m.pl.chosen, m.notice, m.View())
	}
	m = press(m, "enter")
	v := m.View()
	if m.m.Current() != state.Review || !slices.Equal(s.NodeGroups(), []string{"gvisor"}) || !strings.Contains(v, "Node groups: gvisor 2×m8i.2xlarge") ||
		strings.Contains(v, "system 2×") {
		t.Errorf("at %s, groups %v\n%s", m.m.Current().Title(), s.NodeGroups(), v)
	}
	if usd, _ := clusterHourly(s.NewClusterConfig, s.NodeGroups()); usd != eksHourly+natHourly+2*(ec2Price["m8i.2xlarge"]+200*gp3PerGBMonth/hoursPerMonth) {
		t.Errorf("bundled, gVisor: $%.4f an hour", usd)
	}

	// A connected cluster gets no node group from the run: its system
	// nodes are there, so the gateway is preselected, whatever the database
	// and classes (a new cluster with these would make system and gvisor,
	// not the three the cluster has).
	r := &recorder{probes: map[string]string{"new": "SUBSTRATE_EKS_INSTALLED false pools=system,gvisor,microvm"}}
	s = newSetup("new")
	s.Plugins, s.Database, s.SandboxClasses = nil, state.DatabaseBundled, state.ClassGVisor
	m = press(toStep(t, New(testDeps(r), s), state.Settings), "n")
	if m.m.Current() != state.Plugins || !m.pl.chosen["t-gateway"] || strings.Contains(m.notice, "not preselected") {
		t.Errorf("connected, at %s: chosen %v, notice %q", m.m.Current().Title(), m.pl.chosen, m.notice)
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

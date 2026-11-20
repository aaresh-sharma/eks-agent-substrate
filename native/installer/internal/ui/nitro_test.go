package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// hasField reports a Settings row for key.
func hasField(m Model, key string) bool {
	return slices.ContainsFunc(m.set.fields, func(f field) bool { return f.key == key })
}

// planHourly is the cost an hour of step st on Review's plan.
func planHourly(t *testing.T, m Model, st state.Step) float64 {
	t.Helper()
	for _, p := range m.plan() {
		if p.step == st {
			return p.hourly
		}
	}
	t.Fatalf("no %s in the plan", st.Title())
	return 0
}

// Settings, on a new cluster: the default has no nitro row; the nitro class
// adds "Nitro sandbox on" (auto-mode); node-group adds the Nitro AMI row,
// which n refuses empty or malformed, the nitro node group row, and the
// notes; a Nitro Sandbox AMI lets the run go on with the nitro group, and
// Review prices it with the cluster, not as Auto Mode nodes.
func TestSettingsNitroOnTheNodeGroup(t *testing.T) {
	s := newSetup("fresh")
	s.DemoMode = true
	m := toSettings(t, testDeps(&recorder{}), s)
	if hasField(m, "NITRO_COMPUTE") || hasField(m, "NITRO_AMI_ID") || strings.Contains(m.View(), "Nitro sandbox on  ") || slices.Contains(groupRows(m), "nitro") {
		t.Fatalf("the default has a nitro row:\n%s", m.View())
	}
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter")
	if !hasField(m, "NITRO_COMPUTE") || hasField(m, "NITRO_AMI_ID") || slices.Contains(groupRows(m), "nitro") ||
		!strings.Contains(m.View(), "on Auto Mode nodes") {
		t.Fatalf("nitro on Auto Mode's nodes:\n%s", m.View())
	}
	m = cursorTo(t, m, "NITRO_COMPUTE")
	m = press(m, "enter")
	v := m.View()
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || !hasField(m, "NITRO_AMI_ID") ||
		!slices.Equal(groupRows(m), []string{"system", "gvisor", "microvm", "nitro"}) || !strings.Contains(v, "Nitro Enclaves") ||
		!strings.Contains(v, "nitro node group") || !strings.Contains(v, "a Nitro Sandbox AMI is needed") {
		t.Fatalf("node-group: values %v, rows %v\n%s", m.set.values["NITRO_COMPUTE"], groupRows(m), v)
	}
	if !strings.Contains(v, "Now: NITRO_COMPUTE=node-") {
		t.Errorf("no panel value:\n%s", v)
	}
	m = press(m, "n")
	if m.m.Current() != state.Settings || !strings.HasPrefix(m.set.errs["NITRO_AMI_ID"], "a Nitro Sandbox AMI is needed") {
		t.Fatalf("n went on without a Nitro AMI: at %s, errs %v", m.m.Current().Title(), m.set.errs)
	}
	m = cursorTo(t, m, "NITRO_AMI_ID")
	m = setText(m, "ami-x")
	if !strings.Contains(m.set.errs["NITRO_AMI_ID"], "the ID of a Nitro Sandbox AMI") {
		t.Errorf("a malformed AMI: errs %v", m.set.errs)
	}
	m = setText(m, "ami-0a17000000000000e")
	if _, bad := m.set.errs["NITRO_AMI_ID"]; bad {
		t.Fatalf("errs %v", m.set.errs)
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || s.NewClusterConfig.NitroCompute != plugin.NitroNodeGroup || s.NewClusterConfig.NitroAMIID != "ami-0a17000000000000e" ||
		!slices.Equal(s.NodeGroups(), []string{"system", "gvisor", "microvm", "nitro"}) {
		t.Fatalf("at %s, %+v, groups %v", m.m.Current().Title(), s.NewClusterConfig, s.NodeGroups())
	}
	m = press(m, "enter")
	if m.m.Current() != state.Review || !strings.Contains(s.NewClusterConfig.GroupsLine(s.NodeGroups()), " · nitro 1×m8i.xlarge") ||
		!strings.Contains(m.View(), "Node groups: system 2×m8i.xlarge") {
		t.Fatalf("at %s:\n%s", m.m.Current().Title(), m.View())
	}
	// The cluster: the default's $2.3764, plus a m8i.xlarge and its 100 GB.
	if usd := planHourly(t, m, state.CreateCluster); usd < 2.598 || usd > 2.600 {
		t.Errorf("cluster $%.4f an hour", usd)
	}
	if usd := planHourly(t, m, state.NitroSandbox); usd != 0 {
		t.Errorf("Nitro sandbox step $%.2f an hour, want none (the nodes are the nitro group's)", usd)
	}
	// m, the minimal profile, is Auto Mode's: auto-mode, no nitro group.
	m = press(m, "esc", "esc")
	if m.m.Current() != state.Settings {
		t.Fatalf("at %s", m.m.Current().Title())
	}
	m = press(m, "m")
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || hasField(m, "NITRO_AMI_ID") || len(groupRows(m)) != 0 {
		t.Errorf("minimal: %v, rows %v", m.set.values["NITRO_COMPUTE"], groupRows(m))
	}
}

// Without Auto Mode, a new cluster shows where the nitro class would run:
// on auto-mode the class is refused as before; on node-group it goes on,
// with the system and nitro groups the rule requires, and the install runs
// the class's step, whose subtitle names the nitro node group. The Nitro
// AMI panel's build command is for the Kubernetes version Settings has.
func TestSettingsNitroWithoutAutoMode(t *testing.T) {
	r := &recorder{}
	s := newSetup("fresh")
	s.SandboxClasses = state.ClassGVisor
	m := toSettings(t, testDeps(r), s)
	m = cursorTo(t, m, "AUTO_MODE")
	m = press(m, "enter")
	if !hasField(m, "NITRO_COMPUTE") || hasField(m, "NITRO_AMI_ID") {
		t.Fatalf("Auto Mode off: no Nitro sandbox on row:\n%s", m.View())
	}
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter")
	if m.set.classOn(state.ClassNitro) || !strings.HasPrefix(m.notice, "AWS Nitro sandbox needs EKS Auto Mode") || !strings.Contains(m.notice, "NITRO_COMPUTE=node-group") {
		t.Fatalf("auto-mode without Auto Mode: on %v, notice %q", m.set.classOn(state.ClassNitro), m.notice)
	}
	m = cursorTo(t, m, "NITRO_COMPUTE")
	m = press(m, "enter")
	// No nitro group without the class: no Nitro AMI row yet.
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || hasField(m, "NITRO_AMI_ID") || slices.Contains(groupRows(m), "nitro") {
		t.Fatalf("node-group, class off: rows %v, AMI row %v", groupRows(m), hasField(m, "NITRO_AMI_ID"))
	}
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter")
	if !m.set.classOn(state.ClassNitro) || !hasField(m, "NITRO_AMI_ID") || !slices.Equal(groupRows(m), []string{"system", "gvisor", "nitro"}) {
		t.Fatalf("node-group without Auto Mode: on %v, rows %v\n%s", m.set.classOn(state.ClassNitro), groupRows(m), m.View())
	}
	m = cursorTo(t, m, "NITRO_AMI_ID")
	if v := m.View(); !strings.Contains(v, "k8s=1.37") || !strings.Contains(v, "amazon-eks-ami: make") {
		t.Errorf("the Nitro AMI panel for 1.37:\n%s", v)
	}
	m = cursorTo(t, m, "KUBERNETES_VERSION")
	m = setText(m, "1.38")
	m = cursorTo(t, m, "NITRO_AMI_ID")
	if v := m.View(); !strings.Contains(v, "k8s=1.38") || strings.Contains(v, "k8s=1.37") {
		t.Errorf("the Nitro AMI panel for 1.38:\n%s", v)
	}
	m = setText(m, "ami-0a17000000000000e")
	m = press(m, "n")
	if m.m.Current() != state.Plugins || s.AutoMode || !slices.Equal(s.NodeGroups(), []string{"system", "gvisor", "nitro"}) {
		t.Fatalf("at %s, Auto Mode %v, groups %v", m.m.Current().Title(), s.AutoMode, s.NodeGroups())
	}
	m = toStep(t, m, state.NitroSandbox)
	if v := m.View(); !strings.Contains(v, nitroSubtitle(plugin.NitroNodeGroup)[:60]) || strings.Contains(v, "NodePool ate-isolated") {
		t.Errorf("the step's subtitle:\n%s", v)
	}
	m = toStep(t, m, state.Complete)
	before(t, r.ran(), "create cluster", "nitro")
}

// The Set up Nitro sandboxes subtitle says where the class runs: Auto
// Mode's NodePool by default (empty or auto-mode, the text it always had),
// the nitro node group with node-group.
func TestNitroSubtitle(t *testing.T) {
	const autoMode = "The AWS Nitro sandbox class: NodePool ate-isolated on EKS Auto Mode, atelet-iso, and a counter pool on RuntimeClass isolated-sandbox, checked across a suspend and resume"
	if nitroSubtitle("") != autoMode || nitroSubtitle(plugin.NitroAutoMode) != autoMode {
		t.Errorf("auto-mode: %q", nitroSubtitle(""))
	}
	if g := nitroSubtitle(plugin.NitroNodeGroup); !strings.Contains(g, "on the nitro node group (ate.dev/pool=nitro)") || strings.Contains(g, "NodePool") {
		t.Errorf("node-group: %q", g)
	}
}

// A connected cluster: with NITRO_COMPUTE=node-group the class needs a node
// labeled ate.dev/pool=nitro, with or without Auto Mode, and has no
// NITRO_COMPUTE row (config.env decides); with auto-mode a cluster without
// Auto Mode is refused and told about its nitro group.
func TestConnectedClusterNitroNodeGroup(t *testing.T) {
	for _, c := range []struct {
		cluster, pools, compute string
		on                      bool
		notice                  string
	}{
		{"team-partial", "system,gvisor,nitro", plugin.NitroNodeGroup, true, ""},
		{"new", "system,gvisor,nitro", plugin.NitroNodeGroup, true, ""},
		{"team-partial", "system,gvisor", plugin.NitroNodeGroup, false, "team-partial has no nitro node group (no node labeled ate.dev/pool=nitro)"},
		{"team-partial", "system,gvisor,nitro", plugin.NitroAutoMode, false, "team-partial has a nitro node group, which NITRO_COMPUTE=node-group uses"},
	} {
		r := &recorder{probes: map[string]string{c.cluster: "SUBSTRATE_EKS_INSTALLED false pools=" + c.pools}}
		s := newSetup(c.cluster)
		s.SandboxClasses = state.ClassGVisor
		s.NewClusterConfig.NitroCompute = c.compute
		m := toStep(t, New(testDeps(r), s), state.Settings)
		if hasField(m, "NITRO_COMPUTE") || hasField(m, "NITRO_AMI_ID") {
			t.Errorf("%+v: a connected cluster has the nitro rows", c)
		}
		m = cursorTo(t, m, state.ClassNitro)
		m = press(m, "enter")
		if m.set.classOn(state.ClassNitro) != c.on || !strings.Contains(m.notice, c.notice) {
			t.Errorf("%+v: on %v, notice %q", c, m.set.classOn(state.ClassNitro), m.notice)
		}
		if c.on {
			if m = press(m, "n"); m.m.Current() != state.Plugins {
				t.Errorf("%+v: at %s, errs %v", c, m.m.Current().Title(), m.set.errs)
			}
		}
	}
}

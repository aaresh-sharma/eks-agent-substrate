package state

import (
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

func awsxCluster(name string) awsx.Cluster {
	return awsx.Cluster{Name: name, Version: "1.37", Status: "ACTIVE", AutoMode: true}
}

// Plugins for these tests, shaped like the real ones: s-kata adds the
// microvm node group (mode 3), s-nitro is the AWS Nitro sandbox class's
// (mode3-iso), s-gw runs on the system group (the LLM gateway), s-claw on
// gvisor (OpenClaw), s-vol builds on the counter demo (EFS).
func init() {
	for _, p := range []plugin.Plugin{
		{ID: "s-gw", Order: 1, Nodes: []string{"system"}},
		{ID: "s-vol", Order: 2, Nodes: []string{"system"}, NeedsDemo: true},
		{ID: "s-claw", Order: 3, Nodes: []string{"gvisor"}},
		{ID: "s-kata", Order: 4, Nodes: []string{"microvm"}, AddsNodes: true},
		{ID: "s-nitro", Order: 5, Class: ClassNitro, NeedsAutoMode: "needs EKS Auto Mode"},
	} {
		plugin.Register(p)
	}
}

// Every row of the rule: the node groups a new cluster gets for its
// classes, plugins and Auto Mode. The default makes the three groups, as
// before the rule.
func TestNewNodeGroupsEveryRow(t *testing.T) {
	for _, c := range []struct {
		name    string
		auto    bool
		classes string
		plugins []string
		want    string
	}{
		{"default: Auto Mode, gvisor and microvm", true, DefaultSandboxClasses, nil, "system,gvisor,microvm"},
		{"gvisor class", true, "gvisor", nil, "system,gvisor"},
		{"microvm class", true, "microvm", nil, "system,microvm"},
		{"mode 3 without the microvm class", true, "gvisor", []string{"s-kata"}, "system,gvisor,microvm"},
		{"mode 3 with only the Nitro class", true, "nitro", []string{"s-kata"}, "system,microvm"},
		{"Nitro class alone: no node group", true, "nitro", nil, ""},
		{"Nitro with a system-group plugin chosen: still none", true, "nitro", []string{"s-gw"}, ""},
		{"Nitro and gvisor", true, "gvisor,nitro", nil, "system,gvisor"},
		{"no Auto Mode, gvisor: system is required", false, "gvisor", nil, "system,gvisor"},
		{"no Auto Mode, default", false, DefaultSandboxClasses, nil, "system,gvisor,microvm"},
	} {
		if got := strings.Join(NewNodeGroups(c.auto, c.classes, c.plugins), ","); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// Setup.NodeGroups is the same for a new cluster, and a connected
	// cluster's own groups, in order, for one it connects.
	s := &Setup{NewClusterConfig: ClusterConfig{AutoMode: "on"}, SandboxClasses: "nitro"}
	s.NewCluster("c")
	if g := s.NodeGroups(); len(g) != 0 {
		t.Errorf("new, Nitro alone: %v", g)
	}
	s.SelectCluster(awsxCluster("c"), []string{"microvm", "isolated", "system"})
	if g := s.NodeGroups(); !slices.Equal(g, []string{"system", "microvm"}) {
		t.Errorf("connected: %v", g)
	}
}

// The refusals: the Nitro class without Auto Mode, a class or plugin whose
// nodes a connected cluster lacks, a plugin pinned to a group the new
// cluster does not get (with the fix), and one built on the counter demo
// with no class to run it on; each with what makes it available.
func TestClassAndPluginRefusals(t *testing.T) {
	if ClassProblem(ClassNitro, false, true, "c", nil) == "" || ClassProblem(ClassNitro, true, true, "c", nil) != "" {
		t.Error("nitro and Auto Mode")
	}
	if why := ClassProblem(ClassGVisor, true, false, "c", []string{"system", "microvm"}); !strings.Contains(why, "c has no gvisor node group") {
		t.Errorf("connected without gvisor: %q", why)
	}
	if ClassProblem(ClassGVisor, true, false, "c", []string{"gvisor"}) != "" || ClassProblem(ClassGVisor, true, true, "c", nil) != "" {
		t.Error("gvisor refused with its nodes, or on a new cluster")
	}

	s := &Setup{NewClusterConfig: ClusterConfig{AutoMode: "on"}, SandboxClasses: "nitro"}
	s.NewCluster("c")
	get := func(id string) plugin.Plugin { p, _ := plugin.Get(id); return p }
	for id, want := range map[string]string{
		"s-gw":   "an Auto Mode cluster gets only with the gVisor or microVM sandbox class",
		"s-claw": "runs on the gvisor node group: turn on its sandbox class",
		"s-vol":  "an Auto Mode cluster gets only with",
		"s-kata": "",
	} {
		if why := s.PluginProblem(get(id), nil); !strings.Contains(why, want) || (want == "") != (why == "") {
			t.Errorf("minimal profile, %s: %q, want %q", id, why, want)
		}
	}
	// s-kata's microvm group brings the system group: the gateway fits,
	// and the counter demo still has no class to run on.
	if why := s.PluginProblem(get("s-gw"), []string{"s-kata"}); why != "" {
		t.Errorf("gateway with s-kata: %q", why)
	}
	if why := s.PluginProblem(get("s-vol"), []string{"s-kata"}); !strings.Contains(why, "builds on the counter demo") {
		t.Errorf("counter volumes with no demo class: %q", why)
	}
	s.SandboxClasses = "gvisor"
	for _, id := range []string{"s-gw", "s-vol", "s-claw"} {
		if why := s.PluginProblem(get(id), nil); why != "" {
			t.Errorf("gvisor class, %s: %q", id, why)
		}
	}
	s.NewClusterConfig.AutoMode = ""
	s.NewCluster("c")
	if why := s.PluginProblem(get("s-nitro"), nil); !strings.Contains(why, "Auto Mode off") {
		t.Errorf("Nitro's plugin off Auto Mode: %q", why)
	}
	s.SelectCluster(awsxCluster("c"), []string{"system"})
	if why := s.PluginProblem(get("s-claw"), nil); why != "runs on the gvisor node group, which c does not have" {
		t.Errorf("connected without gvisor: %q", why)
	}
}

// What ate-setup gets, from its own two classes: gvisor always (one
// SandboxConfig every gVisor-class pool resolves, the Nitro class's
// counter-iso and mode 3's among them), microvm with the microvm class.
// The counter demo runs on gvisor and microvm; the Nitro class brings its
// plugin, and the install then has a Set up Nitro sandboxes step.
func TestClassesReachAteSetupTheDemoAndThePlugins(t *testing.T) {
	for classes, want := range map[string]string{
		"gvisor,microvm": "gvisor,microvm", "gvisor": "gvisor", "microvm": "gvisor,microvm",
		"nitro": "gvisor", "gvisor,nitro": "gvisor", "microvm,nitro": "gvisor,microvm",
	} {
		if got := AteSetupClasses(classes); got != want {
			t.Errorf("AteSetupClasses(%s) = %s, want %s", classes, got, want)
		}
	}
	if got := DemoClasses("gvisor,microvm,nitro"); !slices.Equal(got, []string{"gvisor", "microvm"}) || len(DemoClasses("nitro")) != 0 {
		t.Errorf("DemoClasses = %v", got)
	}
	s := &Setup{SandboxClasses: "gvisor,nitro", Plugins: []string{"s-gw"}, Database: DatabaseBundled}
	if got := s.InstallPlugins(); !slices.Equal(got, []string{"s-gw", "s-nitro"}) {
		t.Errorf("InstallPlugins = %v", got)
	}
	o := InstallOrder(s)
	if i := slices.Index(o, NitroSandbox); i < 0 || o[i-1] != ControlPlane {
		t.Errorf("order %v", o)
	}
	s.SandboxClasses = "gvisor"
	if slices.Contains(InstallOrder(s), NitroSandbox) || len(s.InstallPlugins()) != 1 {
		t.Errorf("without the class: order %v, plugins %v", InstallOrder(s), s.InstallPlugins())
	}
}

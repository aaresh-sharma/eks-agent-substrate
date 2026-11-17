// Package nodegroups is the rule for which node groups `setup-aws create
// cluster` makes, and of which type, shared with the installer, which shows
// them on its Settings and Review screens and passes them as NODE_GROUPS and
// NODE_GROUP_TYPE. Like phases it has no dependencies, so the installer
// imports it through its replace directive without cobra or the AWS SDK.
//
// The gvisor, microvm and nitro groups run sandboxes: worker Pods of the
// gVisor, microVM (Kata) and AWS Nitro sandbox classes (nitro only with
// NITRO_COMPUTE=node-group; by default the Nitro sandbox runs on Auto
// Mode's nodes). The system group runs what is pinned to it (the control
// plane with RDS on Auto Mode, the plugins' controllers). A cluster without
// EKS Auto Mode has no other nodes, so it always needs the system group and
// a sandbox group. With Auto Mode, Auto Mode's nodes run everything else,
// so the system group is made only for what is pinned to it, and a cluster
// may have no node group at all (its sandboxes are Auto Mode's, such as the
// AWS Nitro sandbox class's).
package nodegroups

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The node groups, by name; each one's nodes carry ate.dev/pool=<name>.
const (
	System  = "system"
	GVisor  = "gvisor"
	MicroVM = "microvm"
	// Nitro runs the AWS Nitro sandbox class on nodes of a Nitro Sandbox
	// AMI (NITRO_AMI_ID), with Nitro Enclaves on.
	Nitro = "nitro"
)

// All are the node groups in the order they are listed and created, and
// the derived names (Auto Scaling groups, launch templates) teardown and
// inventory look for.
var All = []string{System, GVisor, MicroVM, Nitro}

// None is NODE_GROUPS for no node group (an empty value means the default).
const None = "none"

// Default is NODE_GROUPS when it is unset: system, gvisor and microvm, as
// before the rule. It is spelled out: the nitro group is never a default.
const Default = "system,gvisor,microvm"

// The node group types, NODE_GROUP_TYPE: EKS managed node groups, or
// self-managed ones, an Auto Scaling group per node group whose nodes join
// the cluster from their launch template. A cluster has one type.
const (
	Managed     = "managed"
	SelfManaged = "self-managed"
)

// Types are the node group types; Managed is the default.
var Types = []string{Managed, SelfManaged}

// CheckType refuses a NODE_GROUP_TYPE that is not one of Types.
func CheckType(t string) error {
	if !slices.Contains(Types, t) {
		return fmt.Errorf("NODE_GROUP_TYPE=%q is not a node group type (want %s)", t, strings.Join(Types, " or "))
	}
	return nil
}

// For is the node groups of a cluster whose sandboxes need the sandbox
// groups in sandbox (gvisor, microvm, nitro), in All's order: those, plus
// the system group when something runs on it:
//   - without Auto Mode, everything that is not a sandbox: there are no
//     other nodes;
//   - with Auto Mode and RDS (rds) and a sandbox group, the control plane,
//     which the install then cordons onto the system group
//     (--cordon-system-infra-components);
//   - pinned: the Pods of a chosen plugin (the LLM gateway, EFS, ...).
//
// With Auto Mode and the bundled PostgreSQL nothing else is pinned there:
// the control plane and its database run on Auto Mode's nodes, so without a
// pinned plugin no system group is made.
func For(autoMode, rds, pinned bool, sandbox []string) []string {
	var out []string
	for _, g := range All {
		if g == System {
			if !autoMode || pinned || (rds && hasSandbox(sandbox)) {
				out = append(out, g)
			}
			continue
		}
		if slices.Contains(sandbox, g) {
			out = append(out, g)
		}
	}
	return out
}

func hasSandbox(groups []string) bool {
	return slices.Contains(groups, GVisor) || slices.Contains(groups, MicroVM) || slices.Contains(groups, Nitro)
}

// Check refuses an unknown or repeated group, and without Auto Mode a list
// that lacks the system group or a sandbox group: such a cluster has no
// other nodes. With Auto Mode any of the groups, or none, makes a cluster
// that works: Auto Mode's nodes run what no group is made for, and the
// control plane is cordoned onto the system group only when it exists.
func Check(autoMode bool, groups []string) error {
	for i, g := range groups {
		if !slices.Contains(All, g) {
			return fmt.Errorf("unknown node group %q (want %s)", g, strings.Join(All, ", "))
		}
		if slices.Contains(groups[:i], g) {
			return fmt.Errorf("node group %s is named twice", g)
		}
	}
	sandbox := hasSandbox(groups)
	system := slices.Contains(groups, System)
	switch {
	case !autoMode && !sandbox:
		return errors.New("a cluster without EKS Auto Mode needs the gvisor, microvm or nitro node group: no other nodes run sandboxes")
	case !autoMode && !system:
		return errors.New("a cluster without EKS Auto Mode needs the system node group: no other nodes run the control plane")
	}
	return nil
}

// Parse reads NODE_GROUPS: comma-separated names, or None for no group.
func Parse(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == None {
		return []string{}, nil
	}
	if s == "" {
		return nil, fmt.Errorf("NODE_GROUPS is empty: name the node groups (%s), or %s for no node group", strings.Join(All, ","), None)
	}
	var out []string
	for _, g := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(g))
	}
	return out, nil
}

// Format is groups as NODE_GROUPS: comma-separated, or None.
func Format(groups []string) string {
	if len(groups) == 0 {
		return None
	}
	return strings.Join(groups, ",")
}

// The nitro group's nodes come from a Nitro Sandbox AMI, which awslabs/
// amazon-eks-ami builds with enable_pod_isolation=true: its nodeadm drop-in
// (/etc/eks/nodeadm.d/10-pod-isolation.yaml) labels each node
// NitroSandboxLabel=true, points the kubelet at the sandbox's CRI proxy and
// registers the isolated-sandbox runtime handler, and the node advertises
// NitroSandboxResource, one per sandbox it can run. The EKS optimized AMI
// has none of this, so setup-aws and the installer both check the resource.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-ami-create-update.html
const (
	NitroSandboxLabel    = "eks.amazonaws.com/nitro-sandbox"
	NitroSandboxResource = "eks.amazonaws.com/nitro-sandbox"
	NitroAMIDoc          = "https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-ami-create-update.html"
)

// NitroAMIBuild is how to build a Nitro Sandbox AMI for Kubernetes version
// (such as 1.37) from awslabs/amazon-eks-ami.
func NitroAMIBuild(version string) string {
	return "in a clone of https://github.com/awslabs/amazon-eks-ami: make k8s=" + version + " os_distro=al2023 enable_pod_isolation=true"
}

// SandboxCount reads a node's allocatable NitroSandboxResource, a
// Kubernetes quantity of a whole number of sandboxes: "4", or "1k" for a
// thousand. ok is false for anything else, which counts as none.
func SandboxCount(q string) (n int64, ok bool) {
	mult := int64(1)
	for suffix, m := range map[string]int64{"k": 1e3, "M": 1e6, "G": 1e9} {
		if v, cut := strings.CutSuffix(q, suffix); cut {
			q, mult = v, m
			break
		}
	}
	n, err := strconv.ParseInt(q, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, false
	}
	return n * mult, true
}

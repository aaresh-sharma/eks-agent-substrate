// Package nodegroups is the rule for which managed node groups `setup-aws
// create cluster` makes, shared with the installer, which shows them on its
// Settings and Review screens and passes them as NODE_GROUPS. Like phases it
// has no dependencies, so the installer imports it through its replace
// directive without cobra or the AWS SDK.
//
// The gvisor and microvm groups run sandboxes: worker Pods of the gVisor
// and microVM (Kata) classes. The system group runs what is pinned to it
// (the control plane with RDS on Auto Mode, the plugins' controllers). A
// cluster without EKS Auto Mode has no other nodes, so it always needs the
// system group and a sandbox group. With Auto Mode, Auto Mode's nodes run
// everything else, so the system group comes only with a sandbox group, and
// a cluster may have no node group at all (its sandboxes are Auto Mode's,
// such as the AWS Nitro sandbox class's).
package nodegroups

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The node groups, by name; each one's nodes carry ate.dev/pool=<name>.
const (
	System  = "system"
	GVisor  = "gvisor"
	MicroVM = "microvm"
)

// All are the node groups in the order they are listed and created.
var All = []string{System, GVisor, MicroVM}

// None is NODE_GROUPS for no node group (an empty value means the default).
const None = "none"

// Default is NODE_GROUPS when it is unset: all three, as before the rule.
var Default = Format(All)

// For is the node groups of a cluster whose sandboxes need the sandbox
// groups in sandbox (gvisor, microvm), in All's order: those, plus the
// system group when Auto Mode is off or any sandbox group is made.
func For(autoMode bool, sandbox []string) []string {
	var out []string
	for _, g := range All {
		if g == System {
			if !autoMode || hasSandbox(sandbox) {
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
	return slices.Contains(groups, GVisor) || slices.Contains(groups, MicroVM)
}

// Check refuses groups unless they are what For gives for their own sandbox
// groups: without Auto Mode, the system group and at least one sandbox
// group; with it, the system group exactly when a sandbox group is made.
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
		return errors.New("a cluster without EKS Auto Mode needs the gvisor or microvm node group: no other nodes run sandboxes")
	case !autoMode && !system:
		return errors.New("a cluster without EKS Auto Mode needs the system node group: no other nodes run the control plane")
	case sandbox && !system:
		return errors.New("the gvisor and microvm node groups need the system node group too: the controllers pinned to it run there")
	case autoMode && system && !sandbox:
		return errors.New("with EKS Auto Mode the system node group comes only with the gvisor or microvm node group: Auto Mode's nodes run the control plane")
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

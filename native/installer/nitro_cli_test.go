package main

import (
	"strings"
	"testing"
)

// --nitro-compute reaches the plugin: on a cluster without Auto Mode,
// --plugin-action verify --plugins mode3-iso is refused for Auto Mode by
// default and with auto-mode, and gets past that gate with node-group (it
// stops later, at the missing tree); a value that is not one, from the flag
// or NITRO_COMPUTE, stops the run before any cluster read.
func TestNitroComputeFlag(t *testing.T) {
	for _, tc := range []struct {
		args, env []string
		want, not string
	}{
		{nil, nil, "plugin mode3-iso needs EKS Auto Mode", "no mode 3 tree"},
		{[]string{"--nitro-compute", "auto-mode"}, nil, "demo has EKS Auto Mode off", "no mode 3 tree"},
		{[]string{"--nitro-compute", "node-group"}, nil, "no mode 3 tree", "Auto Mode off"},
		{nil, []string{"NITRO_COMPUTE=node-group"}, "no mode 3 tree", "Auto Mode off"},
		{[]string{"--nitro-compute", "nodegroup"}, nil, `NITRO_COMPUTE="nodegroup" is not auto-mode or node-group`, "kubeconfig"},
		{nil, []string{"NITRO_COMPUTE=bogus"}, `NITRO_COMPUTE="bogus" is not auto-mode or node-group`, "kubeconfig"},
	} {
		args := append([]string{"--plugin-action", "verify", "--plugins", "mode3-iso"}, tc.args...)
		env := append([]string{"NITRO_COMPUTE="}, tc.env...)
		err, out := runMainIn(t, "", env, cluster("", "v1.0.0"), args...)
		if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, tc.not) {
			t.Errorf("%v %v: %v, want %q and not %q:\n%s", tc.args, tc.env, err, tc.want, tc.not, out)
		}
	}
}

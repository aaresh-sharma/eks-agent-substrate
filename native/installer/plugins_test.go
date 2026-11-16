package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Every plugin the installer imports registers consistently: what it
// requires exists and installs before it.
func TestPluginRegistry(t *testing.T) {
	if err := plugin.Check(); err != nil {
		t.Fatal(err)
	}
	for _, p := range plugin.All() {
		if p.Title == "" || p.Summary == "" {
			t.Errorf("plugin %s has no title or summary for the wizard", p.ID)
		}
		if p.Deploy == nil && !p.SetupAWS && p.Needs.Source == nil {
			t.Errorf("plugin %s does nothing", p.ID)
		}
	}
}

// childrenOfChecks calls each of ps's Check with an empty lookup and with
// one that answers "x" for every key, and returns every child those calls
// started (plugin.Exec, which every plugin child goes through, records them
// and starts none).
// shortcut: an AWS SDK or HTTP call in a Check does not go through
// plugin.Exec and is not seen; upgrade this when a Check needs the network.
func childrenOfChecks(t *testing.T, ps []plugin.Plugin) []string {
	t.Helper()
	var mu sync.Mutex
	var started []string
	prev := plugin.Exec
	t.Cleanup(func() { plugin.Exec = prev })
	for _, p := range ps {
		if p.Check == nil {
			continue
		}
		plugin.Exec = func(_ context.Context, _ io.Writer, c plugin.Cmd) error {
			mu.Lock()
			defer mu.Unlock()
			started = append(started, p.ID+": "+strings.Join(c.Argv, " "))
			return errors.New("a Check started a child")
		}
		for _, lookup := range []func(string) (string, bool){
			func(string) (string, bool) { return "", false },
			func(string) (string, bool) { return "x", true },
		} {
			_ = p.Check(lookup)
		}
	}
	plugin.Exec = prev
	mu.Lock()
	defer mu.Unlock()
	return started
}

// No plugin's Check starts a child, whatever the settings: Review calls each
// chosen plugin's Check on every render, and Run and --plugin-action call it
// before their first child (review of deb95a7, NIT B). The check is proved
// first on a Check that runs kubectl.
func TestNoPluginCheckStartsAChild(t *testing.T) {
	kubectl := plugin.Plugin{ID: "runs-kubectl", Check: func(func(string) (string, bool)) error {
		return plugin.Kubectl(context.Background(), io.Discard, plugin.Options{Kubeconfig: "/kc"}, nil, nil, "get", "nodes")
	}}
	if got := childrenOfChecks(t, []plugin.Plugin{kubectl}); len(got) != 2 {
		t.Fatalf("the check missed a Check that runs kubectl: %q", got)
	}
	checks := 0
	for _, p := range plugin.All() {
		if p.Check != nil {
			checks++
		}
	}
	if checks == 0 {
		t.Fatal("no registered plugin has a Check, so nothing was checked")
	}
	if got := childrenOfChecks(t, plugin.All()); len(got) > 0 {
		t.Errorf("a plugin's Check started a child: %q", got)
	}
}

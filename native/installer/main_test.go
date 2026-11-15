package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/llmgateway"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// Settings' answers go to the checkout's config.local.env, 0600, and a
// different earlier file is kept under the cache directory, not the checkout.
func TestSaveConfigWritesConfigLocalEnv(t *testing.T) {
	dir, cache := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.env"), []byte("CLUSTER_NAME=agent-substrate\nGVISOR_NODES=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.local.env"), []byte("WHATSAPP_PEER=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := state.LoadConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := state.NewSetupFrom(cfg.Lookup)
	s.ClusterName, s.Region = "sub-demo", "us-west-2"
	s.NewClusterConfig.GVisorNodes = "4"
	path, err := saveConfig(cfg, s, cache)
	if err != nil || path != filepath.Join(dir, "config.local.env") {
		t.Fatalf("saveConfig: %q %v", path, err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"CLUSTER_NAME=sub-demo\n", "GVISOR_NODES=4\n", "WHATSAPP_PEER=x\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config.local.env lacks %q:\n%s", want, b)
		}
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if backups, _ := filepath.Glob(filepath.Join(cache, "substrate-eks", "config-backups", "config.local.env.*")); len(backups) != 1 {
		t.Errorf("backups %v", backups)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 2 {
		t.Errorf("the checkout got more than config.local.env: %v", left)
	}
}

// Review F7: the installer log is 0600 in a 0700 directory, whether it is
// new or was left wider by an earlier run, and what it held is kept.
func TestCreatePrivateLogFile(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "logs", "a.log")
	if err := createPrivate(fresh); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "b.log")
	if err := os.WriteFile(old, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := createPrivate(old); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fresh, old} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v (%v), want 0600", p, st.Mode().Perm(), err)
		}
	}
	if st, _ := os.Stat(filepath.Dir(fresh)); st.Mode().Perm() != 0o700 {
		t.Errorf("log dir mode %v, want 0700", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(old); string(b) != "kept\n" {
		t.Errorf("existing log content lost: %q", b)
	}
}

// Review NIT-6: the checkout's comment on SANDBOX_CLASSES says when Auto
// Mode with the nitro class alone gets a node group: only for a plugin that
// adds one, mode3's microvm or system for a plugin that runs on it (the LLM
// gateway); none without one. The rule, with the real plugins, agrees.
func TestSandboxClassesCommentSaysWhenAPluginAddsANodeGroup(t *testing.T) {
	for _, c := range []struct {
		plugins []string
		want    []string
	}{{nil, nil}, {[]string{mode3.ID}, []string{"microvm"}}, {[]string{llmgateway.ID}, []string{"system"}}} {
		if got := state.NewNodeGroups(true, state.DatabaseBundled, state.ClassNitro, c.plugins); !slices.Equal(got, c.want) {
			t.Errorf("Auto Mode, bundled, nitro, %v: %v, want %v", c.plugins, got, c.want)
		}
	}
	cfg, err := state.LoadConfig(filepath.Join("..", ".."), nil)
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Join(strings.Fields(cfg.Defaults.Doc("SANDBOX_CLASSES")), " ")
	for _, want := range []string{
		"the microvm group for the mode3 plugin",
		"or for a plugin that runs on it",
		"With SANDBOX_CLASSES=nitro it makes a node group only for a plugin that adds one (mode3's microvm, or system for a plugin that runs on it), and none without such a plugin.",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the SANDBOX_CLASSES comment lacks %q:\n%s", want, doc)
		}
	}
}

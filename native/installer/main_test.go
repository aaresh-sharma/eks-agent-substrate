package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
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

// Both ways, through what main calls (searchStarts, then state.FindConfig
// and eksoverlay.Find): a release binary started from inside a checkout,
// from its root (the documented ./native/install.sh --from-release) or from
// its native/, reads its own archive's config.env, writes config.local.env
// beside it and uses its own deploy/eks, as it does outside a checkout. A
// source build keeps searching above the working directory first: started
// from another checkout's native/ it uses that checkout's files, even with
// a setup-aws binary built into its own native/bin, which has no config.env
// and so is not a release.
func TestReleaseBinaryUsesItsOwnFiles(t *testing.T) {
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	checkout := func() string {
		co := t.TempDir()
		write(filepath.Join(co, "config.env"), "SUBSTRATE_TAG=v1.0.0\n")
		write(filepath.Join(co, "native", "tools", "setup-aws", "go.mod"), "module x\n")
		write(filepath.Join(co, "native", "deploy", "eks", "components", "eks", "kustomization.yaml"), "kind: Component\n")
		return co
	}
	co, other := checkout(), checkout()
	built := filepath.Join(co, "native", "bin") // make build's output, with a setup-aws built beside it
	write(filepath.Join(built, "substrate-eks"), "binary")
	write(filepath.Join(built, "setup-aws"), "binary")
	release := t.TempDir()
	write(filepath.Join(release, "config.env"), "SUBSTRATE_TAG=v2.0.0\n")
	write(filepath.Join(release, "setup-aws"), "binary")
	write(filepath.Join(release, "substrate-eks"), "binary")
	write(filepath.Join(release, "deploy", "eks", "components", "eks", "kustomization.yaml"), "kind: Component\n")

	for _, tc := range []struct{ name, cwd, exeDir, dir, overlay string }{
		{"release from a checkout's root", co, release, release, filepath.Join(release, "deploy", "eks")},
		{"release from a checkout's native/", filepath.Join(co, "native"), release, release, filepath.Join(release, "deploy", "eks")},
		{"release outside a checkout", t.TempDir(), release, release, filepath.Join(release, "deploy", "eks")},
		{"source build from its checkout's native/", filepath.Join(co, "native"), built, co, filepath.Join(co, "native", "deploy", "eks")},
		{"source build from another checkout's native/", filepath.Join(other, "native"), built, other, filepath.Join(other, "native", "deploy", "eks")},
	} {
		starts := searchStarts(tc.cwd, tc.exeDir)
		cfg, err := state.FindConfig(func(string) (string, bool) { return "", false }, starts...)
		if err != nil || cfg.Dir != tc.dir || cfg.LocalPath() != filepath.Join(tc.dir, "config.local.env") {
			t.Errorf("%s: config in %q (writes %q), %v; want %q", tc.name, cfg.Dir, cfg.LocalPath(), err, tc.dir)
		}
		overlay, err := eksoverlay.Find("", starts...)
		if err != nil || overlay != tc.overlay {
			t.Errorf("%s: deploy/eks %q, %v; want %q", tc.name, overlay, err, tc.overlay)
		}
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

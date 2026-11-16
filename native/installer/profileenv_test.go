package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// With no --profile (the default credential chain), no child gets
// AWS_PROFILE="". The AWS SDK for Go reads it as unset, but the AWS CLI looks
// for a profile named "" and exits 255, which stopped a macOS install at
// ate-setup's "Stage microVM assets in S3" ("The config profile () could not
// be found"). Every live run before it passed --profile, so none hit it.
func TestNoChildGetsAnEmptyAWSProfile(t *testing.T) {
	for _, profile := range []string{"", "eks-sandbox"} {
		b, s := testBuilder()
		s.Profile = profile
		var pluginEnv []string
		restore := plugin.Exec
		plugin.Exec = func(_ context.Context, _ io.Writer, c plugin.Cmd) error { pluginEnv = c.Env; return nil }
		err := plugin.SetupAWS(context.Background(), io.Discard,
			plugin.Options{Cluster: "demo", Region: "us-west-2", Profile: profile, SetupAWS: t.TempDir()}, "plugin", "efs")
		plugin.Exec = restore
		if err != nil {
			t.Fatal(err)
		}
		envs := map[string][]string{
			"create cluster":   b.createCluster(s).Env,
			"teardown":         b.teardown(s).Env,
			"ate-setup":        controlplane.AteSetupEnv(controlplane.Options{Region: "us-west-2", Profile: profile}, "/kc"),
			"setup-aws plugin": pluginEnv,
		}
		for step, env := range envs {
			got := slices.IndexFunc(env, func(e string) bool { return strings.HasPrefix(e, "AWS_PROFILE=") })
			switch {
			case profile == "" && got >= 0:
				t.Errorf("profile %q: %s env has %q; the AWS CLI exits 255 on an empty AWS_PROFILE", profile, step, env[got])
			case profile != "" && (got < 0 || env[got] != "AWS_PROFILE="+profile):
				t.Errorf("profile %q: %s env %q lacks AWS_PROFILE=%s", profile, step, env, profile)
			}
		}
	}
}

// profileBuilder finds code that builds the AWS_PROFILE=<value> entry
// itself, which lets an empty value through again; awsx.ProfileEnv is the
// one place that builds it.
var profileBuilder = regexp.MustCompile(`"AWS_PROFILE="\s*\+`)

// profileBuilders lists the lines of files (name to source) outside
// awsx.go and tests that build the entry, skipping comment lines.
func profileBuilders(files map[string]string) []string {
	var out []string
	for name, src := range files {
		if filepath.Base(name) == "awsx.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		for i, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if profileBuilder.MatchString(line) {
				out = append(out, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestOnlyProfileEnvBuildsAWSProfile(t *testing.T) {
	files := map[string]string{}
	for _, root := range []string{".", "../tools/setup-aws"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") {
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				files[path] = string(b)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) < 50 {
		t.Fatalf("read only %d Go files: the walk is not looking at the modules", len(files))
	}
	if got := profileBuilders(files); len(got) > 0 {
		t.Errorf("build the AWS_PROFILE entry with awsx.ProfileEnv, which leaves it out when empty:\n%s", strings.Join(got, "\n"))
	}
}

// The check is proved both ways.
func TestProfileBuildersCheck(t *testing.T) {
	bad := map[string]string{
		"a.go": `env := []string{"AWS_PROFILE=" + s.Profile}`,
		"b.go": "x := \"AWS_PROFILE=\"+p",
	}
	if got := profileBuilders(bad); len(got) != 2 {
		t.Errorf("caught %q, want both", got)
	}
	good := map[string]string{
		"c.go":                  `env := awsx.ProfileEnv(p)`,
		"d.go":                  "\t// the old code set \"AWS_PROFILE=\" + p",
		"internal/awsx/awsx.go": `return []string{"AWS_PROFILE=" + profile}`,
		"e_test.go":             `want := "AWS_PROFILE=" + p`,
		"f.go":                  `if os.Getenv("AWS_PROFILE") == "" {}`,
	}
	if got := profileBuilders(good); len(got) != 0 {
		t.Errorf("flagged %q", got)
	}
}

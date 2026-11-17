package plugin

import (
	"slices"
	"strings"
	"testing"
)

func withRegistry(t *testing.T, ps ...Plugin) {
	t.Helper()
	saved := registry
	registry = map[string]Plugin{}
	t.Cleanup(func() { registry = saved })
	for _, p := range ps {
		Register(p)
	}
}

func ids(ps []Plugin) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func TestResolveAddsRequirementsInOrder(t *testing.T) {
	withRegistry(t,
		Plugin{ID: "openclaw", Order: 50, Requires: []string{"efs", "llm-gateway"}},
		Plugin{ID: "efs", Order: 20},
		Plugin{ID: "llm-gateway", Order: 10},
		Plugin{ID: "s3files", Order: 30},
	)
	got, err := Resolve([]string{"openclaw"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"llm-gateway", "efs", "openclaw"}; !slices.Equal(ids(got), want) {
		t.Errorf("Resolve = %v, want %v", ids(got), want)
	}
	if _, err := Resolve([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("unknown plugin: %v", err)
	}
}

func TestResolveRefusesACycle(t *testing.T) {
	withRegistry(t, Plugin{ID: "a", Requires: []string{"b"}}, Plugin{ID: "b", Requires: []string{"a"}})
	if _, err := Resolve([]string{"a"}); err == nil || !strings.Contains(err.Error(), "a -> b -> a") {
		t.Errorf("cycle: %v", err)
	}
}

func TestMergeUnitesNeedsAndRefusesTwoSources(t *testing.T) {
	a := Plugin{ID: "a", Needs: Needs{Preview: []string{"ExternalVolumes"}, IngressIDs: []string{"spiffe://x/b", "spiffe://x/a"}}}
	b := Plugin{ID: "b", Needs: Needs{Preview: []string{"ExternalVolumes", "NetworkWorkers"}, CredentialProviders: []string{`{"name":"aws"}`},
		Source: &Source{Tag: "v1.0.0-fork"}}}
	n, err := Merge([]Plugin{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(n.Preview, []string{"ExternalVolumes", "NetworkWorkers"}) || !slices.Equal(n.IngressIDs, []string{"spiffe://x/a", "spiffe://x/b"}) ||
		len(n.CredentialProviders) != 1 || n.Source == nil || n.Source.Tag != "v1.0.0-fork" {
		t.Errorf("Merge = %+v", n)
	}
	c := Plugin{ID: "c", Needs: Needs{Source: &Source{Tag: "other"}}}
	if _, err := Merge([]Plugin{b, c}); err == nil {
		t.Error("two Sources merged")
	}
}

// Both ways: plugins whose Sources are equal in every field (their own
// copies, as mode3 and mode3-iso declare) merge into that one release; a
// Source that differs in any one field is refused, naming both plugins.
func TestMergeAcceptsOneSourceTwiceAndRefusesTwoDifferent(t *testing.T) {
	src := Source{Repo: "https://example.invalid/fork.git", Commit: "9f36a0d7f2abca950f25fc7bd023752d3852504c", Tag: "v1.0.0-fork", ImageRepo: "ghcr.io/example/fork"}
	same := src
	a := Plugin{ID: "a", Needs: Needs{Preview: []string{"NetworkWorkers"}, Source: &src}}
	b := Plugin{ID: "b", Needs: Needs{Preview: []string{"NetworkWorkers"}, Source: &same}}
	n, err := Merge([]Plugin{a, {ID: "plain"}, b})
	if err != nil || n.Source == nil || *n.Source != src || !slices.Equal(n.Preview, []string{"NetworkWorkers"}) {
		t.Fatalf("one Source twice: %+v, %v", n, err)
	}
	for field, mut := range map[string]func(*Source){
		"Repo":      func(s *Source) { s.Repo = "https://example.invalid/other.git" },
		"Commit":    func(s *Source) { s.Commit = "0000000000000000000000000000000000000000" },
		"Tag":       func(s *Source) { s.Tag = "v1.0.0-other" },
		"ImageRepo": func(s *Source) { s.ImageRepo = "ghcr.io/example/other" },
	} {
		other := src
		mut(&other)
		c := Plugin{ID: "c", Needs: Needs{Source: &other}}
		_, err := Merge([]Plugin{a, b, c})
		if err == nil || !strings.Contains(err.Error(), "plugins a and c install two different Substrate releases") {
			t.Errorf("another %s: %v", field, err)
		}
	}
}

func TestRecordRoundTrip(t *testing.T) {
	if got := ParseRecord(" efs, ,llm-gateway,efs "); !slices.Equal(got, []string{"efs", "llm-gateway"}) {
		t.Errorf("ParseRecord = %v", got)
	}
	if got := FormatRecord([]string{"s3files", "efs", "efs"}); got != "efs,s3files" {
		t.Errorf("FormatRecord = %q", got)
	}
}

func TestRegisterRefusesDuplicatesAndBadIDs(t *testing.T) {
	for _, bad := range []string{"", "a b", "a,b"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) did not panic", bad)
				}
			}()
			withRegistry(t, Plugin{ID: bad})
		}()
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("duplicate did not panic")
			}
		}()
		withRegistry(t, Plugin{ID: "x"}, Plugin{ID: "x"})
	}()
}

// AutoModeProblem: a plugin that needs Auto Mode is refused only off Auto
// Mode, and not where its AutoModeOptional lets it run (mode3-iso's
// NITRO_COMPUTE=node-group); a plugin that needs none never is.
func TestAutoModeProblem(t *testing.T) {
	needs := Plugin{ID: "x", NeedsAutoMode: "needs EKS Auto Mode"}
	optional := needs
	optional.AutoModeOptional = func(o Options) bool { return o.NitroCompute == NitroNodeGroup }
	for _, c := range []struct {
		p    Plugin
		o    Options
		want string
	}{
		{needs, Options{AutoMode: true}, ""},
		{needs, Options{}, "needs EKS Auto Mode"},
		{needs, Options{NitroCompute: NitroNodeGroup}, "needs EKS Auto Mode"},
		{optional, Options{}, "needs EKS Auto Mode"},
		{optional, Options{NitroCompute: NitroAutoMode}, "needs EKS Auto Mode"},
		{optional, Options{NitroCompute: NitroNodeGroup}, ""},
		{optional, Options{AutoMode: true, NitroCompute: NitroAutoMode}, ""},
		{Plugin{ID: "y"}, Options{}, ""},
	} {
		if got := c.p.AutoModeProblem(c.o); got != c.want {
			t.Errorf("%s with %+v: %q, want %q", c.p.ID, c.o, got, c.want)
		}
	}
	for v, ok := range map[string]bool{NitroAutoMode: true, NitroNodeGroup: true, "": false, "nodegroup": false, "Auto-Mode": false} {
		if err := CheckNitroCompute(v); (err == nil) != ok {
			t.Errorf("CheckNitroCompute(%q) = %v", v, err)
		}
	}
}

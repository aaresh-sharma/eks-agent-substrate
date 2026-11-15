package doctor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMaskArn(t *testing.T) {
	got := MaskArn("arn:aws:sts::111122223333:assumed-role/Admin/me")
	if strings.Contains(got, "111122223333") || !strings.Contains(got, "3333:assumed-role/Admin/me") {
		t.Errorf("MaskArn = %q", got)
	}
	if got := MaskArn("not-an-arn"); got != "not-an-arn" {
		t.Errorf("MaskArn changed a non-ARN: %q", got)
	}
}

// Both ways: Go 1.27 or newer passes; an older Go fails with a fix that says
// why an install needs it; a line with no version is a warning.
func TestGoVersionNeeds127(t *testing.T) {
	for line, want := range map[string]Status{
		"go version go1.22.3 darwin/arm64":                      Fail,
		"go version go1.26.4 linux/amd64":                       Fail,
		"go version go1.27.0 linux/amd64":                       OK,
		"go version go1.27rc1 linux/amd64":                      OK,
		"go version go1.28.1 linux/amd64":                       OK,
		"go version go2.0.0 linux/amd64":                        OK,
		"go version devel go1.28-0123abcd Mon Jan 5 2026 +0000": OK,
		"something else":                                        Warn,
	} {
		r := goVersion(line)
		if r.Status != want {
			t.Errorf("%q: status %v, want %v", line, r.Status, want)
		}
		if r.Detail != line {
			t.Errorf("%q: detail %q", line, r.Detail)
		}
		if (want == OK) != (r.Fix == "") {
			t.Errorf("%q: fix %q", line, r.Fix)
		}
		if want == Fail && !strings.Contains(r.Fix, "Go 1.27") || want == Fail && !strings.Contains(r.Fix, "ate-setup") {
			t.Errorf("%q: the fix does not say what needs Go 1.27: %q", line, r.Fix)
		}
	}
}

// Both ways, through the check the wizard and --doctor run: a go on PATH
// that is 1.26 fails it, one that is 1.27 passes, and none fails it with
// the same fix.
func TestGoCheckGradesTheGoOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX /bin/sh")
	}
	for version, want := range map[string]Status{"go1.26.4": Fail, "go1.27.0": OK, "": Fail} {
		bin := t.TempDir()
		if version != "" {
			script := "#!/bin/sh\necho 'go version " + version + " linux/amd64'\n"
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", bin)
		r := goCheck().Run(context.Background())
		if r.Status != want || (want == Fail) != strings.Contains(r.Fix, "ate-setup") {
			t.Errorf("go %q: %+v, want status %v", version, r, want)
		}
	}
}

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// install.sh is a launcher only (issue #5, AC8.2): it fetches the repository,
// builds the installer and execs it. Every AWS and Kubernetes call belongs to
// the installer, so the launcher must never run aws, kubectl or eksctl, nor a
// shell or eval that could run them from a string.

// forbiddenCommand matches a forbidden command in command position: at the
// start of a line, or after a control operator, a subshell or substitution
// opener, a keyword that runs its next word, or variable assignments;
// optionally quoted or given as a path. It runs over shellCode's output, so
// comments and single-quoted text never match. Double-quoted text is kept,
// since "$(aws ...)" and "$dir/kubectl" run; prose there that names a
// command right after a ';' or '|' is flagged too, which only costs a
// rewording.
var forbiddenCommand = regexp.MustCompile(`(?m)(?:^|[;&|(!` + "`" + `]|\$\(|\b(?:then|do|else|elif|exec|command|builtin|sudo|env|time|nohup|xargs)[ \t])` +
	`[ \t]*(?:[A-Za-z_][A-Za-z0-9_]*=[^ \t;&|]*[ \t]+)*["']?(?:[^ \t;&|"']*/)?(aws|kubectl|eksctl|eval|bash|sh|zsh)(?:$|[ \t;&|)"'` + "`" + `])`)

// shellCode returns script with comments removed and single-quoted text
// blanked. Line breaks are kept, so offsets map to the script's lines.
//
// shortcut: this is not a full shell parser. It refuses heredocs (their
// bodies would go unchecked), which install.sh has no need for; replace it
// with a real tokenizer if the launcher ever needs one.
func shellCode(script string) (string, error) {
	if strings.Contains(script, "<<") {
		return "", fmt.Errorf("heredoc in the launcher: its body cannot be checked")
	}
	var b strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
				b.WriteByte(c)
			} else if c == '\n' {
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case c == '\\' && i+1 < len(script):
			b.WriteByte(c)
			b.WriteByte(script[i+1])
			i++
		case inDouble:
			inDouble = c != '"'
			b.WriteByte(c)
		case c == '#' && (i == 0 || strings.IndexByte(" \t\n;", script[i-1]) >= 0):
			for i+1 < len(script) && script[i+1] != '\n' {
				i++
			}
		case c == '\'':
			inSingle = true
			b.WriteByte(c)
		case c == '"':
			inDouble = true
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// launcherCalls lists every forbidden command the script would run, by line.
func launcherCalls(script string) ([]string, error) {
	code, err := shellCode(script)
	if err != nil {
		return nil, err
	}
	var out []string
	lines := strings.Split(script, "\n")
	for _, loc := range forbiddenCommand.FindAllStringSubmatchIndex(code, -1) {
		n := strings.Count(code[:loc[2]], "\n")
		out = append(out, fmt.Sprintf("line %d: %s", n+1, strings.TrimSpace(lines[n])))
	}
	return out, nil
}

func TestLauncherRunsNoAWSOrKubectlCommand(t *testing.T) {
	b, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	calls, err := launcherCalls(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) > 0 {
		t.Errorf("install.sh must only launch the installer, but runs:\n%s", strings.Join(calls, "\n"))
	}
}

// The check is proved both ways: text that only mentions the commands
// passes, and each way of running one is caught, on the line it is on.
func TestLauncherCheckAcceptsMentionsAndRejectsInvocations(t *testing.T) {
	accept := strings.Join([]string{
		`#!/usr/bin/env bash`,
		`# the installer runs kubectl and aws; never this script`,
		`echo "the doctor checks aws, kubectl and eksctl, not this script"`,
		`printf '%s\n' 'kubectl apply; aws s3 ls | eksctl create'`,
		`aws_region="${AWS_REGION:-us-west-2}"`,
		`kubectl_version=1.37 # ; kubectl`,
		`if ! command -v git >/dev/null 2>&1; then exit 1; fi`,
		`echo "a # is not a comment here"`,
		`exec "${native_dir}/bin/substrate-eks" "$@"`,
		`echo "${awscli:-none} kubectl-free ${#x}"`,
	}, "\n")
	if calls, err := launcherCalls(accept); err != nil || len(calls) > 0 {
		t.Errorf("flagged a script that runs none of them: %q %v", calls, err)
	}
	for _, line := range []string{
		`kubectl apply -f x.yaml`,
		`    aws sts get-caller-identity`,
		`make build && eksctl create cluster`,
		`ok || kubectl get ns`,
		`id="$(aws sts get-caller-identity)"`,
		"id=`aws sts get-caller-identity`",
		`exec kubectl get pods`,
		`AWS_PROFILE=p aws s3 ls`,
		`if true; then kubectl get ns; fi`,
		`/usr/local/bin/aws --version`,
		`"$HOME/bin/kubectl" version`,
		`cat x | kubectl apply -f -`,
		`( eksctl get clusters )`,
		`! aws s3 ls`,
		`env AWS_REGION=x aws ec2 describe-vpcs`,
		`command kubectl version`,
		`echo "\"quoted\"" ; aws s3 ls`,
		`bash -c 'kubectl apply -f x'`,
		`eval "$cmd"`,
		`x=1 sh -c "$cmd"`,
	} {
		calls, err := launcherCalls("#!/usr/bin/env bash\nset -e\n" + line + "\n")
		if err != nil || len(calls) != 1 || !strings.HasPrefix(calls[0], "line 3: ") {
			t.Errorf("%q: calls = %q, err = %v; want one call on line 3", line, calls, err)
		}
	}
	if _, err := launcherCalls("cat <<EOF\nkubectl apply\nEOF\n"); err == nil {
		t.Error("a heredoc was accepted unchecked")
	}
}

// fakeGo answers `go env GOVERSION` with FAKE_GO_VERSION, after writing the
// directory it was asked from to FAKE_GO_PWD, and fails any other call, so a
// launcher that gets past its Go check stops at the build instead of running
// it.
const fakeGo = `#!/bin/sh
if [ "$1 $2" = 'env GOVERSION' ]; then
  pwd > "$FAKE_GO_PWD"
  printf '%s\n' "$FAKE_GO_VERSION"
  exit 0
fi
echo fake-go-build "$@"
exit 3
`

// install.sh stops before the build, naming the fix, when the Go the build
// would use is older than 1.27, and goes on to the build otherwise. It asks
// that Go from native/, where the build runs, so a per-directory version
// manager answers as it will for the build. PATH holds only the fake go and
// the tools the launcher needs, so no real Go or build runs.
func TestLauncherChecksTheGoVersion(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs bash and a POSIX /bin/sh")
	}
	script, err := filepath.Abs("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	nativeDir, err := filepath.EvalSymlinks(filepath.Dir(script))
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{}
	for _, name := range []string{"git", "make", "dirname"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("needs %s", name)
		}
		tools[name] = p
	}

	for _, c := range []struct {
		version string
		refused bool
	}{
		{"go1.22.3", true},
		{"go1.26.4", true},
		{"go1.27.0", false},
		{"go1.27rc1", false},
		{"go1.28.1", false},
		{"go2.0.0", false},
		{"devel go1.28-0123abcd Mon Jan 5 2026", false},
		{"", false},
	} {
		name := fmt.Sprintf("%q", c.version)
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0o755); err != nil {
			t.Fatal(err)
		}
		for tool, p := range tools {
			if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
		pwdFile := filepath.Join(t.TempDir(), "pwd")
		cmd := exec.Command(bash, script, "--dry-run")
		cmd.Env = []string{"PATH=" + bin, "HOME=" + t.TempDir(), "FAKE_GO_VERSION=" + c.version, "FAKE_GO_PWD=" + pwdFile}
		b, runErr := cmd.CombinedOutput()
		out := string(b)
		if runErr == nil {
			t.Errorf("%s: install.sh exited 0; the fake build always fails:\n%s", name, out)
		}

		asked, err := os.ReadFile(pwdFile)
		if err != nil {
			t.Errorf("%s: the launcher never asked go for its version: %v\n%s", name, err, out)
		} else if dir, err := filepath.EvalSymlinks(strings.TrimSpace(string(asked))); err != nil || dir != nativeDir {
			t.Errorf("%s: asked go for its version from %q, want %q (the build's directory)", name, strings.TrimSpace(string(asked)), nativeDir)
		}

		built := strings.Contains(out, "==> Building") || strings.Contains(out, "fake-go-build")
		refusal := "needs Go 1.27 or newer; found " + c.version
		if c.refused {
			if built || !strings.Contains(out, refusal) || !strings.Contains(out, "https://go.dev/dl/") {
				t.Errorf("%s: want a refusal naming the version and the fix, before the build:\n%s", name, out)
			}
			continue
		}
		if !strings.Contains(out, "==> Building") || !strings.Contains(out, "fake-go-build") || strings.Contains(out, "error: the substrate-eks installer needs Go") {
			t.Errorf("%s: want the build to start, with no refusal:\n%s", name, out)
		}
		if warned := strings.Contains(out, "could not read the Go version"); warned != (c.version == "") {
			t.Errorf("%s: unreadable-version warning shown = %v:\n%s", name, warned, out)
		}
	}
}

// fakeCurl serves FAKE_RELEASE as GitHub Releases would: it writes the file
// the URL's last element names to the -o path and fails as curl -f does when
// there is none. It answers the latest-release lookup (-w) with FAKE_LATEST,
// logs every URL to FAKE_CURL_LOG, and refuses a call without
// --proto =https and --proto-redir =https.
const fakeCurl = `#!/bin/sh
out="" url="" w="" proto=0
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    -w) w="$2"; shift ;;
    --proto|--proto-redir)
      [ "$2" = '=https' ] || { echo "fake curl: $1 $2" >&2; exit 2; }
      proto=$((proto + 1)); shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
[ "$proto" = 2 ] || { echo "fake curl: not https-only: $url" >&2; exit 2; }
echo "$url" >> "$FAKE_CURL_LOG"
if [ -n "$w" ]; then printf '%s' "$FAKE_LATEST"; exit 0; fi
src="$FAKE_RELEASE/${url##*/}"
[ -f "$src" ] || { echo "curl: (22) The requested URL returned error: 404" >&2; exit 22; }
cat "$src" > "$out"
`

// fakeUname answers -s and -m with FAKE_UNAME_S and FAKE_UNAME_M.
const fakeUname = `#!/bin/sh
case "$1" in -s) echo "$FAKE_UNAME_S" ;; -m) echo "$FAKE_UNAME_M" ;; *) exit 2 ;; esac
`

// fakeInstaller is the archive's substrate-eks: it writes its arguments to
// FAKE_ARGS, one a line.
const fakeInstaller = `#!/bin/sh
for a; do printf '%s\n' "$a"; done > "$FAKE_ARGS"
`

// releaseArchive is a tar.gz holding the fake installer as substrate-eks.
func releaseArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"substrate-eks": fakeInstaller, "README.md": "# release\n"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// install.sh --from-release, with a fake curl serving a local release and
// no git, make or go on PATH: it downloads the archive for this OS and CPU
// (the latest when no version is named), extracts it once its sha256 matches
// checksums.txt, and execs its substrate-eks with the arguments after
// --from-release and the version; a wrong checksum, a missing checksum line,
// a line with a field after the archive's name, the archive listed twice and
// a tag that is not one are refused before anything is extracted.
func TestLauncherFromReleaseVerifiesTheArchive(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs bash and a POSIX /bin/sh")
	}
	script, err := filepath.Abs("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{}
	for _, name := range []string{"tar", "awk", "mktemp", "rm", "rmdir", "mkdir", "cat", "gzip", "sha256sum", "shasum"} {
		if p, err := exec.LookPath(name); err == nil {
			tools[name] = p
		} else if name != "gzip" && name != "sha256sum" && name != "shasum" {
			t.Skipf("needs %s", name)
		}
	}
	if tools["sha256sum"] == "" && tools["shasum"] == "" {
		t.Skip("needs sha256sum or shasum")
	}

	const base = "https://github.com/csantanapr/eks-agent-substrate/releases"
	archive := releaseArchive(t)
	sum := sha256.Sum256(archive)
	good := hex.EncodeToString(sum[:])
	wrong := strings.Repeat("0", 64)
	names := []string{"substrate-eks_9.8.7_linux_amd64.tar.gz", "substrate-eks_9.8.7_linux_arm64.tar.gz", "substrate-eks_9.8.7_darwin_arm64.tar.gz",
		"substrate-eks_9.8.7-SNAPSHOT-abc1234_linux_amd64.tar.gz"}
	sums := func(skip, bad string) string {
		var b strings.Builder
		for _, n := range names {
			switch n {
			case skip:
			case bad:
				fmt.Fprintf(&b, "%s  %s\n", wrong, n)
			default:
				fmt.Fprintf(&b, "%s  %s\n", good, n)
			}
		}
		return b.String()
	}

	for _, c := range []struct {
		name, unameS, unameM, checksums string
		args                            []string
		tag                             string   // the release it uses; "" is v9.8.7
		archive                         string   // the one it must download
		latest                          bool     // it looks up the latest release first
		passed                          []string // the installer's arguments
		refusal                         string   // "" for a run that execs the installer
	}{
		{name: "named version", unameS: "Linux", unameM: "x86_64", checksums: sums("", ""),
			args: []string{"--from-release", "v9.8.7", "--dry-run", "--region", "us-east-1"}, archive: names[0],
			passed: []string{"--dry-run", "--region", "us-east-1"}},
		{name: "latest", unameS: "Darwin", unameM: "arm64", checksums: sums("", ""),
			args: []string{"--from-release", "--dry-run"}, archive: names[2], latest: true, passed: []string{"--dry-run"}},
		{name: "aarch64, no installer flags", unameS: "Linux", unameM: "aarch64", checksums: sums("", ""),
			args: []string{"--from-release", "v9.8.7"}, archive: names[1], passed: nil},
		{name: "pre-release tag", unameS: "Linux", unameM: "x86_64", checksums: sums("", ""),
			args: []string{"--from-release", "v9.8.7-SNAPSHOT-abc1234", "--version"}, tag: "v9.8.7-SNAPSHOT-abc1234",
			archive: names[3], passed: []string{"--version"}},
		{name: "wrong checksum", unameS: "Linux", unameM: "x86_64", checksums: sums("", names[0]),
			args: []string{"--from-release", "v9.8.7", "--dry-run"}, archive: names[0],
			refusal: "has sha256 " + good + ", but the v9.8.7 checksums.txt says " + wrong + "; refusing it"},
		{name: "no checksum line", unameS: "Linux", unameM: "x86_64", checksums: sums(names[0], ""),
			args: []string{"--from-release", "v9.8.7", "--dry-run"}, archive: names[0],
			refusal: "checksums.txt has no sha256 line for " + names[0] + "; refusing it"},
		{name: "a field after the archive's name", unameS: "Linux", unameM: "x86_64",
			checksums: sums(names[0], "") + good + "  " + names[0] + " trailing\n",
			args:      []string{"--from-release", "v9.8.7", "--dry-run"}, archive: names[0],
			refusal: "checksums.txt has no sha256 line for " + names[0] + "; refusing it"},
		{name: "the archive listed twice", unameS: "Linux", unameM: "x86_64", checksums: sums("", "") + good + "  " + names[0] + "\n",
			args: []string{"--from-release", "v9.8.7", "--dry-run"}, archive: names[0],
			refusal: names[0] + " is listed 2 times in the v9.8.7 checksums.txt; refusing it"},
		{name: "not a tag", unameS: "Linux", unameM: "x86_64", checksums: sums("", ""),
			args:    []string{"--from-release", "9.8.7/../x", "--dry-run"},
			refusal: "'9.8.7/../x' is not a substrate-eks release tag"},
	} {
		dir := t.TempDir()
		bin, release, home, tmp := filepath.Join(dir, "bin"), filepath.Join(dir, "release"), filepath.Join(dir, "home"), filepath.Join(dir, "tmp")
		for _, d := range []string{bin, release, home, tmp} {
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for name, body := range map[string]string{"curl": fakeCurl, "uname": fakeUname} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for tool, p := range tools {
			if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(release, n), archive, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(release, "checksums.txt"), []byte(c.checksums), 0o644); err != nil {
			t.Fatal(err)
		}
		curlLog, argsOut := filepath.Join(dir, "curl.log"), filepath.Join(dir, "args")

		cmd := exec.Command(bash, append([]string{script}, c.args...)...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + bin, "HOME=" + home, "TMPDIR=" + tmp,
			"FAKE_RELEASE=" + release, "FAKE_CURL_LOG=" + curlLog, "FAKE_ARGS=" + argsOut,
			"FAKE_LATEST=" + base + "/tag/v9.8.7", "FAKE_UNAME_S=" + c.unameS, "FAKE_UNAME_M=" + c.unameM}
		b, runErr := cmd.CombinedOutput()
		out := string(b)

		tag := c.tag
		if tag == "" {
			tag = "v9.8.7"
		}
		// Releases go beside the source mode's clone directory, never in it:
		// git refuses to clone into a non-empty ~/.substrate-eks.
		releases := filepath.Join(home, ".substrate-eks-releases")
		extracted := filepath.Join(releases, tag)
		if _, err := os.Stat(filepath.Join(home, ".substrate-eks")); err == nil {
			t.Errorf("%s: the release mode wrote into ~/.substrate-eks, where the source mode clones", c.name)
		}
		_, statErr := os.Stat(filepath.Join(extracted, "substrate-eks"))
		passed, argsErr := os.ReadFile(argsOut)
		logged, _ := os.ReadFile(curlLog)
		var urls []string
		if s := strings.TrimSpace(string(logged)); s != "" {
			urls = strings.Split(s, "\n")
		}
		left, _ := os.ReadDir(tmp)
		if len(left) != 0 {
			t.Errorf("%s: downloads left in TMPDIR: %v", c.name, left)
		}

		var wantURLs []string
		if c.latest {
			wantURLs = append(wantURLs, base+"/latest")
		}
		if c.archive != "" {
			wantURLs = append(wantURLs, base+"/download/"+tag+"/"+c.archive, base+"/download/"+tag+"/checksums.txt")
		}
		if !slices.Equal(urls, wantURLs) {
			t.Errorf("%s: downloaded %q, want %q\n%s", c.name, urls, wantURLs, out)
		}

		if c.refusal != "" {
			if runErr == nil || !strings.Contains(out, c.refusal) {
				t.Errorf("%s: want the refusal %q:\n%s", c.name, c.refusal, out)
			}
			if _, err := os.Stat(releases); err == nil {
				t.Errorf("%s: made %s after a refusal", c.name, releases)
			}
			if argsErr == nil {
				t.Errorf("%s: ran the installer after a refusal (args %q)", c.name, passed)
			}
			continue
		}
		if runErr != nil || statErr != nil || argsErr != nil {
			t.Errorf("%s: exit %v, extracted %v, installer ran %v:\n%s", c.name, runErr, statErr, argsErr, out)
			continue
		}
		var got []string
		if s := strings.TrimSuffix(string(passed), "\n"); s != "" {
			got = strings.Split(s, "\n")
		}
		if !slices.Equal(got, c.passed) {
			t.Errorf("%s: the installer got %q, want %q (no --from-release, no version)", c.name, got, c.passed)
		}
		if !strings.Contains(out, "sha256 matches checksums.txt: "+good) {
			t.Errorf("%s: no checksum line:\n%s", c.name, out)
		}
	}
}

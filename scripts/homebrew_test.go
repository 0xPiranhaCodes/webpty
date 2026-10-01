package scripts

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var formulaPlatforms = []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"}

func writeChecksums(t *testing.T, version string, skip string) (string, map[string]string) {
	t.Helper()
	sums := map[string]string{}
	var b strings.Builder
	for i, platform := range formulaPlatforms {
		name := "webpty_" + version + "_" + platform + ".tar.gz"
		sums[platform] = strings.Repeat(fmt.Sprintf("%x", i+10), 64)
		if platform != skip {
			fmt.Fprintf(&b, "%s  %s\n", sums[platform], name)
		}
	}
	fmt.Fprintf(&b, "%s  webpty_%s_checksums.sbom.json\n", strings.Repeat("f", 64), version)
	path := filepath.Join(t.TempDir(), "checksums.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, sums
}

func renderFormula(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"homebrew-formula.sh"}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), stdout.String(), stderr.String()
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, stdout.String(), stderr.String()
}

func TestFormulaPinsEveryArchiveToItsPublishedChecksum(t *testing.T) {
	checksums, sums := writeChecksums(t, "1.4.0", "")
	code, formula, stderr := renderFormula(t, "1.4.0", checksums)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	base := "https://github.com/0xPiranhaCodes/webpty/releases/download/v1.4.0/"
	for _, platform := range formulaPlatforms {
		url := fmt.Sprintf("url %q", base+"webpty_1.4.0_"+platform+".tar.gz")
		i := strings.Index(formula, url)
		if i < 0 {
			t.Fatalf("formula lacks %s:\n%s", url, formula)
		}
		// Each url is immediately followed by its own checksum.
		next := strings.SplitN(formula[i+len(url):], "\n", 3)
		if len(next) < 2 || strings.TrimSpace(next[1]) != fmt.Sprintf("sha256 %q", sums[platform]) {
			t.Errorf("%s is not followed by its sha256 %s:\n%s", platform, sums[platform], formula)
		}
	}
	for _, want := range []string{`version "1.4.0"`, `license "MIT"`, `bin.install "webpty"`, "on_macos do", "on_linux do", "on_arm do", "on_intel do", `shell_output("#{bin}/webpty version")`} {
		if !strings.Contains(formula, want) {
			t.Errorf("formula lacks %q", want)
		}
	}
	if ruby, err := exec.LookPath("ruby"); err == nil {
		path := filepath.Join(t.TempDir(), "webpty.rb")
		if err := os.WriteFile(path, []byte(formula), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(ruby, "-c", path).CombinedOutput(); err != nil {
			t.Errorf("ruby -c: %v\n%s", err, out)
		}
	}
}

func TestFormulaCanPointAtALocalSnapshot(t *testing.T) {
	checksums, _ := writeChecksums(t, "0.0.0-SNAPSHOT-abc1234", "")
	dir := t.TempDir()
	code, formula, stderr := renderFormula(t, "--local-dir", dir, "0.0.0-SNAPSHOT-abc1234", checksums)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(formula, `url "file://`+dir+`/webpty_0.0.0-SNAPSHOT-abc1234_darwin_arm64.tar.gz"`) {
		t.Errorf("formula does not use the local snapshot:\n%s", formula)
	}
}

func assemble(t *testing.T, dist string) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "release-assemble.sh", dist)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "WEBPTY_REPOSITORY=example/webpty"}
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func TestAssemblyListsTheFormulaInTheChecksumsItIsPinnedTo(t *testing.T) {
	checksums, sums := writeChecksums(t, "1.4.0", "")
	dist := t.TempDir()
	original, err := os.ReadFile(checksums)
	if err != nil {
		t.Fatal(err)
	}
	listed := filepath.Join(dist, "webpty_1.4.0_checksums.txt")
	if err := os.WriteFile(listed, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := assemble(t, dist); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	formula, err := os.ReadFile(filepath.Join(dist, "webpty.rb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`version "1.4.0"`, "https://github.com/example/webpty/releases/download/v1.4.0/", sums["darwin_arm64"]} {
		if !strings.Contains(string(formula), want) {
			t.Errorf("webpty.rb lacks %q", want)
		}
	}
	after, _ := os.ReadFile(listed)
	if want := string(original) + sum(formula) + "  webpty.rb\n"; string(after) != want {
		t.Errorf("checksums after assembly:\n%s\nwant:\n%s", after, want)
	}
	if code, out := assemble(t, dist); code == 0 || !strings.Contains(out, "already") {
		t.Errorf("a second assembly exit %d:\n%s", code, out)
	}
	if again, _ := os.ReadFile(listed); !bytes.Equal(again, after) {
		t.Error("a refused second assembly changed the checksums")
	}
}

// releaseDist writes a dist/ directory with real archives and checksums.
func releaseDist(t *testing.T, version string) string {
	t.Helper()
	rel := &release{files: map[string][]byte{}}
	rel.addVersion(t, version)
	dist := t.TempDir()
	for name, body := range rel.files {
		if err := os.WriteFile(filepath.Join(dist, filepath.Base(name)), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

func imageBinaries(t *testing.T, dist, out string) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "image-binaries.sh", dist, out)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	combined, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(combined)
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, string(combined)
}

func TestImageBinariesComeFromTheVerifiedLinuxArchives(t *testing.T) {
	dist := releaseDist(t, "1.4.0")
	out := filepath.Join(t.TempDir(), "image")
	if code, output := imageBinaries(t, dist, out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, output)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		info, err := os.Stat(filepath.Join(out, "linux_"+arch, "webpty"))
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("linux_%s/webpty: %v %v", arch, info, err)
		}
	}

	tampered := releaseDist(t, "1.4.0")
	if err := os.WriteFile(filepath.Join(tampered, "webpty_1.4.0_linux_arm64.tar.gz"), archive(t, map[string][]byte{"webpty": []byte("evil")}), 0o644); err != nil {
		t.Fatal(err)
	}
	out = filepath.Join(t.TempDir(), "image")
	if code, output := imageBinaries(t, tampered, out); code == 0 || !strings.Contains(output, "checksum") {
		t.Fatalf("exit %d with a tampered archive:\n%s", code, output)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("produced image binaries from a tampered archive")
	}
}

func TestFormulaRefusesIncompleteOrUnsafeInput(t *testing.T) {
	complete, _ := writeChecksums(t, "1.4.0", "")
	missing, _ := writeChecksums(t, "1.4.0", "darwin_arm64")
	for name, args := range map[string][]string{
		"missing archive checksum": {"1.4.0", missing},
		"invalid version":          {"1.4.0\"; system \"id", complete},
		"short version":            {"1.4", complete},
		"missing checksums file":   {"1.4.0", filepath.Join(t.TempDir(), "nope")},
		"relative local dir":       {"--local-dir", "dist", "1.4.0", complete},
		"no arguments":             {},
	} {
		t.Run(name, func(t *testing.T) {
			code, formula, _ := renderFormula(t, args...)
			if code == 0 || formula != "" {
				t.Fatalf("exit %d, formula:\n%s", code, formula)
			}
		})
	}
}

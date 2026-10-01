package scripts

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBrew records every call and keeps a fake Homebrew repository with
// taps under Library/Taps, the way brew --repository USER/REPO reports them.
const fakeBrew = `#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_BREW_LOG"
case $1 in
--repository)
	user=${2%%/*}
	repo=${2#*/}
	echo "$FAKE_BREW_ROOT/Library/Taps/$user/homebrew-$repo"
	;;
commands)
	echo install
	echo upgrade
	[ -z "${FAKE_BREW_TRUST:-}" ] || echo trust
	;;
list) [ -f "$FAKE_BREW_ROOT/installed" ] ;;
install) touch "$FAKE_BREW_ROOT/installed" ;;
esac
`

const fakeCosign = `#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_COSIGN_LOG"
exit "${FAKE_COSIGN_EXIT:-0}"
`

const tapFormula = "webpty-local/webpty/webpty"

type tapper struct {
	t       *testing.T
	release *release
	root    string // fake Homebrew repository
	bin     string
	env     []string
}

func formulaFor(version string) []byte {
	return []byte(fmt.Sprintf("class Webpty < Formula\n  version %q\n  license \"MIT\"\nend\n", version))
}

// addFormula publishes webpty.rb, lists it in the checksums, and adds a
// Sigstore bundle for the checksums.
func (r *release) addFormula(t *testing.T, version string, formula []byte) {
	t.Helper()
	key := "v" + version + "/webpty_" + version + "_checksums.txt"
	var checksums strings.Builder
	for _, line := range strings.SplitAfter(string(r.mustFile(t, key)), "\n") {
		if line != "" && !strings.HasSuffix(line, "  webpty.rb\n") {
			checksums.WriteString(line)
		}
	}
	fmt.Fprintf(&checksums, "%s  webpty.rb\n", sum(formula))
	r.set(key, []byte(checksums.String()))
	r.set("v"+version+"/webpty.rb", formula)
	r.set("v"+version+"/webpty_"+version+"_checksums.txt.sigstore.json", []byte(`{"bundle":"fake"}`))
}

func newTapper(t *testing.T) *tapper {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl unavailable")
	}
	rel := &release{latest: "1.2.3", files: map[string][]byte{}}
	for _, v := range []string{"1.2.3", "1.0.0"} {
		rel.addVersion(t, v)
		rel.addFormula(t, v, formulaFor(v))
	}
	server := httptest.NewTLSServer(rel)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fakebin")
	root := filepath.Join(dir, "homebrew")
	for _, d := range []string{bin, root} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"brew": fakeBrew, "cosign": fakeCosign} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &tapper{
		t: t, release: rel, root: root, bin: bin,
		env: []string{
			"PATH=" + bin + ":/usr/bin:/bin",
			"HOME=" + dir,
			"CURL_CA_BUNDLE=" + ca,
			"SSL_CERT_FILE=" + ca,
			"WEBPTY_RELEASES_URL=" + server.URL + "/releases",
			"FAKE_BREW_ROOT=" + root,
			"FAKE_BREW_LOG=" + filepath.Join(dir, "brew.log"),
			"FAKE_COSIGN_LOG=" + filepath.Join(dir, "cosign.log"),
			"FAKE_BREW_TRUST=1",
		},
	}
}

func (tp *tapper) setenv(kv string) {
	key := kv[:strings.Index(kv, "=")+1]
	for i, old := range tp.env {
		if strings.HasPrefix(old, key) {
			tp.env[i] = kv
			return
		}
	}
	tp.env = append(tp.env, kv)
}

func (tp *tapper) run(args ...string) (int, string) {
	tp.t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"homebrew-tap.sh"}, args...)...)
	cmd.Env = tp.env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), out.String()
	} else if err != nil {
		tp.t.Fatal(err)
	}
	return 0, out.String()
}

func (tp *tapper) logOf(name string) string {
	data, _ := os.ReadFile(filepath.Join(filepath.Dir(tp.root), name+".log"))
	return string(data)
}

func (tp *tapper) tapFile() string {
	return filepath.Join(tp.root, "Library", "Taps", "webpty-local", "homebrew-webpty", "Formula", "webpty.rb")
}

func (tp *tapper) installedOrUpgraded() bool {
	log := tp.logOf("brew")
	return strings.Contains(log, "install ") || strings.Contains(log, "upgrade ")
}

func TestTapInstallsTheVerifiedReleaseFormulaByName(t *testing.T) {
	tp := newTapper(t)
	code, out := tp.run()
	if code != 0 {
		t.Fatalf("homebrew-tap.sh exit %d:\n%s", code, out)
	}
	got, err := os.ReadFile(tp.tapFile())
	if err != nil {
		t.Fatalf("formula not placed in the local tap: %v\n%s", err, out)
	}
	if !bytes.Equal(got, formulaFor("1.2.3")) {
		t.Errorf("tap holds %q", got)
	}
	brew := tp.logOf("brew")
	for _, want := range []string{"trust --formula " + tapFormula, "install --formula " + tapFormula} {
		if !strings.Contains(brew, want) {
			t.Errorf("brew was not called with %q:\n%s", want, brew)
		}
	}
	// tap-new turns on Homebrew developer mode as a side effect.
	if strings.Contains(brew, "tap-new") || strings.Contains(brew, "upgrade") {
		t.Errorf("unexpected brew calls:\n%s", brew)
	}
	cosign := tp.logOf("cosign")
	for _, want := range []string{
		"verify-blob",
		"--bundle ",
		"--certificate-identity https://github.com/0xPiranhaCodes/webpty/.github/workflows/release.yml@refs/tags/v1.2.3",
		"--certificate-oidc-issuer https://token.actions.githubusercontent.com",
		"webpty_1.2.3_checksums.txt",
	} {
		if !strings.Contains(cosign, want) {
			t.Errorf("cosign was not called with %q:\n%s", want, cosign)
		}
	}
}

func TestTapUpgradesAnInstalledFormulaByName(t *testing.T) {
	tp := newTapper(t)
	if code, out := tp.run("--version", "1.0.0"); code != 0 {
		t.Fatalf("install exit %d:\n%s", code, out)
	}
	code, out := tp.run()
	if code != 0 {
		t.Fatalf("upgrade exit %d:\n%s", code, out)
	}
	if got, _ := os.ReadFile(tp.tapFile()); !bytes.Equal(got, formulaFor("1.2.3")) {
		t.Errorf("tap was not updated to 1.2.3: %q", got)
	}
	brew := tp.logOf("brew")
	if !strings.Contains(brew, "upgrade --formula "+tapFormula) {
		t.Errorf("an installed formula was not upgraded by name:\n%s", brew)
	}
}

func TestTapRefusesAFormulaThatDoesNotMatchTheChecksums(t *testing.T) {
	tp := newTapper(t)
	previous := []byte("previous formula\n")
	if err := os.MkdirAll(filepath.Dir(tp.tapFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tp.tapFile(), previous, 0o644); err != nil {
		t.Fatal(err)
	}
	tp.release.set("v1.2.3/webpty.rb", []byte("class Webpty < Formula\n  version \"1.2.3\"\n  def install; system \"id\"; end\nend\n"))
	code, out := tp.run()
	if code == 0 || !strings.Contains(out, "checksum") {
		t.Fatalf("exit %d with a tampered formula:\n%s", code, out)
	}
	if got, _ := os.ReadFile(tp.tapFile()); !bytes.Equal(got, previous) {
		t.Errorf("the tap formula was replaced with %q", got)
	}
	if tp.installedOrUpgraded() {
		t.Errorf("brew ran with an unverified formula:\n%s", tp.logOf("brew"))
	}
}

func TestTapRefusesAFormulaMissingFromTheChecksums(t *testing.T) {
	tp := newTapper(t)
	key := "v1.2.3/webpty_1.2.3_checksums.txt"
	var kept []string
	for _, line := range strings.Split(string(tp.release.mustFile(t, key)), "\n") {
		if !strings.HasSuffix(line, "webpty.rb") {
			kept = append(kept, line)
		}
	}
	tp.release.set(key, []byte(strings.Join(kept, "\n")))
	code, out := tp.run()
	if code == 0 || !strings.Contains(out, "webpty.rb") || tp.installedOrUpgraded() {
		t.Fatalf("exit %d without a webpty.rb checksum:\n%s", code, out)
	}
}

func TestTapRefusesWhenTheSignatureDoesNotVerify(t *testing.T) {
	tp := newTapper(t)
	tp.setenv("FAKE_COSIGN_EXIT=1")
	code, out := tp.run()
	if code == 0 || !strings.Contains(out, "signature") || tp.installedOrUpgraded() {
		t.Fatalf("exit %d with a failing signature:\n%s", code, out)
	}
	if _, err := os.Stat(tp.tapFile()); err == nil {
		t.Error("an unverified formula was placed in the tap")
	}
}

func TestTapRequiresCosignUnlessVerificationIsExplicitlySkipped(t *testing.T) {
	tp := newTapper(t)
	if err := os.Remove(filepath.Join(tp.bin, "cosign")); err != nil {
		t.Fatal(err)
	}
	code, out := tp.run()
	if code == 0 || !strings.Contains(out, "cosign") || tp.installedOrUpgraded() {
		t.Fatalf("exit %d without cosign:\n%s", code, out)
	}
	code, out = tp.run("--skip-signature-verification")
	if code != 0 || !strings.Contains(out, "not verifying") {
		t.Fatalf("exit %d when skipping signature verification:\n%s", code, out)
	}
}

func TestTapRefusesAFormulaForAnotherVersion(t *testing.T) {
	tp := newTapper(t)
	tp.release.addFormula(t, "1.2.3", formulaFor("9.9.9"))
	code, out := tp.run()
	if code == 0 || !strings.Contains(out, "version") || tp.installedOrUpgraded() {
		t.Fatalf("exit %d with a formula for another version:\n%s", code, out)
	}
}

func TestTapWorksWithHomebrewBeforeTapTrust(t *testing.T) {
	tp := newTapper(t)
	tp.setenv("FAKE_BREW_TRUST=")
	code, out := tp.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if brew := tp.logOf("brew"); strings.Contains(brew, "trust ") || !strings.Contains(brew, "install --formula "+tapFormula) {
		t.Errorf("unexpected brew calls without tap trust:\n%s", brew)
	}
}

func TestTapInstallsFromALocalDirectory(t *testing.T) {
	tp := newTapper(t)
	dir := t.TempDir()
	for _, name := range []string{"webpty.rb", "webpty_1.0.0_checksums.txt", "webpty_1.0.0_checksums.txt.sigstore.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), tp.release.mustFile(t, "v1.0.0/"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, out := tp.run("--from-dir", dir, "--version", "1.0.0", "--tap", "webpty-local/validate")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if len(tp.release.log()) != 0 {
		t.Errorf("downloaded %v with --from-dir", tp.release.log())
	}
	if !strings.Contains(tp.logOf("brew"), "install --formula webpty-local/validate/webpty") {
		t.Errorf("did not install from the named tap:\n%s", tp.logOf("brew"))
	}
}

func TestTapRefusesUnsafeInput(t *testing.T) {
	for name, args := range map[string][]string{
		"invalid version":     {"--version", "1.2"},
		"injected version":    {"--version", "1.2.3;id"},
		"relative from-dir":   {"--from-dir", "dist", "--version", "1.2.3"},
		"from-dir no version": {"--from-dir", "/tmp"},
		"bad tap":             {"--tap", "../evil"},
		"unknown argument":    {"--force"},
	} {
		t.Run(name, func(t *testing.T) {
			tp := newTapper(t)
			code, out := tp.run(args...)
			if code == 0 || len(tp.release.log()) != 0 || tp.logOf("brew") != "" {
				t.Fatalf("exit %d, requests %v, brew %q:\n%s", code, tp.release.log(), tp.logOf("brew"), out)
			}
		})
	}
	tp := newTapper(t)
	tp.setenv("WEBPTY_RELEASES_URL=http://example.invalid/releases")
	if code, out := tp.run(); code == 0 || !strings.Contains(out, "https") {
		t.Fatalf("exit %d with an http release URL:\n%s", code, out)
	}
}

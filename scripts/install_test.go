package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// release serves a fake GitHub release layout over HTTPS:
// /latest redirects to /tag/v<latest>, and /download/v<version>/<file>
// serves archives and checksums.
type release struct {
	latest   string
	files    map[string][]byte // path below /download/
	mu       sync.Mutex
	requests []string
}

func (r *release) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.URL.Path)
	r.mu.Unlock()
	switch {
	case req.URL.Path == "/releases/latest":
		http.Redirect(w, req, "/releases/tag/v"+r.latest, http.StatusFound)
	case strings.HasPrefix(req.URL.Path, "/releases/tag/"):
		fmt.Fprintln(w, "release page")
	case strings.HasPrefix(req.URL.Path, "/releases/download/"):
		body, ok := r.file(strings.TrimPrefix(req.URL.Path, "/releases/download/"))
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	default:
		http.NotFound(w, req)
	}
}

func (r *release) set(name string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[name] = body
}

func (r *release) file(name string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, ok := r.files[name]
	return body, ok
}

func (r *release) mustFile(t *testing.T, name string) []byte {
	t.Helper()
	body, ok := r.file(name)
	if !ok {
		t.Fatalf("no release file %s", name)
	}
	return body
}

func (r *release) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func (r *release) requested(path string) bool {
	for _, p := range r.log() {
		if p == path {
			return true
		}
	}
	return false
}

// fakeBinary is the webpty "executable" inside test archives.
func fakeBinary(version string) []byte {
	return []byte("#!/bin/sh\necho \"webpty " + version + " (fake)\"\n")
}

func archive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
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

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// addVersion publishes archives for every supported platform and their
// checksums file.
func (r *release) addVersion(t *testing.T, version string) {
	t.Helper()
	var checksums strings.Builder
	for _, platform := range []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"} {
		name := "webpty_" + version + "_" + platform + ".tar.gz"
		body := archive(t, map[string][]byte{"webpty": fakeBinary(version), "LICENSE": []byte("MIT\n")})
		r.set("v"+version+"/"+name, body)
		fmt.Fprintf(&checksums, "%s  %s\n", sum(body), name)
	}
	r.set("v"+version+"/webpty_"+version+"_checksums.txt", []byte(checksums.String()))
}

func hostPlatform(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install.sh supports macOS and Linux only")
	}
	return runtime.GOOS + "_" + runtime.GOARCH
}

type installer struct {
	t       *testing.T
	release *release
	url     string
	home    string
	env     []string
}

func newInstaller(t *testing.T) *installer {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl unavailable")
	}
	rel := &release{latest: "1.2.3", files: map[string][]byte{}}
	rel.addVersion(t, "1.2.3")
	rel.addVersion(t, "1.0.0")
	server := httptest.NewTLSServer(rel)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(ca, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return &installer{
		t:       t,
		release: rel,
		url:     server.URL + "/releases",
		home:    home,
		env: []string{
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
			"HOME=" + home,
			"CURL_CA_BUNDLE=" + ca,
			"SSL_CERT_FILE=" + ca,
			"WEBPTY_RELEASES_URL=" + server.URL + "/releases",
		},
	}
}

// fakeUname makes uname report the given kernel and machine.
func (in *installer) fakeUname(kernel, machine string) {
	in.t.Helper()
	dir := filepath.Join(in.t.TempDir(), "fakebin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		in.t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo %s ;; -m) echo %s ;; *) echo %s ;; esac\n", kernel, machine, kernel)
	if err := os.WriteFile(filepath.Join(dir, "uname"), []byte(script), 0o755); err != nil {
		in.t.Fatal(err)
	}
	for i, kv := range in.env {
		if strings.HasPrefix(kv, "PATH=") {
			in.env[i] = "PATH=" + dir + ":" + strings.TrimPrefix(kv, "PATH=")
		}
	}
}

func (in *installer) run(args ...string) (int, string) {
	in.t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"install.sh"}, args...)...)
	cmd.Env = in.env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), out.String()
	} else if err != nil {
		in.t.Fatal(err)
	}
	return 0, out.String()
}

func TestInstallsTheLatestReleaseIntoAUserPrefixWithoutRoot(t *testing.T) {
	platform := hostPlatform(t)
	in := newInstaller(t)
	code, out := in.run()
	if code != 0 {
		t.Fatalf("install.sh exit %d:\n%s", code, out)
	}
	installed := filepath.Join(in.home, ".local", "bin", "webpty")
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("installed mode %o, want 755", info.Mode().Perm())
	}
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeBinary("1.2.3")) {
		t.Errorf("installed the wrong binary:\n%s", got)
	}
	if !in.release.requested("/releases/download/v1.2.3/webpty_1.2.3_" + platform + ".tar.gz") {
		t.Errorf("did not download the %s archive", platform)
	}
	for _, want := range []string{"v1.2.3", "SHA-256", "webpty 1.2.3 (fake)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestInstallsAnExactVersion(t *testing.T) {
	platform := hostPlatform(t)
	for _, version := range []string{"1.0.0", "v1.0.0"} {
		t.Run(version, func(t *testing.T) {
			in := newInstaller(t)
			bin := filepath.Join(in.home, "bin")
			code, out := in.run("--version", version, "--bin-dir", bin)
			if code != 0 {
				t.Fatalf("install.sh exit %d:\n%s", code, out)
			}
			if in.release.requested("/releases/latest") {
				t.Error("an exact version must not look up the latest release")
			}
			if !in.release.requested("/releases/download/v1.0.0/webpty_1.0.0_" + platform + ".tar.gz") {
				t.Error("did not download the requested version")
			}
			got, _ := os.ReadFile(filepath.Join(bin, "webpty"))
			if !bytes.Equal(got, fakeBinary("1.0.0")) {
				t.Errorf("installed %q", got)
			}
		})
	}
}

func TestRefusesAnArchiveWhoseChecksumDoesNotMatch(t *testing.T) {
	platform := hostPlatform(t)
	in := newInstaller(t)
	bin := filepath.Join(in.home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := []byte("previous install\n")
	if err := os.WriteFile(filepath.Join(bin, "webpty"), previous, 0o755); err != nil {
		t.Fatal(err)
	}
	in.release.set("v1.2.3/webpty_1.2.3_"+platform+".tar.gz", archive(t, map[string][]byte{"webpty": []byte("#!/bin/sh\necho tampered\n")}))

	code, out := in.run("--bin-dir", bin)
	if code == 0 {
		t.Fatalf("install.sh accepted a tampered archive:\n%s", out)
	}
	if !strings.Contains(out, "checksum") {
		t.Errorf("output does not explain the checksum failure:\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(bin, "webpty")); !bytes.Equal(got, previous) {
		t.Errorf("the previous install was replaced with %q", got)
	}
}

func TestRefusesAnArchiveMissingFromTheChecksums(t *testing.T) {
	platform := hostPlatform(t)
	in := newInstaller(t)
	key := "v1.2.3/webpty_1.2.3_checksums.txt"
	var kept []string
	for _, line := range strings.Split(string(in.release.mustFile(t, key)), "\n") {
		if !strings.Contains(line, platform) {
			kept = append(kept, line)
		}
	}
	in.release.set(key, []byte(strings.Join(kept, "\n")))
	code, out := in.run("--bin-dir", filepath.Join(in.home, "bin"))
	if code == 0 || !strings.Contains(out, "checksum") {
		t.Fatalf("install.sh exit %d without a checksum entry:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(in.home, "bin", "webpty")); err == nil {
		t.Error("installed an unverified binary")
	}
}

func TestRefusesUnsupportedPlatforms(t *testing.T) {
	hostPlatform(t)
	for _, tc := range []struct{ kernel, machine string }{
		{"MINGW64_NT-10.0", "x86_64"},
		{"FreeBSD", "amd64"},
		{"Linux", "i686"},
		{"Linux", "armv7l"},
		{"Darwin", "ppc"},
	} {
		t.Run(tc.kernel+"/"+tc.machine, func(t *testing.T) {
			in := newInstaller(t)
			in.fakeUname(tc.kernel, tc.machine)
			code, out := in.run("--bin-dir", filepath.Join(in.home, "bin"))
			if code == 0 || !strings.Contains(out, "unsupported") {
				t.Fatalf("install.sh exit %d on %s/%s:\n%s", code, tc.kernel, tc.machine, out)
			}
			if len(in.release.log()) != 0 {
				t.Errorf("downloaded %v before refusing the platform", in.release.log())
			}
		})
	}
}

func TestMapsPlatformNamesToArchives(t *testing.T) {
	hostPlatform(t)
	for _, tc := range []struct{ kernel, machine, platform string }{
		{"Darwin", "arm64", "darwin_arm64"},
		{"Darwin", "x86_64", "darwin_amd64"},
		{"Linux", "aarch64", "linux_arm64"},
		{"Linux", "x86_64", "linux_amd64"},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			in := newInstaller(t)
			in.fakeUname(tc.kernel, tc.machine)
			// The fake binary is a shell script, so it runs on any host.
			code, out := in.run("--bin-dir", filepath.Join(in.home, "bin"))
			if code != 0 {
				t.Fatalf("install.sh exit %d:\n%s", code, out)
			}
			if !in.release.requested("/releases/download/v1.2.3/webpty_1.2.3_" + tc.platform + ".tar.gz") {
				t.Errorf("did not download the %s archive: %v", tc.platform, in.release.log())
			}
		})
	}
}

func TestRefusesPlainHTTPDownloads(t *testing.T) {
	hostPlatform(t)
	in := newInstaller(t)
	for i, kv := range in.env {
		if strings.HasPrefix(kv, "WEBPTY_RELEASES_URL=") {
			in.env[i] = "WEBPTY_RELEASES_URL=" + strings.Replace(in.url, "https://", "http://", 1)
		}
	}
	code, out := in.run("--bin-dir", filepath.Join(in.home, "bin"))
	if code == 0 || !strings.Contains(out, "https") {
		t.Fatalf("install.sh exit %d with an http release URL:\n%s", code, out)
	}
}

func TestRefusesInvalidVersions(t *testing.T) {
	hostPlatform(t)
	for _, version := range []string{"latest-ish", "1.2", "1.2.3;id", "../1.2.3", ""} {
		in := newInstaller(t)
		code, out := in.run("--version", version, "--bin-dir", filepath.Join(in.home, "bin"))
		if code == 0 || len(in.release.log()) != 0 {
			t.Errorf("version %q: exit %d, requests %v:\n%s", version, code, in.release.log(), out)
		}
	}
}

func TestRefusesUnsafeDestinations(t *testing.T) {
	hostPlatform(t)
	in := newInstaller(t)
	shared := filepath.Join(in.home, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	sticky := filepath.Join(in.home, "sticky")
	if err := os.Mkdir(sticky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o1777); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(in.home, "linked")
	if err := os.Mkdir(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(linked, "webpty")); err != nil {
		t.Fatal(err)
	}
	notDir := filepath.Join(in.home, "file")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"relative":              "relative/bin",
		"dot-dot":               in.home + "/bin/../../escape",
		"world-writable":        shared,
		"world-writable sticky": sticky,
		"under world-writable":  filepath.Join(shared, "bin"),
		"symlinked binary":      linked,
		"not a directory":       notDir,
	} {
		t.Run(name, func(t *testing.T) {
			code, out := in.run("--bin-dir", dir)
			if code == 0 {
				t.Fatalf("install.sh installed into %s:\n%s", dir, out)
			}
		})
	}
	if target, _ := os.Readlink(filepath.Join(linked, "webpty")); target != "/bin/sh" {
		t.Error("the symlinked destination was modified")
	}
}

func TestExplainsASymlinkedBinary(t *testing.T) {
	hostPlatform(t)
	in := newInstaller(t)
	bin := filepath.Join(in.home, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(bin, "webpty")); err != nil {
		t.Fatal(err)
	}
	code, out := in.run("--bin-dir", bin)
	if code == 0 || !strings.Contains(out, "symbolic link") {
		t.Fatalf("exit %d for a symlinked webpty:\n%s", code, out)
	}
}

func TestRefusesASymlinkedBinDirByName(t *testing.T) {
	hostPlatform(t)
	in := newInstaller(t)
	real := filepath.Join(in.home, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(in.home, "bin")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	code, out := in.run("--bin-dir", link)
	if code == 0 || !strings.Contains(out, link+" is a symbolic link") || !strings.Contains(out, "--bin-dir "+resolved) {
		t.Fatalf("exit %d for a symlinked --bin-dir; want the link named and its target suggested:\n%s", code, out)
	}
	if strings.Contains(out, "other users") {
		t.Errorf("a private symlinked --bin-dir was reported as shared:\n%s", out)
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Errorf("installed through the link: %v", entries)
	}
}

func TestJudgesASymlinkedAncestorByItsTarget(t *testing.T) {
	hostPlatform(t)
	in := newInstaller(t)
	real := filepath.Join(in.home, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(in.home, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	if code, out := in.run("--bin-dir", filepath.Join(alias, "bin")); code != 0 {
		t.Fatalf("exit %d under a private symlinked ancestor:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(real, "bin", "webpty")); err != nil {
		t.Fatal(err)
	}

	shared := filepath.Join(in.home, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	sharedAlias := filepath.Join(in.home, "shared-alias")
	if err := os.Symlink(shared, sharedAlias); err != nil {
		t.Fatal(err)
	}
	if code, out := in.run("--bin-dir", filepath.Join(sharedAlias, "bin")); code == 0 || !strings.Contains(out, "other users") {
		t.Fatalf("exit %d under a symlink to a world-writable directory:\n%s", code, out)
	}
}

// fakeTool puts a script named name first on PATH.
func (in *installer) fakeTool(name, script string) {
	in.t.Helper()
	dir := filepath.Join(in.t.TempDir(), "tools")
	if err := os.Mkdir(dir, 0o755); err != nil {
		in.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		in.t.Fatal(err)
	}
	for i, kv := range in.env {
		if strings.HasPrefix(kv, "PATH=") {
			in.env[i] = "PATH=" + dir + ":" + strings.TrimPrefix(kv, "PATH=")
		}
	}
}

func TestRemovesTheStagedBinaryWhenTheInstallFails(t *testing.T) {
	hostPlatform(t)
	in := newInstaller(t)
	in.fakeTool("mv", "#!/bin/sh\nexit 1\n")
	bin := filepath.Join(in.home, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	code, out := in.run("--bin-dir", bin)
	if code == 0 {
		t.Fatalf("install.sh succeeded although mv failed:\n%s", out)
	}
	entries, _ := os.ReadDir(bin)
	for _, e := range entries {
		t.Errorf("left %s in %s", e.Name(), bin)
	}
}

func TestRechecksTheDestinationAfterCreatingIt(t *testing.T) {
	hostPlatform(t)
	in := newInstaller(t)
	// A mkdir that leaves what it creates writable by everyone.
	in.fakeTool("mkdir", "#!/bin/sh\n/bin/mkdir \"$@\" || exit\nfor a; do case $a in -*) ;; *) chmod 0777 \"$a\" ;; esac; done\n")
	bin := filepath.Join(in.home, "new", "bin")
	code, out := in.run("--bin-dir", bin)
	if code == 0 || !strings.Contains(out, "other users") {
		t.Fatalf("exit %d into a world-writable new directory:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "webpty")); err == nil {
		t.Error("installed into a directory other users can modify")
	}
}

func TestAcceptsPrereleaseVersionsWithHyphens(t *testing.T) {
	platform := hostPlatform(t)
	in := newInstaller(t)
	in.release.addVersion(t, "2.0.0-rc.1-hotfix")
	bin := filepath.Join(in.home, "bin")
	code, out := in.run("--version", "v2.0.0-rc.1-hotfix", "--bin-dir", bin)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !in.release.requested("/releases/download/v2.0.0-rc.1-hotfix/webpty_2.0.0-rc.1-hotfix_" + platform + ".tar.gz") {
		t.Error("did not download the prerelease")
	}
}

func TestInstallScriptPassesShellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck unavailable; CI runs it")
	}
	out, err := exec.Command("shellcheck", "-s", "sh", "install.sh", "homebrew-formula.sh", "homebrew-tap.sh",
		"homebrew-validate.sh", "release-assemble.sh", "image-binaries.sh", "verify-archives.sh", "docker-smoke.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}
}

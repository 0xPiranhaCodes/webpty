package repocheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Go server is the only webpty runtime. These paths belonged to the
// retired Python/Tornado implementation and its packaging.
// A trailing slash means only a directory counts: "go build ./cmd/webpty"
// leaves a webpty binary at the root, which is not the old package.
var legacyPaths = []string{
	"webpty/",
	"tests/iframe.html",
	"pyproject.toml",
	"poetry.lock",
	"setup.py",
	"setup.cfg",
	"MANIFEST.in",
	"Procfile",
	"requirements.txt",
	".python-version",
	".black.toml",
	".flake8",
	".pre-commit-config.yaml",
	".vscode/settings.json",
	".github/workflows/python-publish.yml",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	var files []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name == "" {
			continue
		}
		// Deleted but not yet staged files are still listed by --cached.
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			files = append(files, name)
		}
	}
	return files
}

func legacyPathPresent(root, name string) bool {
	info, err := os.Lstat(filepath.Join(root, strings.TrimSuffix(name, "/")))
	if err != nil {
		return false
	}
	return !strings.HasSuffix(name, "/") || info.IsDir()
}

func TestLegacyPythonRuntimeIsGone(t *testing.T) {
	root := repoRoot(t)
	for _, name := range legacyPaths {
		if legacyPathPresent(root, name) {
			t.Errorf("legacy Python path %s exists", name)
		}
	}
	for _, name := range trackedFiles(t, root) {
		if strings.HasSuffix(name, ".py") || strings.HasSuffix(name, ".pyc") {
			t.Errorf("Python source %s is in the repository", name)
		}
		if name == "webpty" {
			t.Error("webpty at the repository root is a compiled binary (a go build artifact); remove it from git")
		}
	}
	if info, err := os.Lstat(filepath.Join(root, "webpty")); err == nil && info.Mode().IsRegular() {
		t.Log("ignoring the build artifact ./webpty (go build output, ignored by git)")
	}
}

func TestABuiltBinaryIsNotMistakenForThePythonPackage(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "webpty"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if legacyPathPresent(root, "webpty/") {
		t.Error("a webpty binary at the root was reported as the legacy webpty/ package")
	}
	if err := os.Remove(filepath.Join(root, "webpty")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "webpty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !legacyPathPresent(root, "webpty/") {
		t.Error("the legacy webpty/ package directory was not detected")
	}
}

const modulePath = "github.com/0xPiranhaCodes/webpty"

// The module path is the repository's real origin, so go install and
// import paths resolve.
func TestModulePathIsTheOrigin(t *testing.T) {
	root := repoRoot(t)
	if !strings.HasPrefix(readRepoFile(t, "go.mod"), "module "+modulePath+"\n") {
		t.Errorf("go.mod does not declare module %s", modulePath)
	}
	stale := "github.com/" + "hackerearth/webpty"
	for _, name := range trackedFiles(t, root) {
		if strings.HasSuffix(name, ".png") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), stale) {
			t.Errorf("%s still names the old module path %s", name, stale)
		}
	}
}

var pythonMention = regexp.MustCompile(`(?i)\b(python|tornado|pip install|pypi|poetry)\b`)

// Workflows and user documentation must not build, publish, or describe the
// Python runtime. The migration guide, and links to it, are the only places
// that name it.
func TestWorkflowsAndDocsDoNotTargetPython(t *testing.T) {
	root := repoRoot(t)
	for _, name := range trackedFiles(t, root) {
		check := strings.HasPrefix(name, ".github/") ||
			name == "README.md" || name == "Makefile" || name == "Dockerfile" ||
			(strings.HasPrefix(name, "docs/") && !strings.HasPrefix(name, "docs/superpowers/"))
		if !check || name == "docs/migrating-from-python.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if pythonMention.MatchString(line) && !strings.Contains(line, "migrating-from-python.md") {
				t.Errorf("%s:%d mentions the Python runtime: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

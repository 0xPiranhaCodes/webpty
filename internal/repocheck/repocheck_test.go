// Package repocheck holds tests about the repository layout itself.
package repocheck

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The frontend's node_modules contains third-party Go code that must never
// be built, vetted, or tested as part of webpty.
func TestGoPackagesExcludeTheFrontend(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool unavailable")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	web := filepath.Join(root, "web") + string(filepath.Separator)
	for _, dir := range strings.Fields(string(out)) {
		if strings.HasPrefix(dir+string(filepath.Separator), web) {
			t.Errorf("go ./... includes %s", dir)
		}
	}
}

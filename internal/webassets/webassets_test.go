package webassets_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/webassets"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncReplacesStaleAssetsWithTheCompleteBuild(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "index.html"), "new index")
	write(t, filepath.Join(src, "assets", "index-new.js"), "js")
	write(t, filepath.Join(src, "assets", "_virtual-helper.js"), "underscore")
	write(t, filepath.Join(src, "assets", "fonts", "plex.woff2"), "font")
	write(t, filepath.Join(dst, ".gitkeep"), "")
	write(t, filepath.Join(dst, "index.html"), "old index")
	write(t, filepath.Join(dst, "assets", "index-old.js"), "stale")

	if err := webassets.Sync(src, dst); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		".gitkeep":                  "",
		"index.html":                "new index",
		"assets/index-new.js":       "js",
		"assets/_virtual-helper.js": "underscore",
		"assets/fonts/plex.woff2":   "font",
	}
	got := map[string]string{}
	err := filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dst, path)
		data, err := os.ReadFile(path)
		got[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	for name, data := range want {
		if got[name] != data {
			t.Errorf("%s = %q, want %q", name, got[name], data)
		}
	}
}

func TestSyncRefusesABuildWithoutIndex(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "assets", "a.js"), "js")
	write(t, filepath.Join(dst, "index.html"), "keep")
	if err := webassets.Sync(src, dst); err == nil {
		t.Fatal("Sync accepted a build without index.html")
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "index.html")); string(data) != "keep" {
		t.Fatalf("destination was modified: %q", data)
	}
}

func TestDistIsRootedAtTheBuildAndIncludesDotfiles(t *testing.T) {
	if _, err := fs.Stat(webassets.Dist(), ".gitkeep"); err != nil {
		t.Fatalf(".gitkeep not embedded at the root: %v", err)
	}
}

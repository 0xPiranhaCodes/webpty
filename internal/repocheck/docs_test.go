package repocheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPublicDocumentationLayout(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{
		"CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "SECURITY.md", "SUPPORT.md",
		"docs/specs/product.md",
	} {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("required public document %s is missing", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "superpowers")); !os.IsNotExist(err) {
		t.Error("tool-specific docs/superpowers must not exist")
	}
	var specs []string
	err := filepath.WalkDir(filepath.Join(root, "docs", "specs"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			specs = append(specs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || filepath.Base(specs[0]) != "product.md" {
		t.Errorf("docs/specs must contain only product.md, got %v", specs)
	}
	readme := readRepoFile(t, "README.md")
	for _, link := range []string{
		"CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "SECURITY.md", "SUPPORT.md",
		"docs/specs/product.md",
	} {
		if !strings.Contains(readme, "]("+link+")") {
			t.Errorf("README.md does not link %s", link)
		}
	}
}

var markdownLink = regexp.MustCompile(`!?\[[^]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

func TestTrackedMarkdownLinksResolve(t *testing.T) {
	root := repoRoot(t)
	for _, name := range trackedFiles(t, root) {
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		for _, match := range markdownLink.FindAllStringSubmatch(readRepoFile(t, name), -1) {
			target := match[1]
			if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") ||
				strings.Contains(target, "://") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			target, _, _ = strings.Cut(target, "?")
			if target == "" {
				continue
			}
			resolved := filepath.Clean(filepath.Join(root, filepath.Dir(name), filepath.FromSlash(target)))
			relative, err := filepath.Rel(root, resolved)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				t.Errorf("%s links outside the repository: %s", name, match[1])
				continue
			}
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s has broken local link %s: %v", name, match[1], err)
			}
		}
	}
}

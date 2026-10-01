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

var (
	markdownLink                = regexp.MustCompile(`!?\[[^]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	markdownReferenceDefinition = regexp.MustCompile(
		`(?m)^[ \t]{0,3}\[[^]\r\n]+\]:[ \t]*(?:<([^>\r\n]+)>|([^ \t\r\n]+))`,
	)
)

func TestTrackedMarkdownLinksResolve(t *testing.T) {
	root := repoRoot(t)
	for _, name := range trackedFiles(t, root) {
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		for _, target := range brokenLocalMarkdownLinks(root, name, readRepoFile(t, name)) {
			t.Errorf("%s has broken local link %s", name, target)
		}
	}
}

func brokenLocalMarkdownLinks(root, name, body string) []string {
	var targets []string
	for _, match := range markdownLink.FindAllStringSubmatch(body, -1) {
		targets = append(targets, match[1])
	}
	for _, match := range markdownReferenceDefinition.FindAllStringSubmatch(body, -1) {
		target := match[1]
		if target == "" {
			target = match[2]
		}
		targets = append(targets, target)
	}

	var broken []string
	for _, original := range targets {
		target := original
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
			broken = append(broken, original)
			continue
		}
		if _, err := os.Stat(resolved); err != nil {
			broken = append(broken, original)
		}
	}
	return broken
}

func TestBrokenLocalMarkdownLinksIncludesReferenceDefinitions(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "existing.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		"[existing]: existing.md",
		"[missing]: missing.md \"optional title\"",
		"[external]: https://example.com/docs",
	}, "\n")

	broken := brokenLocalMarkdownLinks(root, "docs/guide.md", body)
	if len(broken) != 1 || broken[0] != "missing.md" {
		t.Fatalf("broken links = %v, want [missing.md]", broken)
	}
}

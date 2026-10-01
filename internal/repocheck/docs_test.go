package repocheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
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

func TestCommunityTemplatesAdvertiseAvailableChannels(t *testing.T) {
	const securityURL = "https://github.com/0xPiranhaCodes/webpty/blob/main/SECURITY.md"
	const publicWarning = "Do not disclose credentials, capability links, terminal contents, or vulnerabilities. Vulnerabilities must not be reported publicly; follow [SECURITY.md](" + securityURL + ")."
	security := readRepoFile(t, "SECURITY.md")
	support := readRepoFile(t, "SUPPORT.md")
	config := readRepoFile(t, ".github/ISSUE_TEMPLATE/config.yml")

	for name, body := range map[string]string{
		"SECURITY.md": security,
		"SUPPORT.md":  support,
		"config.yml":  config,
	} {
		if strings.Contains(body, "github.com/0xPiranhaCodes/webpty/discussions") {
			t.Errorf("%s advertises disabled GitHub Discussions", name)
		}
		if strings.Contains(body, "/security/advisories/new") {
			t.Errorf("%s advertises disabled private vulnerability reporting", name)
		}
	}
	if !strings.Contains(security, "mailto:satheesh.101097@gmail.com") {
		t.Error("SECURITY.md does not make the reporting email an actionable link")
	}
	if !strings.Contains(config, securityURL) {
		t.Error("issue chooser does not link security reporters to SECURITY.md")
	}

	supportForm := readRepoFile(t, ".github/ISSUE_TEMPLATE/support_request.yml")
	if !strings.Contains(support, "issues/new?template=support_request.yml") {
		t.Error("SUPPORT.md does not link the support request form")
	}
	for _, id := range []string{"version", "installation", "host", "question", "attempted", "sensitive-data"} {
		if !strings.Contains(supportForm, "id: "+id) {
			t.Errorf("support request form lacks %q field", id)
		}
	}
	for _, name := range []string{"bug_report.yml", "feature_request.yml", "support_request.yml"} {
		body := readRepoFile(t, filepath.Join(".github", "ISSUE_TEMPLATE", name))
		if !strings.Contains(body, publicWarning) {
			t.Errorf("%s lacks the sensitive-data and vulnerability warning", name)
		}
	}
	if !strings.Contains(supportForm, "best-effort basis") {
		t.Error("support request form does not set best-effort expectations")
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
		if strings.HasPrefix(target, "mailto:") || strings.Contains(target, "://") {
			continue
		}
		target, fragment, hasFragment := strings.Cut(target, "#")
		target, _, _ = strings.Cut(target, "?")
		resolved := filepath.Join(root, filepath.FromSlash(name))
		targetBody := body
		if target != "" {
			resolved = filepath.Clean(filepath.Join(root, filepath.Dir(name), filepath.FromSlash(target)))
			relative, err := filepath.Rel(root, resolved)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				broken = append(broken, original)
				continue
			}
			if _, err := os.Stat(resolved); err != nil {
				broken = append(broken, original)
				continue
			}
			if hasFragment && strings.HasSuffix(strings.ToLower(resolved), ".md") {
				content, err := os.ReadFile(resolved)
				if err != nil {
					broken = append(broken, original)
					continue
				}
				targetBody = string(content)
			}
		}
		if hasFragment && strings.HasSuffix(strings.ToLower(resolved), ".md") &&
			!markdownHeadingSlugs(targetBody)[fragment] {
			broken = append(broken, original)
		}
	}
	return broken
}

func markdownHeadingSlugs(body string) map[string]bool {
	slugs := make(map[string]bool)
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		heading := strings.TrimLeft(line, " \t")
		markerEnd := 0
		for markerEnd < len(heading) && heading[markerEnd] == '#' {
			markerEnd++
		}
		if markerEnd == 0 || markerEnd > 6 || markerEnd == len(heading) ||
			(heading[markerEnd] != ' ' && heading[markerEnd] != '\t') {
			continue
		}
		text := strings.TrimSpace(heading[markerEnd:])
		text = strings.TrimSpace(strings.TrimRight(text, "#"))
		base := githubHeadingSlug(text)
		slug := base
		for suffix := 1; slugs[slug]; suffix++ {
			slug = base + "-" + strconv.Itoa(suffix)
		}
		slugs[slug] = true
	}
	return slugs
}

func githubHeadingSlug(heading string) string {
	var slug strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r), unicode.IsNumber(r), r == '-', r == '_':
			slug.WriteRune(r)
		case unicode.IsSpace(r):
			slug.WriteByte('-')
		}
	}
	return slug.String()
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

func TestBrokenLocalMarkdownLinksValidatesSameFileFragments(t *testing.T) {
	root := t.TempDir()
	body := strings.Join([]string{
		"# Install & Configure",
		"[valid](#install--configure)",
		"[broken](#missing-section)",
	}, "\n")

	broken := brokenLocalMarkdownLinks(root, "README.md", body)
	if len(broken) != 1 || broken[0] != "#missing-section" {
		t.Fatalf("broken links = %v, want [#missing-section]", broken)
	}
}

func TestBrokenLocalMarkdownLinksValidatesCrossFileFragments(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := "# Deployment\n"
	if err := os.WriteFile(filepath.Join(root, "docs", "guide.md"), []byte(target), 0o644); err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		"[valid](docs/guide.md#deployment)",
		"[broken](docs/guide.md#configuration)",
	}, "\n")

	broken := brokenLocalMarkdownLinks(root, "README.md", body)
	if len(broken) != 1 || broken[0] != "docs/guide.md#configuration" {
		t.Fatalf("broken links = %v, want [docs/guide.md#configuration]", broken)
	}
}

func TestBrokenLocalMarkdownLinksNumbersDuplicateHeadingSlugs(t *testing.T) {
	root := t.TempDir()
	body := strings.Join([]string{
		"# Setup",
		"## Setup",
		"[first](#setup)",
		"[second](#setup-1)",
		"[missing third](#setup-2)",
	}, "\n")

	broken := brokenLocalMarkdownLinks(root, "README.md", body)
	if len(broken) != 1 || broken[0] != "#setup-2" {
		t.Fatalf("broken links = %v, want [#setup-2]", broken)
	}
}

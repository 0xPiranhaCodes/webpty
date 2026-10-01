package repocheck

import (
	"regexp"
	"strings"
	"testing"
)

// go.mod's toolchain line is the one Go version of the project: setup-go
// reads it in every workflow, the Dockerfile's builder pins the same
// release, verify-archives.sh requires release binaries to report it, and
// the docs and the design spec name it.
func TestGoToolchainIsOneVersionEverywhere(t *testing.T) {
	m := regexp.MustCompile(`(?m)^toolchain go(\d+\.\d+\.\d+)$`).FindStringSubmatch(readRepoFile(t, "go.mod"))
	if m == nil {
		t.Fatal("go.mod has no toolchain goX.Y.Z line")
	}
	want := m[1]
	minor := want[:strings.LastIndex(want, ".")]

	goLine := regexp.MustCompile(`(?m)^go (\d+\.\d+)(\.\d+)?$`)
	for _, mod := range []string{"go.mod", "web/go.mod"} {
		if g := goLine.FindStringSubmatch(readRepoFile(t, mod)); g == nil || g[1] != minor {
			t.Errorf("%s's go directive is not Go %s.x", mod, minor)
		}
	}
	if !strings.Contains(readRepoFile(t, "Dockerfile"), "FROM golang:"+want+"-") {
		t.Errorf("the Dockerfile's builder is not golang:%s", want)
	}
	for name, body := range workflows(t) {
		if strings.Contains(body, "go-version:") {
			t.Errorf("%s pins Go by go-version instead of go.mod", name)
		}
		if setups := strings.Count(body, "actions/setup-go@"); setups != strings.Count(body, "go-version-file: go.mod") {
			t.Errorf("%s has %d setup-go steps that do not all read go.mod", name, setups)
		}
	}
	override := regexp.MustCompile(`GOTOOLCHAIN\s*[:?+]?=`)
	for _, name := range []string{"Makefile", ".goreleaser.yaml"} {
		if override.MatchString(readRepoFile(t, name)) {
			t.Errorf("%s overrides go.mod's toolchain with GOTOOLCHAIN", name)
		}
	}
	if !strings.Contains(readRepoFile(t, "scripts/verify-archives.sh"), `"toolchain"`) {
		t.Error("verify-archives.sh does not check release binaries against go.mod's toolchain")
	}

	prose := regexp.MustCompile(`\bGo (\d+\.\d+(?:\.\d+)?)\b|\bgo(\d+\.\d+\.\d+)\b`)
	for _, name := range trackedFiles(t, repoRoot(t)) {
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		for i, line := range strings.Split(readRepoFile(t, name), "\n") {
			for _, v := range prose.FindAllStringSubmatch(line, -1) {
				if got := v[1] + v[2]; got != want {
					t.Errorf("%s:%d names Go %s, not go.mod's toolchain %s: %s", name, i+1, got, want, strings.TrimSpace(line))
				}
			}
		}
	}
	for _, name := range []string{"README.md", "docs/superpowers/specs/2026-10-01-go-enterprise-rewrite-design.md"} {
		body := readRepoFile(t, name)
		if !strings.Contains(body, "Go "+want) || !strings.Contains(body, "toolchain") {
			t.Errorf("%s does not state the required Go %s toolchain", name, want)
		}
	}
}

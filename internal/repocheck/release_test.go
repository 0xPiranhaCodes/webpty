package repocheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func workflows(t *testing.T) map[string]string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(name)] = string(data)
	}
	return out
}

var (
	usesLine   = regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*(\S+)(.*)$`)
	pinnedUses = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	versionTag = regexp.MustCompile(`#\s*v\d`)
)

// A tag can be moved to different code; a full commit SHA cannot.
func TestWorkflowActionsArePinnedToCommits(t *testing.T) {
	for name, body := range workflows(t) {
		for _, m := range usesLine.FindAllStringSubmatch(body, -1) {
			if strings.HasPrefix(m[1], "./") {
				continue
			}
			if !pinnedUses.MatchString(m[1]) {
				t.Errorf("%s: %s is not pinned to a full commit SHA", name, m[1])
			}
			if !versionTag.MatchString(m[2]) {
				t.Errorf("%s: %s has no # vX.Y.Z comment naming the release", name, m[1])
			}
		}
	}
}

func TestWorkflowsDefaultToReadOnlyPermissions(t *testing.T) {
	for name, body := range workflows(t) {
		if !regexp.MustCompile(`(?m)^permissions:\n\s+contents: read\s*$`).MatchString(body) &&
			!regexp.MustCompile(`(?m)^permissions: \{\}\s*$`).MatchString(body) {
			t.Errorf("%s: top-level permissions must be read-only (contents: read) or empty", name)
		}
		if strings.Contains(body, "write-all") {
			t.Errorf("%s: uses write-all permissions", name)
		}
	}
}

func TestReleaseWorkflowIsTagTriggeredAndKeyless(t *testing.T) {
	body := readRepoFile(t, ".github/workflows/release.yml")
	for _, want := range []string{
		"tags:",
		"id-token: write",     // keyless signing and attestations through GitHub OIDC
		"attestations: write", // build provenance
		"contents: write",     // uploading release assets, on the release job only
		"goreleaser",          // archives, checksums, SBOMs, signatures
		"attest-build-provenance",
		"cosign",
		"syft",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("release.yml lacks %q", want)
		}
	}
	for _, forbidden := range []string{"secrets.PYPI", "pull_request_target"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("release.yml contains %q", forbidden)
		}
	}
}

// releaseJobs splits release.yml into its jobs, keyed by job id.
func releaseJobs(t *testing.T) map[string]string {
	t.Helper()
	body := readRepoFile(t, ".github/workflows/release.yml")
	_, after, ok := strings.Cut(body, "\njobs:\n")
	if !ok {
		t.Fatal("release.yml has no jobs")
	}
	jobs := map[string]string{}
	header := regexp.MustCompile(`(?m)^  ([\w-]+):\s*$`)
	idx := header.FindAllStringSubmatchIndex(after, -1)
	for i, m := range idx {
		end := len(after)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		jobs[after[m[2]:m[3]]] = after[m[0]:end]
	}
	return jobs
}

func needs(job string) []string {
	m := regexp.MustCompile(`(?m)^    needs:\s*\[([^\]]*)\]`).FindStringSubmatch(job)
	if m == nil {
		if m = regexp.MustCompile(`(?m)^    needs:\s*([\w-]+)\s*$`).FindStringSubmatch(job); m == nil {
			return nil
		}
	}
	var out []string
	for _, n := range strings.Split(m[1], ",") {
		out = append(out, strings.TrimSpace(n))
	}
	return out
}

// dependsOn reports whether job transitively needs dep.
func dependsOn(jobs map[string]string, job, dep string) bool {
	for _, n := range needs(jobs[job]) {
		if n == dep || dependsOn(jobs, n, dep) {
			return true
		}
	}
	return false
}

var writePermission = regexp.MustCompile(`(?m)^\s+[\w-]+: write\b`)

// npm lifecycle scripts run arbitrary dependency code, so they must never
// run where a token can publish, sign, or write the repository.
var runsNpm = regexp.MustCompile(`npm|npx|setup-node|make web-|make snapshot|make check|make e2e|make vuln-npm|goreleaser`)

func TestReleaseWorkflowKeepsNpmAwayFromCredentials(t *testing.T) {
	jobs := releaseJobs(t)
	built := false
	for name, job := range jobs {
		privileged := writePermission.MatchString(job)
		if privileged && runsNpm.MatchString(job) {
			t.Errorf("release job %s holds write or OIDC permissions and runs npm or a build that does: %s",
				name, runsNpm.FindString(job))
		}
		if !privileged && strings.Contains(job, "make web-assets") && strings.Contains(job, "upload-artifact") {
			built = true
		}
	}
	if !built {
		t.Error("no read-only release job builds the web assets and hands them on as an artifact")
	}
	if archives := jobs["archives"]; writePermission.MatchString(archives) || !strings.Contains(archives, "download-artifact") {
		t.Error("the archives job must be read-only and embed the downloaded web assets")
	}
	if image := jobs["image-build"]; writePermission.MatchString(image) || !strings.Contains(image, "image-binaries.sh") {
		t.Error("the image must be built without write permissions from the verified archive binaries")
	}
}

func TestReleaseIsADraftUntilEveryCheckPasses(t *testing.T) {
	jobs := releaseJobs(t)
	for _, name := range []string{"verify", "scan", "web-assets", "archives", "homebrew", "image-build", "image-attest", "publish", "image", "finalize"} {
		if _, ok := jobs[name]; !ok {
			t.Fatalf("release.yml has no %s job", name)
		}
	}
	for job, deps := range map[string][]string{
		"archives":     {"verify", "scan", "web-assets"},
		"homebrew":     {"archives"},
		"image-build":  {"archives"},
		"image-attest": {"image-build"},
		"publish":      {"verify", "scan", "archives", "homebrew", "image-build", "image-attest"},
		"image":        {"publish", "image-build", "image-attest"},
		"finalize":     {"publish", "image"},
	} {
		for _, dep := range deps {
			if !dependsOn(jobs, job, dep) {
				t.Errorf("release job %s does not wait for %s", job, dep)
			}
		}
	}
	scan := jobs["scan"]
	for _, want := range []string{"make vuln-go", "make vuln-npm", "make secrets", "fetch-depth: 0"} {
		if !strings.Contains(scan, want) {
			t.Errorf("scan job lacks %q", want)
		}
	}
	if writePermission.MatchString(scan) || writePermission.MatchString(jobs["verify"]) {
		t.Error("verification and scan jobs must be read-only")
	}
	if !strings.Contains(jobs["homebrew"], "macos") || !strings.Contains(jobs["homebrew"], "homebrew-validate") {
		t.Error("the homebrew job must install and test the formula on macOS")
	}
	if !strings.Contains(jobs["image-build"], "docker-smoke.sh") || writePermission.MatchString(jobs["image-build"]) {
		t.Error("the image-build job must smoke-test the image without write permissions")
	}
	publish := jobs["publish"]
	for _, want := range []string{"gh release create", "--draft", "--verify-tag", "cosign sign-blob", "cosign verify-blob", "attest-build-provenance", "webpty.rb"} {
		if !strings.Contains(publish, want) {
			t.Errorf("publish job lacks %q", want)
		}
	}
	if !strings.Contains(jobs["finalize"], "--draft=false") {
		t.Error("only the finalize job may take the release out of draft")
	}
	for name, job := range jobs {
		if name != "finalize" && strings.Contains(job, "--draft=false") {
			t.Errorf("job %s publishes the release before every check passed", name)
		}
	}
}

// registryWrite matches anything that can create or move a tag on a registry.
var registryWrite = regexp.MustCompile(`push: true|--push\b|docker push|docker://|login-action|skopeo login|packages: write|push-to-registry: true`)

// A tag on ghcr.io is what users pull, so none may exist before the image's
// provenance is attested and verified. The image is built once into a local
// OCI archive, smoke-tested and attested by digest without registry
// credentials, and only then copied to its release tags unchanged. Nothing
// is staged on the registry, so a failed gate leaves nothing to clean up.
func TestImageIsAttestedBeforeAnyReleaseTagExists(t *testing.T) {
	jobs := releaseJobs(t)
	for name, job := range jobs {
		if name != "image" && registryWrite.MatchString(job) {
			t.Errorf("job %s can write to the registry (%s); only the final image job may", name, registryWrite.FindString(job))
		}
	}

	build := jobs["image-build"]
	for _, want := range []string{"target release", "--build-context binaries=dist/image", "type=oci", "docker-smoke.sh", "upload-artifact", "digest="} {
		if !strings.Contains(build, want) {
			t.Errorf("image-build lacks %q", want)
		}
	}
	if smoke, oci := strings.Index(build, "docker-smoke.sh"), strings.Index(build, "type=oci"); smoke < oci {
		t.Error("image-build must smoke-test the image it exported, not a separate build")
	}

	attest := jobs["image-attest"]
	if regexp.MustCompile(`(?m)^\s+(contents|packages): write`).MatchString(attest) {
		t.Error("image-attest may hold only id-token and attestations write")
	}
	for _, want := range []string{"attest-build-provenance", "subject-digest: ${{ needs.image-build.outputs.digest }}", "push-to-registry: false", "gh attestation verify"} {
		if !strings.Contains(attest, want) {
			t.Errorf("image-attest lacks %q", want)
		}
	}

	image := jobs["image"]
	if regexp.MustCompile(`(?m)^\s+(id-token|attestations|contents): write`).MatchString(image) {
		t.Error("the image job may only hold packages: write")
	}
	if regexp.MustCompile(`buildx build|build-push-action|docker build`).MatchString(image) {
		t.Error("the image job must publish the attested archive, not rebuild it")
	}
	verify := strings.Index(image, "gh attestation verify")
	firstPush := strings.Index(image, "docker://")
	if verify < 0 || firstPush < 0 || verify > firstPush {
		t.Error("the image job must verify the digest's attestation before it creates any tag")
	}
	for _, want := range []string{"sha256sum --check --strict", "--signer-workflow", "--source-ref", "--preserve-digests", `= "$DIGEST"`} {
		if !strings.Contains(image, want) {
			t.Errorf("the image job lacks %q", want)
		}
	}
	if !regexp.MustCompile(`case \$version in \*-\*\)`).MatchString(image) || !strings.Contains(image, "latest") {
		t.Error("the image job must tag latest and major.minor for stable releases only")
	}
	if !dependsOn(jobs, "finalize", "image") {
		t.Error("the release must stay a draft until the image is published")
	}
}

func TestReleaseFormulaIsCoveredByTheSignedChecksums(t *testing.T) {
	jobs := releaseJobs(t)
	if !strings.Contains(jobs["archives"], "release-assemble.sh") {
		t.Error("the archives job must assemble webpty.rb into the checksums before anything is signed")
	}
	publish := jobs["publish"]
	i := strings.Index(publish, "attest-build-provenance")
	if i < 0 {
		t.Fatal("the publish job attests nothing")
	}
	attest := publish[i:]
	if !strings.Contains(attest, "webpty.rb") || !strings.Contains(attest, "_checksums.txt") || !strings.Contains(attest, ".tar.gz") {
		t.Error("provenance must cover the archives, the checksums, and webpty.rb")
	}
	assemble := readRepoFile(t, "scripts/release-assemble.sh")
	if !strings.Contains(assemble, "webpty.rb") || !strings.Contains(assemble, "homebrew-formula.sh") {
		t.Error("release-assemble.sh must render webpty.rb and list it in the checksums")
	}
}

// A regexp identity would accept a signature from any workflow or ref of
// the repository; the documented identity names release.yml on a tag.
func TestCosignIdentityIsTheTaggedReleaseWorkflow(t *testing.T) {
	identity := regexp.MustCompile(`https://github\.com/\S+/\.github/workflows/release\.yml@refs/tags/(v|\$\{?(TAG|tag)\}?)`)
	for _, name := range []string{".github/workflows/release.yml", "docs/upgrading.md", "scripts/homebrew-tap.sh", "README.md"} {
		body := readRepoFile(t, name)
		if strings.Contains(body, "certificate-identity-regexp") {
			t.Errorf("%s accepts a cosign identity by regular expression", name)
		}
		if !strings.Contains(body, "--certificate-identity") || !identity.MatchString(body) {
			t.Errorf("%s does not pin the cosign identity to release.yml@refs/tags/v...", name)
		}
	}
	if !regexp.MustCompile(`--certificate-identity[ =]"?https://github\.com/[^/\s]+/[^/\s]+/\.github/workflows/release\.yml@refs/tags/v\d`).MatchString(readRepoFile(t, "docs/upgrading.md")) {
		t.Error("docs/upgrading.md does not show a literal identity such as .../release.yml@refs/tags/v1.3.0")
	}
	for _, name := range []string{"docs/upgrading.md"} {
		body := readRepoFile(t, name)
		if !strings.Contains(body, "--signer-workflow") || !strings.Contains(body, "--source-ref refs/tags/v") {
			t.Errorf("%s does not pin gh attestation verify to the tagged release workflow", name)
		}
	}
}

var rawScriptURL = regexp.MustCompile(`raw\.githubusercontent\.com/(?:\$\{GITHUB_REPOSITORY\}|[^/\s$]+/[^/\s]+)/([^/\s]+)/scripts/([\w.-]+\.sh)`)

// The Homebrew helper is the trust anchor for the formula's cosign check, so
// it is fetched from the release tag it installs, never from a moving
// branch. A pinned install.sh command is fetched from its tag as well.
func TestBootstrapScriptsComeFromTheReleaseTag(t *testing.T) {
	tagRef := regexp.MustCompile(`^(v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?|\$\{TAG\})$`)
	names := []string{"README.md", ".github/workflows/release.yml", "scripts/install.sh", "scripts/homebrew-tap.sh"}
	docs, err := filepath.Glob(filepath.Join(repoRoot(t), "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		names = append(names, filepath.Join("docs", filepath.Base(doc)))
	}
	tapURLs := map[string]int{}
	for _, name := range names {
		for i, line := range strings.Split(readRepoFile(t, name), "\n") {
			for _, m := range rawScriptURL.FindAllStringSubmatch(line, -1) {
				ref, script := m[1], m[2]
				pinned := script == "homebrew-tap.sh" || strings.Contains(line, "--version")
				if pinned && !tagRef.MatchString(ref) {
					t.Errorf("%s:%d fetches %s from %q, not a release tag: %s", name, i+1, script, ref, strings.TrimSpace(line))
				}
				if script == "homebrew-tap.sh" {
					tapURLs[name]++
				}
			}
		}
	}
	for _, name := range []string{"README.md", "docs/upgrading.md", ".github/workflows/release.yml"} {
		if tapURLs[name] == 0 {
			t.Errorf("%s does not show where to download homebrew-tap.sh", name)
		}
	}
	notes := releaseJobs(t)["publish"]
	if !strings.Contains(notes, "raw.githubusercontent.com/${GITHUB_REPOSITORY}/${TAG}/scripts/homebrew-tap.sh") ||
		!strings.Contains(notes, "sh homebrew-tap.sh --version ${version}") {
		t.Error("the release notes do not give the tagged homebrew-tap.sh download and install command")
	}
}

// Linux runners have sha256sum, macOS has shasum; scripts run on both.
func TestScriptsHashWithEitherTool(t *testing.T) {
	names, err := filepath.Glob(filepath.Join(repoRoot(t), "scripts", "*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range names {
		name := filepath.Join("scripts", filepath.Base(path))
		body := readRepoFile(t, name)
		usesSha := strings.Contains(body, "sha256sum")
		usesShasum := strings.Contains(body, "shasum")
		if usesSha != usesShasum || (usesSha && !strings.Contains(body, "command -v sha256sum")) {
			t.Errorf("%s computes SHA-256 without choosing between sha256sum and shasum", name)
		}
	}
}

// The Makefile and workflows run scripts directly.
func TestScriptsAreExecutable(t *testing.T) {
	names, err := filepath.Glob(filepath.Join(repoRoot(t), "scripts", "*.sh"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no scripts: %v", err)
	}
	for _, name := range names {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 != 0o111 {
			t.Errorf("%s has mode %o, want executable", filepath.Base(name), info.Mode().Perm())
		}
	}
}

func TestNodeIsPinnedToTheDockerVersion(t *testing.T) {
	dockerNode := regexp.MustCompile(`(?m)^FROM node:(\d+\.\d+\.\d+)-`).FindStringSubmatch(readRepoFile(t, "Dockerfile"))
	if dockerNode == nil {
		t.Fatal("Dockerfile has no exact node:X.Y.Z base")
	}
	if m := regexp.MustCompile(`(?m)^NODE_VERSION \?= (\S+)$`).FindStringSubmatch(readRepoFile(t, "Makefile")); m == nil || m[1] != dockerNode[1] {
		t.Errorf("Makefile NODE_VERSION = %v, want %s so local artifacts match release builds", m, dockerNode[1])
	}
	for name, body := range workflows(t) {
		for _, m := range regexp.MustCompile(`node-version:\s*"?([^"\s]+)"?`).FindAllStringSubmatch(body, -1) {
			if m[1] != dockerNode[1] {
				t.Errorf("%s uses node-version %s, want %s to match the Dockerfile", name, m[1], dockerNode[1])
			}
		}
	}
}

// The one version grammar: X.Y.Z with an optional prerelease suffix.
const versionGrammar = `[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`

func TestEveryReleaseEntryPointSharesOneVersionGrammar(t *testing.T) {
	for _, name := range []string{"scripts/install.sh", "scripts/homebrew-formula.sh", "scripts/homebrew-tap.sh", ".github/workflows/release.yml"} {
		body := readRepoFile(t, name)
		if !strings.Contains(body, versionGrammar) {
			t.Errorf("%s does not validate versions with %s", name, versionGrammar)
		}
	}
}

func TestGoReleaserBuildsEveryArchiveReproducibly(t *testing.T) {
	body := readRepoFile(t, ".goreleaser.yaml")
	for _, want := range []string{
		"version: 2",
		"- darwin", "- linux", "- amd64", "- arm64",
		"CGO_ENABLED=0",
		"-trimpath",
		"github.com/0xPiranhaCodes/webpty/internal/buildinfo.version={{.Version}}",
		"github.com/0xPiranhaCodes/webpty/internal/buildinfo.commit={{.FullCommit}}",
		"github.com/0xPiranhaCodes/webpty/internal/buildinfo.date={{.CommitDate}}",
		"mod_timestamp: \"{{ .CommitTimestamp }}\"",
		"owner: root", // archive entries must not carry the build host's user
		"group: root",
		"internal/webassets/dist/index.html", // refuses to build without the embedded UI
		"algorithm: sha256",
		"sboms:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf(".goreleaser.yaml lacks %q", want)
		}
	}
	if strings.Contains(body, "windows") {
		t.Error(".goreleaser.yaml builds for Windows, which is unsupported")
	}
	if regexp.MustCompile(`npm|make web-assets`).MatchString(body) {
		t.Error(".goreleaser.yaml runs npm; the UI must be built in a separate unprivileged step")
	}
}

func TestDockerfileIsHardened(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")
	stages := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^FROM\s+(\S+)(?:\s+AS\s+(\S+))?`).FindAllStringSubmatch(body, -1) {
		if !strings.Contains(m[1], "@sha256:") && !stages[m[1]] {
			t.Errorf("Dockerfile base image %s is not pinned by digest", m[1])
		}
		stages[m[2]] = true
	}
	start := strings.Index(body, " AS release\n")
	if start < 0 {
		t.Fatal("Dockerfile has no release stage")
	}
	release := body[start:]
	if i := strings.Index(release, "\nFROM "); i >= 0 {
		release = release[:i]
	}
	if !strings.Contains(release, "--from=binaries") || regexp.MustCompile(`--from=(web|build)\b|npm|go build`).MatchString(release) {
		t.Error("the release stage must package prebuilt archive binaries, never build from source")
	}
	if !strings.Contains(body, `${WEBPTY_ADDRESS##*:}`) {
		t.Error("the HEALTHCHECK must probe the port WEBPTY_ADDRESS listens on")
	}
	for _, want := range []string{"HEALTHCHECK", "VOLUME [\"/data\"]", "EXPOSE 8000", "WEBPTY_DATABASE_PATH=/data/webpty.db"} {
		if !strings.Contains(body, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	users := regexp.MustCompile(`(?m)^USER\s+(\S+)`).FindAllStringSubmatch(body, -1)
	if len(users) == 0 || users[len(users)-1][1] == "root" || users[len(users)-1][1] == "0" ||
		strings.HasPrefix(users[len(users)-1][1], "0:") {
		t.Error("the final Dockerfile stage must run as a non-root USER")
	}
	if regexp.MustCompile(`(?im)^(ARG|ENV)\s+\S*(SECRET|TOKEN|PASSWORD|KEY)`).MatchString(body) {
		t.Error("Dockerfile passes a secret through ARG or ENV")
	}

	ignore := readRepoFile(t, ".dockerignore")
	for _, want := range []string{".git", "node_modules", "*.db", ".superpowers", "dist"} {
		if !strings.Contains(ignore, want) {
			t.Errorf(".dockerignore lacks %q", want)
		}
	}
}

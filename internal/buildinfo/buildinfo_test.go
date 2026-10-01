package buildinfo_test

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/buildinfo"
)

func TestDevelopmentBuildsReportDevVersion(t *testing.T) {
	info := buildinfo.Get()
	if info.Version != "dev" {
		t.Errorf("Version = %q, want dev for a build without linker flags", info.Version)
	}
	if info.Commit == "" || info.Date != "unknown" {
		t.Errorf("Commit/Date = %q/%q, want a commit and an unknown build date", info.Commit, info.Date)
	}
	if info.GoVersion != runtime.Version() || info.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("runtime = %q %q", info.GoVersion, info.Platform)
	}
}

func TestStringNamesVersionCommitDateAndPlatform(t *testing.T) {
	info := buildinfo.Info{Version: "1.2.3", Commit: "abc1234", Date: "2026-10-01T00:00:00Z", GoVersion: "go1.26.8", Platform: "darwin/arm64"}
	want := "webpty 1.2.3 (commit abc1234, built 2026-10-01T00:00:00Z, go1.26.8, darwin/arm64)"
	if got := info.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// Release builds inject their metadata with -ldflags -X.
func TestLinkerFlagsSetVersionCommitAndDate(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	pkg := "github.com/0xPiranhaCodes/webpty/internal/buildinfo"
	ldflags := "-X " + pkg + ".version=9.8.7 -X " + pkg + ".commit=deadbeef -X " + pkg + ".date=2026-10-01T10:00:00Z"
	out, err := exec.Command("go", "run", "-ldflags", ldflags, "./testdata/printinfo").CombinedOutput()
	if err != nil {
		t.Fatalf("go run: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "9.8.7 deadbeef 2026-10-01T10:00:00Z" {
		t.Errorf("metadata = %q", got)
	}
}

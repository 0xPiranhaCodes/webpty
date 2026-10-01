// Package buildinfo reports which webpty build is running.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Release builds set these with -ldflags "-X <package>.version=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Info describes a build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the running build's metadata. A build without linker flags is
// version "dev" with build date "unknown"; its commit comes from the Go
// toolchain's VCS stamp when there is one.
func Get() Info {
	info := Info{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if info.Commit == "" {
		revision, modified := vcsStamp()
		info.Commit = revision
		if revision != "" && modified {
			info.Commit += "-dirty"
		}
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.Date == "" {
		info.Date = "unknown"
	}
	return info
}

func vcsStamp() (revision string, modified bool) {
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
}

func (i Info) String() string {
	return fmt.Sprintf("webpty %s (commit %s, built %s, %s, %s)", i.Version, i.Commit, i.Date, i.GoVersion, i.Platform)
}

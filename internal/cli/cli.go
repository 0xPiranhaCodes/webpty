// Package cli implements the webpty command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"strings"

	"github.com/0xPiranhaCodes/webpty/internal/app"
	"github.com/0xPiranhaCodes/webpty/internal/buildinfo"
	"github.com/0xPiranhaCodes/webpty/internal/config"
	"github.com/0xPiranhaCodes/webpty/internal/doctor"
	"github.com/0xPiranhaCodes/webpty/internal/store"
	"github.com/0xPiranhaCodes/webpty/internal/webassets"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// CLI runs webpty commands against an environment and output streams.
type CLI struct {
	Lookup         func(string) (string, bool)
	Stdout, Stderr io.Writer
	// Assets is the web UI doctor checks; nil means the embedded build.
	Assets fs.FS
}

const usage = `webpty serves terminals to the browser.

Usage:
  webpty [serve] [flags] [-- command arguments]
  webpty version [--json]
  webpty doctor [serve flags]
  webpty backup --output PATH [--database PATH]
  webpty restore --input PATH [--database PATH]

Commands:
  serve     run the server (the default)
  version   print the version, commit, and build date
  doctor    check this host and configuration, without changing anything
  backup    write a consistent copy of the database, even while serving
  restore   replace the database with a backup; the server must be stopped

Serve flags:
`

// Run executes args (without the program name) and returns the exit code.
// The serve command runs until ctx is done.
func (c CLI) Run(ctx context.Context, args []string) int {
	command, rest := "serve", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, rest = args[0], args[1:]
	}
	switch {
	case command == "help" || (command == "serve" && len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help")):
		c.printUsage()
		return exitOK
	case command == "serve" && len(args) > 0 && (args[0] == "--version" || args[0] == "-version"):
		return c.version(nil)
	}
	switch command {
	case "serve":
		return c.serve(ctx, rest)
	case "version":
		return c.version(rest)
	case "doctor":
		return c.doctor(ctx, rest)
	case "backup":
		return c.backup(ctx, rest)
	case "restore":
		return c.restore(ctx, rest)
	default:
		fmt.Fprintf(c.Stderr, "webpty: unknown command %q; run webpty help\n", command)
		return exitUsage
	}
}

func (c CLI) printUsage() {
	fmt.Fprint(c.Stdout, usage)
	_, _, _ = config.ParseServe(c.Lookup, []string{"-h"}, &flagDefaults{w: c.Stdout})
}

// flagDefaults passes through a FlagSet's flag list without its usage line.
type flagDefaults struct {
	w       io.Writer
	started bool
}

func (f *flagDefaults) Write(p []byte) (int, error) {
	if !f.started {
		f.started = true
		if i := strings.Index(string(p), "\n"); strings.HasPrefix(string(p), "Usage:") && i >= 0 {
			if _, err := f.w.Write(p[i+1:]); err != nil {
				return 0, err
			}
			return len(p), nil
		}
	}
	return f.w.Write(p)
}

func (c CLI) serve(ctx context.Context, args []string) int {
	cfg, warnings, err := config.ParseServe(c.Lookup, args, c.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(c.Stderr, "webpty: %v\n", err)
		return exitUsage
	}
	for _, warning := range warnings {
		fmt.Fprintf(c.Stderr, "webpty: warning: %s\n", warning)
	}
	if err := app.Run(ctx, cfg); err != nil {
		fmt.Fprintf(c.Stderr, "webpty: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func (c CLI) version(args []string) int {
	flags := c.flags("version", "")
	asJSON := flags.Bool("json", false, "print JSON")
	if code, ok := c.parse(flags, args); !ok {
		return code
	}
	info := buildinfo.Get()
	if *asJSON {
		encoder := json.NewEncoder(c.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(info)
		return exitOK
	}
	fmt.Fprintln(c.Stdout, info)
	return exitOK
}

func (c CLI) doctor(ctx context.Context, args []string) int {
	fmt.Fprintf(c.Stdout, "%s\n\n", buildinfo.Get())
	cfg, warnings, err := config.ParseServe(c.Lookup, args, c.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(c.Stdout, "[%s] configuration: %v\n", doctor.Fail, err)
		return exitFailure
	}
	configuration := "environment and flags are valid"
	if len(warnings) > 0 {
		configuration += "; " + strings.Join(warnings, "; ")
	}
	fmt.Fprintf(c.Stdout, "[%s] configuration: %s\n", doctor.OK, configuration)
	assets := c.Assets
	if assets == nil {
		assets = webassets.Dist()
	}
	report := doctor.Run(ctx, doctor.Options{Config: cfg, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Assets: assets})
	fmt.Fprint(c.Stdout, report)
	if report.Failed() {
		return exitFailure
	}
	return exitOK
}

func (c CLI) backup(ctx context.Context, args []string) int {
	flags := c.flags("backup", "--output PATH [--database PATH]")
	output := flags.String("output", "", "backup file to create; it must not exist")
	database := flags.String("database", "", "database to back up (WEBPTY_DATABASE_PATH, default webpty.db)")
	if code, ok := c.parse(flags, args); !ok {
		return code
	}
	if *output == "" {
		fmt.Fprintln(c.Stderr, "webpty backup: --output is required")
		return exitUsage
	}
	info := buildinfo.Get()
	source := c.databasePath(*database)
	meta, err := store.Backup(ctx, source, *output, store.BackupOptions{Version: info.Version, Commit: info.Commit})
	if err != nil {
		fmt.Fprintf(c.Stderr, "webpty backup: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(c.Stdout, "Backed up %s to %s\n  schema version %d, created %s by webpty %s (commit %s)\n",
		source, *output, meta.SchemaVersion, meta.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), meta.WebptyVersion, meta.WebptyCommit)
	return exitOK
}

func (c CLI) restore(ctx context.Context, args []string) int {
	flags := c.flags("restore", "--input PATH [--database PATH]")
	input := flags.String("input", "", "backup file to restore")
	database := flags.String("database", "", "database to replace (WEBPTY_DATABASE_PATH, default webpty.db)")
	if code, ok := c.parse(flags, args); !ok {
		return code
	}
	if *input == "" {
		fmt.Fprintln(c.Stderr, "webpty restore: --input is required")
		return exitUsage
	}
	target := c.databasePath(*database)
	result, err := store.Restore(ctx, *input, target)
	if err != nil {
		fmt.Fprintf(c.Stderr, "webpty restore: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(c.Stdout, "Restored %s from %s (schema version %d)\n", target, *input, result.SchemaVersion)
	if result.Metadata != nil {
		fmt.Fprintf(c.Stdout, "  backup created %s by webpty %s (commit %s)\n",
			result.Metadata.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), result.Metadata.WebptyVersion, result.Metadata.WebptyCommit)
	}
	if result.RollbackPath != "" {
		fmt.Fprintf(c.Stdout, "  the replaced database is kept at %s\n", result.RollbackPath)
	}
	if latest := store.LatestSchemaVersion(); result.SchemaVersion < latest {
		fmt.Fprintf(c.Stdout, "  %d migrations will be applied when webpty next starts\n", latest-result.SchemaVersion)
	}
	return exitOK
}

func (c CLI) databasePath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if value, ok := c.Lookup("WEBPTY_DATABASE_PATH"); ok && value != "" {
		return value
	}
	return "webpty.db"
}

func (c CLI) flags(name, synopsis string) *flag.FlagSet {
	flags := flag.NewFlagSet("webpty "+name, flag.ContinueOnError)
	flags.SetOutput(c.Stderr)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: webpty %s %s\n", name, synopsis)
		flags.PrintDefaults()
	}
	return flags
}

// parse reports ok=false with the exit code when the command must stop.
func (c CLI) parse(flags *flag.FlagSet, args []string) (code int, ok bool) {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitUsage, false
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(c.Stderr, "%s: unexpected argument %q\n", flags.Name(), flags.Arg(0))
		return exitUsage, false
	}
	return 0, true
}

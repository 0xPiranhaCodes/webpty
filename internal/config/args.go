package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// ParseServe builds the server configuration from the environment and the
// serve command line. Flags take precedence over their environment
// variables and are applied before validation, so the combined result is
// checked as a whole.
//
// --cmd (also -cmd and -c) and -p/--port keep the legacy CLI's meaning.
// --cmd names the terminal executable exactly as given; arguments after
// "--" are passed to it verbatim and never through a shell. The returned
// warnings describe accepted legacy flags that no longer have an effect.
func ParseServe(lookup func(string) (string, bool), args []string, output io.Writer) (Config, []string, error) {
	f := newServeFlags(output)
	if err := f.set.Parse(args); err != nil {
		return Config{}, nil, err
	}
	f.recordVisited()
	if err := f.rejectRemoved(); err != nil {
		return Config{}, nil, err
	}
	overrides, err := f.overrides(lookup)
	if err != nil {
		return Config{}, nil, err
	}
	cfg, err := LoadFrom(overlay(overrides, lookup))
	if err != nil {
		return Config{}, nil, err
	}
	if f.commandSet() && f.set.NArg() > 0 {
		cfg.CommandArgs = append([]string(nil), f.set.Args()...)
	}
	return cfg, f.legacyWarnings(), nil
}

const (
	minPort = 1
	maxPort = 65535
)

// serveFlags holds the parsed serve command line. Each legacy short alias
// shares its long flag's variable.
type serveFlags struct {
	set                                                                         *flag.FlagSet
	visited                                                                     map[string]bool
	address, port, database, origin, command, allowedHosts, password, keepalive string
	secure                                                                      bool
}

func newServeFlags(output io.Writer) *serveFlags {
	f := &serveFlags{set: flag.NewFlagSet("webpty", flag.ContinueOnError), visited: map[string]bool{}}
	f.set.SetOutput(output)
	f.set.Usage = func() {
		fmt.Fprintln(output, "Usage: webpty [serve] [flags] [-- command arguments]")
		fmt.Fprintln(output, "\nFlags override the WEBPTY_* environment variable named in each description.")
		f.set.PrintDefaults()
	}
	f.defineCurrent()
	f.defineLegacy()
	return f
}

func (f *serveFlags) defineCurrent() {
	f.set.StringVar(&f.address, "address", "", "listen `host:port` (WEBPTY_ADDRESS, default 127.0.0.1:8000)")
	f.stringAliases(&f.port, "listen `port`, keeping the configured host (legacy; conflicts with --address)", "port", "p")
	f.set.StringVar(&f.database, "database", "", "SQLite database `path` (WEBPTY_DATABASE_PATH, default webpty.db)")
	f.set.StringVar(&f.origin, "public-origin", "", "`URL` browsers use, required off loopback (WEBPTY_PUBLIC_ORIGIN)")
	f.set.BoolVar(&f.secure, "secure-cookies", false, "mark cookies Secure; defaults on for an https public origin (WEBPTY_SECURE_COOKIES)")
	f.stringAliases(&f.command, "terminal `executable`, run without a shell; its arguments follow -- (WEBPTY_COMMAND, default $SHELL)", "cmd", "c")
}

// defineLegacy keeps flags from the old CLI parseable so scripts using them
// get a clear message instead of "flag provided but not defined".
func (f *serveFlags) defineLegacy() {
	f.stringAliases(&f.allowedHosts, "deprecated and ignored: use --public-origin", "allowed-hosts", "ah")
	f.stringAliases(&f.password, "no longer supported: the administrator sets a password at first sign-in", "password", "pass")
	f.stringAliases(&f.keepalive, "deprecated and ignored: use WEBPTY_WS_PING_INTERVAL", "keepalive", "k")
}

func (f *serveFlags) stringAliases(target *string, usage string, names ...string) {
	for _, name := range names {
		f.set.StringVar(target, name, "", usage)
	}
}

func (f *serveFlags) recordVisited() {
	f.set.Visit(func(fl *flag.Flag) { f.visited[fl.Name] = true })
}

// given reports whether any of names was on the command line.
func (f *serveFlags) given(names ...string) bool {
	for _, name := range names {
		if f.visited[name] {
			return true
		}
	}
	return false
}

func (f *serveFlags) commandSet() bool { return f.given("cmd", "c") }

func (f *serveFlags) rejectRemoved() error {
	if f.given("password", "pass") {
		return errors.New("--password is no longer supported: webpty starts with the administrator password CHANGEME " +
			"and requires changing it at first sign-in")
	}
	return nil
}

func (f *serveFlags) legacyWarnings() []string {
	var warnings []string
	if f.given("allowed-hosts", "ah") {
		warnings = append(warnings, "--allowed-hosts is deprecated and has no effect: webpty accepts only loopback hosts, "+
			"or the host of --public-origin when one is set")
	}
	if f.given("keepalive", "k") {
		warnings = append(warnings, "--keepalive is deprecated and has no effect: idle connections are probed every WEBPTY_WS_PING_INTERVAL")
	}
	return warnings
}

// overrides maps each given flag onto the environment variable it replaces.
func (f *serveFlags) overrides(lookup func(string) (string, bool)) (map[string]string, error) {
	overrides := map[string]string{}
	if err := f.addressOverride(lookup, overrides); err != nil {
		return nil, err
	}
	if f.given("database") {
		if f.database == "" {
			return nil, errors.New("--database must not be empty")
		}
		overrides["WEBPTY_DATABASE_PATH"] = f.database
	}
	if f.given("public-origin") {
		overrides["WEBPTY_PUBLIC_ORIGIN"] = f.origin
	}
	if f.given("secure-cookies") {
		overrides["WEBPTY_SECURE_COOKIES"] = strconv.FormatBool(f.secure)
	}
	return overrides, f.commandOverride(overrides)
}

func (f *serveFlags) addressOverride(lookup func(string) (string, bool), overrides map[string]string) error {
	portGiven := f.given("port", "p")
	if f.given("address") {
		if portGiven {
			return errors.New("use either --address or --port, not both")
		}
		if f.address == "" {
			return errors.New("--address must not be empty")
		}
		overrides["WEBPTY_ADDRESS"] = f.address
	}
	if !portGiven {
		return nil
	}
	address, err := f.addressWithPort(lookup)
	if err != nil {
		return err
	}
	overrides["WEBPTY_ADDRESS"] = address
	return nil
}

// addressWithPort keeps the configured host and swaps in --port.
func (f *serveFlags) addressWithPort(lookup func(string) (string, bool)) (string, error) {
	n, err := strconv.Atoi(f.port)
	if err != nil || n < minPort || n > maxPort {
		return "", fmt.Errorf("--port %q must be a number from %d to %d", f.port, minPort, maxPort)
	}
	current, _ := lookup("WEBPTY_ADDRESS")
	host, _, err := net.SplitHostPort(stringOr(current, defaultAddress))
	if err != nil {
		return "", fmt.Errorf("WEBPTY_ADDRESS: %w", err)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

func (f *serveFlags) commandOverride(overrides map[string]string) error {
	if !f.commandSet() {
		if f.set.NArg() > 0 {
			return errors.New("command arguments require --cmd")
		}
		return nil
	}
	if f.command == "" || strings.IndexByte(f.command, 0) >= 0 {
		return errors.New("--cmd must name an executable")
	}
	overrides["WEBPTY_COMMAND"] = f.command
	return nil
}

// overlay looks keys up in overrides first, then in lookup.
func overlay(overrides map[string]string, lookup func(string) (string, bool)) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if value, ok := overrides[key]; ok {
			return value, true
		}
		return lookup(key)
	}
}

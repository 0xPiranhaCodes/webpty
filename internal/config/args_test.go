package config_test

import (
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/config"
)

func parseServe(t *testing.T, values map[string]string, args ...string) (config.Config, []string) {
	t.Helper()
	cfg, warnings, err := config.ParseServe(env(values), args, io.Discard)
	if err != nil {
		t.Fatalf("ParseServe(%q) = %v", args, err)
	}
	return cfg, warnings
}

func TestParseServeWithoutFlagsKeepsTheLocalDefault(t *testing.T) {
	cfg, warnings := parseServe(t, nil)
	want, err := config.LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, want) || len(warnings) != 0 {
		t.Fatalf("cfg = %+v warnings %q, want the environment defaults", cfg, warnings)
	}
	if cfg.Address != "127.0.0.1:8000" {
		t.Errorf("Address = %q, want loopback port 8000 like the legacy server", cfg.Address)
	}
}

func TestParseServeCommandFlag(t *testing.T) {
	for name, tc := range map[string]struct {
		args        []string
		command     string
		commandArgs []string
	}{
		"no flags":             {nil, "/bin/sh", nil},
		"long flag":            {[]string{"--cmd", "vim"}, "vim", nil},
		"short flag":           {[]string{"-c", "htop"}, "htop", nil},
		"single dash long":     {[]string{"-cmd", "top"}, "top", nil},
		"no shell splitting":   {[]string{"--cmd=ls -la; rm -rf /"}, "ls -la; rm -rf /", nil},
		"arguments after dash": {[]string{"--cmd", "/usr/bin/tmux", "--", "new", "-A"}, "/usr/bin/tmux", []string{"new", "-A"}},
		"arguments stay literal": {[]string{"--cmd", "/bin/echo", "--", "$HOME", "`id`", "a b"},
			"/bin/echo", []string{"$HOME", "`id`", "a b"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := parseServe(t, nil, tc.args...)
			if cfg.Command != tc.command || !reflect.DeepEqual(cfg.CommandArgs, tc.commandArgs) {
				t.Fatalf("command = %q %v, want %q %v", cfg.Command, cfg.CommandArgs, tc.command, tc.commandArgs)
			}
		})
	}
}

func TestParseServePortKeepsTheConfiguredHost(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"short":            {nil, []string{"-p", "9000"}, "127.0.0.1:9000"},
		"long":             {nil, []string{"--port=9001"}, "127.0.0.1:9001"},
		"environment host": {map[string]string{"WEBPTY_ADDRESS": "localhost:7000"}, []string{"-p", "9002"}, "localhost:9002"},
		"ipv6 host":        {map[string]string{"WEBPTY_ADDRESS": "[::1]:7000"}, []string{"--port", "9003"}, "[::1]:9003"},
		"address flag":     {nil, []string{"--address", "127.0.0.2:9004"}, "127.0.0.2:9004"},
		"flag beats env":   {map[string]string{"WEBPTY_ADDRESS": "127.0.0.1:1"}, []string{"--address", "127.0.0.1:9005"}, "127.0.0.1:9005"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := parseServe(t, tc.env, tc.args...)
			if cfg.Address != tc.want {
				t.Fatalf("Address = %q, want %q", cfg.Address, tc.want)
			}
		})
	}
}

func TestParseServeAppliesDatabaseOriginAndCookieFlags(t *testing.T) {
	cfg, _ := parseServe(t, nil,
		"--address", "0.0.0.0:8443", "--public-origin", "https://pty.example.com", "--database", "/var/lib/webpty/webpty.db")
	if cfg.Address != "0.0.0.0:8443" || cfg.PublicOrigin != "https://pty.example.com" || cfg.DatabasePath != "/var/lib/webpty/webpty.db" {
		t.Fatalf("cfg = %q %q %q", cfg.Address, cfg.PublicOrigin, cfg.DatabasePath)
	}
	if !cfg.SecureCookies {
		t.Error("an https public origin must default to secure cookies")
	}
	cfg, _ = parseServe(t, map[string]string{"WEBPTY_PUBLIC_ORIGIN": "https://pty.example.com"}, "--secure-cookies=false")
	if cfg.SecureCookies {
		t.Error("--secure-cookies=false was ignored")
	}
	cfg, _ = parseServe(t, nil, "--secure-cookies")
	if !cfg.SecureCookies {
		t.Error("--secure-cookies was ignored")
	}
}

// Flags are applied before validation, so a flag can complete a
// configuration the environment alone would reject.
func TestParseServeValidatesTheCombinedConfiguration(t *testing.T) {
	exposed := map[string]string{"WEBPTY_ADDRESS": "0.0.0.0:8000"}
	if _, _, err := config.ParseServe(env(exposed), nil, io.Discard); err == nil {
		t.Fatal("a reachable address without a public origin was accepted")
	}
	parseServe(t, exposed, "--public-origin", "https://pty.example.com")
	if _, _, err := config.ParseServe(env(nil), []string{"--address", "0.0.0.0:8000"}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "WEBPTY_PUBLIC_ORIGIN") {
		t.Fatalf("err = %v, want the public-origin requirement", err)
	}
	denied := map[string]string{"WEBPTY_COMMAND_DENY": "/bin/sh"}
	if _, _, err := config.ParseServe(env(denied), []string{"--cmd", "/bin/sh"}, io.Discard); err == nil {
		t.Fatal("--cmd bypassed the command policy")
	}
}

func TestParseServeRejectsInvalidInput(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":            {"--bogus"},
		"arguments without --cmd": {"extra"},
		"empty command":           {"--cmd", ""},
		"missing flag value":      {"--cmd"},
		"port zero":               {"-p", "0"},
		"port too large":          {"-p", "65536"},
		"port not a number":       {"-p", "http"},
		"port and address":        {"-p", "9000", "--address", "127.0.0.1:9001"},
		"bad origin":              {"--public-origin", "https://example.com/path"},
		"empty database":          {"--database", ""},
	} {
		if _, _, err := config.ParseServe(env(nil), args, io.Discard); err == nil {
			t.Errorf("%s: ParseServe succeeded, want error", name)
		}
	}
	if _, _, err := config.ParseServe(env(nil), []string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h err = %v, want flag.ErrHelp", err)
	}
}

func TestParseServeHelpListsCompatibilityFlags(t *testing.T) {
	var usage strings.Builder
	_, _, _ = config.ParseServe(env(nil), []string{"--help"}, &usage)
	for _, want := range []string{"-port", "-cmd", "-address", "-database", "-public-origin", "-secure-cookies", "-allowed-hosts"} {
		if !strings.Contains(usage.String(), want) {
			t.Errorf("usage lacks %s:\n%s", want, usage.String())
		}
	}
}

// --allowed-hosts never widens access: the Host and Origin checks already
// accept only loopback hosts or the public origin.
func TestParseServeAllowedHostsIsDeprecated(t *testing.T) {
	for _, flagName := range []string{"--allowed-hosts", "-ah"} {
		cfg, warnings := parseServe(t, nil, flagName, "127.0.0.1,pty.example.com")
		want, _ := config.LoadFrom(env(nil))
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("%s changed the configuration: %+v", flagName, cfg)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "deprecated") || !strings.Contains(warnings[0], "--public-origin") {
			t.Errorf("%s warnings = %q, want one deprecation pointing at --public-origin", flagName, warnings)
		}
	}
}

func TestParseServeKeepaliveIsDeprecated(t *testing.T) {
	_, warnings := parseServe(t, nil, "-k", "30")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "WEBPTY_WS_PING_INTERVAL") {
		t.Errorf("warnings = %q, want a pointer to WEBPTY_WS_PING_INTERVAL", warnings)
	}
}

// A password on the command line is visible to every local user, and the
// administrator password is now set through first-run rotation.
func TestParseServeRefusesLegacyPasswordWithoutEchoingIt(t *testing.T) {
	for _, flagName := range []string{"--password", "-pass"} {
		_, _, err := config.ParseServe(env(nil), []string{flagName, "hunter2-secret"}, io.Discard)
		if err == nil {
			t.Fatalf("%s was accepted", flagName)
		}
		if strings.Contains(err.Error(), "hunter2-secret") || !strings.Contains(err.Error(), "CHANGEME") {
			t.Errorf("%s err = %q, want a CHANGEME migration hint without the value", flagName, err)
		}
	}
}

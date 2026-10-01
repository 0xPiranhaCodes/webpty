package config_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/config"
)

func TestNonLoopbackAddressRequiresPublicOrigin(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8000", ":8000", "192.0.2.10:8000", "[::]:8000", "example.com:8000"} {
		_, err := config.LoadFrom(env(map[string]string{"WEBPTY_ADDRESS": address}))
		if err == nil || !strings.Contains(err.Error(), "WEBPTY_PUBLIC_ORIGIN") {
			t.Errorf("address %q without an origin: err = %v", address, err)
		}
		if _, err := config.LoadFrom(env(map[string]string{
			"WEBPTY_ADDRESS": address, "WEBPTY_PUBLIC_ORIGIN": "https://pty.example.com",
		})); err != nil {
			t.Errorf("address %q with an origin: %v", address, err)
		}
	}
	for _, address := range []string{"127.0.0.1:8000", "127.0.0.2:9000", "localhost:8000", "[::1]:8000"} {
		if _, err := config.LoadFrom(env(map[string]string{"WEBPTY_ADDRESS": address})); err != nil {
			t.Errorf("loopback address %q: %v", address, err)
		}
	}
}

func TestIsLoopbackAddress(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:1": true, "[::1]:1": true, "localhost:1": true,
		":1": false, "0.0.0.0:1": false, "10.0.0.1:1": false, "[::]:1": false, "host:1": false,
	}
	for address, want := range cases {
		if got := config.IsLoopbackAddress(address); got != want {
			t.Errorf("IsLoopbackAddress(%q) = %v, want %v", address, got, want)
		}
	}
}

func TestLoadHardeningSettings(t *testing.T) {
	cfg, err := config.LoadFrom(env(map[string]string{
		"WEBPTY_COMMAND":                 "/bin/zsh",
		"WEBPTY_COMMAND_ALLOW":           "/bin/zsh:/bin/bash",
		"WEBPTY_COMMAND_DENY":            "/usr/bin/sudo",
		"WEBPTY_CHILD_ENV_PASSTHROUGH":   "SSH_AUTH_SOCK, EDITOR",
		"WEBPTY_RECORDING_START_TIMEOUT": "3s",
		"WEBPTY_SESSION_SWEEP_INTERVAL":  "2m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.CommandAllow, []string{"/bin/zsh", "/bin/bash"}) ||
		!reflect.DeepEqual(cfg.CommandDeny, []string{"/usr/bin/sudo"}) ||
		!reflect.DeepEqual(cfg.ChildEnvPassthrough, []string{"SSH_AUTH_SOCK", "EDITOR"}) ||
		cfg.RecordingStartTimeout != 3*time.Second || cfg.SessionSweepInterval != 2*time.Minute {
		t.Fatalf("cfg = %+v", cfg)
	}

	defaults, err := config.LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.RecordingStartTimeout != time.Second || defaults.SessionSweepInterval != 10*time.Minute ||
		defaults.CommandAllow != nil || defaults.CommandDeny != nil || defaults.ChildEnvPassthrough != nil {
		t.Fatalf("defaults = %+v", defaults)
	}
}

func TestRejectsUnsafeHardeningSettings(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"relative allow entry":       {"WEBPTY_COMMAND_ALLOW": "zsh"},
		"default command not listed": {"WEBPTY_COMMAND": "/bin/sh", "WEBPTY_COMMAND_ALLOW": "/bin/zsh"},
		"default command denied":     {"WEBPTY_COMMAND": "/bin/sh", "WEBPTY_COMMAND_DENY": "/bin/sh"},
		"webpty passthrough":         {"WEBPTY_CHILD_ENV_PASSTHROUGH": "WEBPTY_SECRET"},
		"invalid passthrough":        {"WEBPTY_CHILD_ENV_PASSTHROUGH": "A=B"},
		"replay below one read":      {"WEBPTY_REPLAY_BYTES": "1024"},
		"zero start timeout":         {"WEBPTY_RECORDING_START_TIMEOUT": "0s"},
	} {
		if _, err := config.LoadFrom(env(values)); err == nil {
			t.Errorf("%s: accepted %v", name, values)
		}
	}
	if _, err := config.LoadFrom(env(map[string]string{"WEBPTY_REPLAY_BYTES": "16432", "WEBPTY_CLIENT_QUEUE_BYTES": "16432"})); err != nil {
		t.Errorf("buffers holding exactly one read chunk: %v", err)
	}
}

func TestWithDefaultsFillsZeroFieldsOnly(t *testing.T) {
	defaults, err := config.LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	got := config.WithDefaults(config.Config{DatabasePath: "custom.db", MaxSessions: 3})
	if got.DatabasePath != "custom.db" || got.MaxSessions != 3 {
		t.Fatalf("explicit values overwritten: %+v", got)
	}
	if got.ShutdownTimeout != defaults.ShutdownTimeout || got.TerminalShutdownTimeout != defaults.TerminalShutdownTimeout ||
		got.TerminalKillGrace != defaults.TerminalKillGrace || got.Address != defaults.Address ||
		got.SessionSweepInterval != defaults.SessionSweepInterval || got.ReplayBytes != defaults.ReplayBytes {
		t.Fatalf("zero fields not defaulted: %+v", got)
	}
	if got.RecordingEnabled {
		t.Fatal("an explicit false boolean was overwritten")
	}
}

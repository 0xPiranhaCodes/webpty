package config_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/config"
)

func env(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		Address:           "127.0.0.1:8000",
		DatabasePath:      "webpty.db",
		PublicOrigin:      "",
		SecureCookies:     false,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ShutdownTimeout:   10 * time.Second,
		SessionTTL:        12 * time.Hour,
		BootstrapTTL:      10 * time.Minute,
		Command:           "/bin/sh",
		MaxSessions:       16,
		MaxViewers:        8,
		TerminalIdle:      time.Hour,
		TerminalKillGrace: 5 * time.Second,
		// Session cleanup gets its own budget, longer than the kill grace.
		TerminalShutdownTimeout: 10 * time.Second,
		ReplayBytes:             256 << 10,
		ClientQueueBytes:        1 << 20,
		WSWriteTimeout:          10 * time.Second,
		WSPingInterval:          30 * time.Second,
		AccessSessionTTL:        12 * time.Hour,
		GrantDefaultTTL:         24 * time.Hour,
		GrantMaxTTL:             7 * 24 * time.Hour,
		GrantMaxRedemptions:     100,

		RecordingEnabled:         true,
		RecordingQueueBytes:      1 << 20,
		RecordingChunkBytes:      64 << 10,
		RecordingChunkEvents:     1024,
		RecordingFlushInterval:   time.Second,
		RecordingRetention:       30 * 24 * time.Hour,
		RecordingMaxBytes:        256 << 20,
		RecordingShutdownTimeout: 5 * time.Second,
		RecordingStartTimeout:    time.Second,
		SessionSweepInterval:     10 * time.Minute,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("defaults = %+v\nwant %+v", cfg, want)
	}
}

func TestLoadAccessSettings(t *testing.T) {
	cfg, err := config.LoadFrom(env(map[string]string{
		"WEBPTY_ACCESS_SESSION_TTL":    "30m",
		"WEBPTY_GRANT_DEFAULT_TTL":     "2h",
		"WEBPTY_GRANT_MAX_TTL":         "48h",
		"WEBPTY_GRANT_MAX_REDEMPTIONS": "5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessSessionTTL != 30*time.Minute || cfg.GrantDefaultTTL != 2*time.Hour ||
		cfg.GrantMaxTTL != 48*time.Hour || cfg.GrantMaxRedemptions != 5 {
		t.Fatalf("access settings = %+v", cfg)
	}
	for name, values := range map[string]map[string]string{
		"default above max": {"WEBPTY_GRANT_DEFAULT_TTL": "3h", "WEBPTY_GRANT_MAX_TTL": "2h"},
		"default below min": {"WEBPTY_GRANT_DEFAULT_TTL": "30s"},
		"zero redemptions":  {"WEBPTY_GRANT_MAX_REDEMPTIONS": "0"},
		"bad session ttl":   {"WEBPTY_ACCESS_SESSION_TTL": "forever"},
		"max below default": {"WEBPTY_GRANT_MAX_TTL": "2h"},
	} {
		if _, err := config.LoadFrom(env(values)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadRecordingSettings(t *testing.T) {
	cfg, err := config.LoadFrom(env(map[string]string{
		"WEBPTY_RECORDING_ENABLED":          "false",
		"WEBPTY_RECORDING_QUEUE_BYTES":      "65536",
		"WEBPTY_RECORDING_CHUNK_BYTES":      "4096",
		"WEBPTY_RECORDING_CHUNK_EVENTS":     "16",
		"WEBPTY_RECORDING_FLUSH_INTERVAL":   "250ms",
		"WEBPTY_RECORDING_RETENTION":        "72h",
		"WEBPTY_RECORDING_MAX_BYTES":        "1048576",
		"WEBPTY_RECORDING_SHUTDOWN_TIMEOUT": "2s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecordingEnabled || cfg.RecordingQueueBytes != 65536 || cfg.RecordingChunkBytes != 4096 ||
		cfg.RecordingChunkEvents != 16 || cfg.RecordingFlushInterval != 250*time.Millisecond ||
		cfg.RecordingRetention != 72*time.Hour || cfg.RecordingMaxBytes != 1<<20 ||
		cfg.RecordingShutdownTimeout != 2*time.Second {
		t.Fatalf("recording settings = %+v", cfg)
	}
	for name, values := range map[string]map[string]string{
		"bad enabled":          {"WEBPTY_RECORDING_ENABLED": "sometimes"},
		"queue too small":      {"WEBPTY_RECORDING_QUEUE_BYTES": "1024"},
		"chunk too small":      {"WEBPTY_RECORDING_CHUNK_BYTES": "512"},
		"chunk too large":      {"WEBPTY_RECORDING_CHUNK_BYTES": "2097152"},
		"too many events":      {"WEBPTY_RECORDING_CHUNK_EVENTS": "100000"},
		"zero events":          {"WEBPTY_RECORDING_CHUNK_EVENTS": "0"},
		"bad flush":            {"WEBPTY_RECORDING_FLUSH_INTERVAL": "0s"},
		"bad retention":        {"WEBPTY_RECORDING_RETENTION": "forever"},
		"max below chunk":      {"WEBPTY_RECORDING_MAX_BYTES": "1024", "WEBPTY_RECORDING_CHUNK_BYTES": "4096"},
		"bad max bytes":        {"WEBPTY_RECORDING_MAX_BYTES": "-5"},
		"bad shutdown timeout": {"WEBPTY_RECORDING_SHUTDOWN_TIMEOUT": "-1s"},
	} {
		if _, err := config.LoadFrom(env(values)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := config.LoadFrom(env(map[string]string{
		"WEBPTY_ADDRESS":             "0.0.0.0:9000",
		"WEBPTY_DATABASE_PATH":       "/var/lib/webpty/data.db",
		"WEBPTY_PUBLIC_ORIGIN":       "https://pty.example.com",
		"WEBPTY_READ_HEADER_TIMEOUT": "2s",
		"WEBPTY_READ_TIMEOUT":        "3s",
		"WEBPTY_WRITE_TIMEOUT":       "4s",
		"WEBPTY_IDLE_TIMEOUT":        "5s",
		"WEBPTY_SHUTDOWN_TIMEOUT":    "6s",
		"WEBPTY_SESSION_TTL":         "1h",
		"WEBPTY_BOOTSTRAP_TTL":       "2m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != "0.0.0.0:9000" || cfg.DatabasePath != "/var/lib/webpty/data.db" {
		t.Errorf("address/database = %q/%q", cfg.Address, cfg.DatabasePath)
	}
	if cfg.PublicOrigin != "https://pty.example.com" {
		t.Errorf("PublicOrigin = %q", cfg.PublicOrigin)
	}
	if !cfg.SecureCookies {
		t.Error("SecureCookies = false for an https public origin, want true")
	}
	if cfg.ReadHeaderTimeout != 2*time.Second || cfg.ReadTimeout != 3*time.Second ||
		cfg.WriteTimeout != 4*time.Second || cfg.IdleTimeout != 5*time.Second ||
		cfg.ShutdownTimeout != 6*time.Second || cfg.SessionTTL != time.Hour || cfg.BootstrapTTL != 2*time.Minute {
		t.Errorf("durations = %+v", cfg)
	}
}

func TestLoadSecureCookiesExplicit(t *testing.T) {
	cfg, err := config.LoadFrom(env(map[string]string{
		"WEBPTY_PUBLIC_ORIGIN":  "https://pty.example.com",
		"WEBPTY_SECURE_COOKIES": "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecureCookies {
		t.Error("SecureCookies = true, want explicit false")
	}

	cfg, err = config.LoadFrom(env(map[string]string{"WEBPTY_SECURE_COOKIES": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SecureCookies {
		t.Error("SecureCookies = false, want explicit true")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"bad duration":      {"WEBPTY_READ_TIMEOUT": "soon"},
		"negative duration": {"WEBPTY_IDLE_TIMEOUT": "-1s"},
		"bad bool":          {"WEBPTY_SECURE_COOKIES": "maybe"},
		"origin with path":  {"WEBPTY_PUBLIC_ORIGIN": "https://pty.example.com/app"},
		"origin no scheme":  {"WEBPTY_PUBLIC_ORIGIN": "pty.example.com"},
		"origin bad scheme": {"WEBPTY_PUBLIC_ORIGIN": "ftp://pty.example.com"},
		"zero ttl":          {"WEBPTY_SESSION_TTL": "0s"},
	} {
		if _, err := config.LoadFrom(env(values)); err == nil {
			t.Errorf("%s: LoadFrom succeeded, want error", name)
		}
	}
}

func TestLoadTerminalSettings(t *testing.T) {
	cfg, err := config.LoadFrom(env(map[string]string{
		"WEBPTY_COMMAND":               "/usr/bin/zsh",
		"SHELL":                        "/bin/bash",
		"WEBPTY_MAX_SESSIONS":          "3",
		"WEBPTY_MAX_VIEWERS":           "2",
		"WEBPTY_TERMINAL_IDLE_TIMEOUT": "10m",
		"WEBPTY_TERMINAL_KILL_GRACE":   "2s",
		"WEBPTY_REPLAY_BYTES":          "65536",
		"WEBPTY_CLIENT_QUEUE_BYTES":    "131072",
		"WEBPTY_WS_WRITE_TIMEOUT":      "3s",
		"WEBPTY_WS_PING_INTERVAL":      "15s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command != "/usr/bin/zsh" || cfg.CommandArgs != nil {
		t.Errorf("command = %q %v, want WEBPTY_COMMAND verbatim", cfg.Command, cfg.CommandArgs)
	}
	if cfg.MaxSessions != 3 || cfg.MaxViewers != 2 || cfg.TerminalIdle != 10*time.Minute ||
		cfg.TerminalKillGrace != 2*time.Second || cfg.ReplayBytes != 65536 || cfg.ClientQueueBytes != 131072 ||
		cfg.WSWriteTimeout != 3*time.Second || cfg.WSPingInterval != 15*time.Second {
		t.Errorf("terminal settings = %+v", cfg)
	}
}

func TestLoadTerminalShutdownTimeout(t *testing.T) {
	cfg, err := config.LoadFrom(env(map[string]string{"WEBPTY_TERMINAL_KILL_GRACE": "30s"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TerminalShutdownTimeout != 35*time.Second {
		t.Errorf("default TerminalShutdownTimeout = %v, want kill grace + 5s", cfg.TerminalShutdownTimeout)
	}
	cfg, err = config.LoadFrom(env(map[string]string{
		"WEBPTY_TERMINAL_KILL_GRACE": "2s", "WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT": "3s"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TerminalShutdownTimeout != 3*time.Second {
		t.Errorf("TerminalShutdownTimeout = %v, want 3s", cfg.TerminalShutdownTimeout)
	}
}

func TestLoadCommandFallsBackToUserShellThenBinSh(t *testing.T) {
	cfg, err := config.LoadFrom(env(map[string]string{"SHELL": "/bin/zsh"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command != "/bin/zsh" {
		t.Errorf("Command = %q, want $SHELL", cfg.Command)
	}
	cfg, err = config.LoadFrom(env(map[string]string{"WEBPTY_COMMAND": "/opt/my shell"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command != "/opt/my shell" || cfg.CommandArgs != nil {
		t.Errorf("Command = %q %v, want the value as one executable path", cfg.Command, cfg.CommandArgs)
	}
}

func TestLoadRejectsInvalidTerminalSettings(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"zero sessions":         {"WEBPTY_MAX_SESSIONS": "0"},
		"bad viewers":           {"WEBPTY_MAX_VIEWERS": "many"},
		"negative replay":       {"WEBPTY_REPLAY_BYTES": "-1"},
		"queue below replay":    {"WEBPTY_REPLAY_BYTES": "4096", "WEBPTY_CLIENT_QUEUE_BYTES": "1024"},
		"bad kill grace":        {"WEBPTY_TERMINAL_KILL_GRACE": "0s"},
		"nul in command":        {"WEBPTY_COMMAND": "/bin/sh\x00"},
		"bad ws write timeout":  {"WEBPTY_WS_WRITE_TIMEOUT": "later"},
		"bad terminal shutdown": {"WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT": "0s"},
		"terminal shutdown not above kill grace": {
			"WEBPTY_TERMINAL_KILL_GRACE": "5s", "WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT": "5s"},
		"kill grace outlasts default shutdown": {
			"WEBPTY_TERMINAL_KILL_GRACE": "20s", "WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT": "10s"},
	} {
		if _, err := config.LoadFrom(env(values)); err == nil {
			t.Errorf("%s: LoadFrom succeeded, want error", name)
		}
	}
}

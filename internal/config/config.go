package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/session"
)

type Config struct {
	Address      string
	DatabasePath string
	// PublicOrigin is the externally visible scheme://host[:port]. When empty,
	// mutating requests must originate from the request's own Host.
	PublicOrigin      string
	SecureCookies     bool
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	SessionTTL        time.Duration
	BootstrapTTL      time.Duration

	// Command is the default terminal executable. It is never split or
	// interpreted by a shell; CommandArgs are its arguments.
	Command           string
	CommandArgs       []string
	MaxSessions       int
	MaxViewers        int
	TerminalIdle      time.Duration
	TerminalKillGrace time.Duration
	// TerminalShutdownTimeout is how long shutdown waits for sessions to
	// exit, separately from ShutdownTimeout, before SIGKILLing them. It must
	// exceed TerminalKillGrace.
	TerminalShutdownTimeout time.Duration
	ReplayBytes             int
	ClientQueueBytes        int
	// WSWriteTimeout bounds each WebSocket message write; WSPingInterval is
	// how often idle WebSocket peers are probed.
	WSWriteTimeout time.Duration
	WSPingInterval time.Duration

	// AccessSessionTTL bounds a redeemed guest session; it never outlives
	// its grant. GrantDefaultTTL applies to grants created without a TTL,
	// and GrantMaxTTL is the longest TTL an administrator may request.
	AccessSessionTTL time.Duration
	GrantDefaultTTL  time.Duration
	GrantMaxTTL      time.Duration
	// GrantMaxRedemptions is the default and maximum number of times one
	// grant may be redeemed.
	GrantMaxRedemptions int

	// RecordingEnabled records new terminals unless a request opts out.
	// Recording queues and chunks are bounded in bytes and events; a
	// recording that exceeds RecordingMaxBytes or cannot be flushed within
	// RecordingShutdownTimeout is kept but marked incomplete. Ended
	// recordings are deleted after RecordingRetention.
	RecordingEnabled         bool
	RecordingQueueBytes      int
	RecordingChunkBytes      int
	RecordingChunkEvents     int
	RecordingFlushInterval   time.Duration
	RecordingRetention       time.Duration
	RecordingMaxBytes        int64
	RecordingShutdownTimeout time.Duration
	// RecordingStartTimeout bounds how long creating a recording may delay
	// a terminal's start.
	RecordingStartTimeout time.Duration

	// CommandAllow and CommandDeny are absolute executable paths sessions
	// may and may not run; see session.CommandPolicy.
	CommandAllow []string
	CommandDeny  []string
	// ChildEnvPassthrough names environment variables passed to terminal
	// children beyond the default allowlist; see pty.ChildEnvironment.
	ChildEnvPassthrough []string
	// SessionSweepInterval is how often expired login and guest sessions
	// are deleted.
	SessionSweepInterval time.Duration
}

// minGrantTTL matches access.MinGrantTTL.
const minGrantTTL = time.Minute

// These bounds match the recording package's configuration limits.
const (
	minRecordingQueueBytes  = 16 << 10
	minRecordingChunkBytes  = 1 << 10
	maxRecordingChunkBytes  = 1 << 20
	maxRecordingChunkEvents = 1 << 16
)

// minStreamBufferBytes is one PTY read chunk plus its per-event overhead
// (session.readChunk + session.EventOverhead): a smaller replay buffer or
// client queue could not hold a single burst of output.
const minStreamBufferBytes = 16<<10 + 48

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(os.LookupEnv)
}

// LoadFrom reads configuration using lookup. Unset or empty values use defaults.
func LoadFrom(lookup func(string) (string, bool)) (Config, error) {
	get := func(key string) string {
		value, _ := lookup(key)
		return value
	}
	cfg := Config{
		Address:      stringOr(get("WEBPTY_ADDRESS"), "127.0.0.1:8000"),
		DatabasePath: stringOr(get("WEBPTY_DATABASE_PATH"), "webpty.db"),
		PublicOrigin: get("WEBPTY_PUBLIC_ORIGIN"),
		Command:      stringOr(get("WEBPTY_COMMAND"), stringOr(get("SHELL"), "/bin/sh")),
	}

	var errs []error
	if strings.IndexByte(cfg.Command, 0) >= 0 {
		errs = append(errs, errors.New("WEBPTY_COMMAND: must not contain a NUL byte"))
	}
	if cfg.PublicOrigin != "" {
		if err := validateOrigin(cfg.PublicOrigin); err != nil {
			errs = append(errs, err)
		}
	}

	cfg.SecureCookies = strings.HasPrefix(cfg.PublicOrigin, "https://")
	if raw := get("WEBPTY_SECURE_COOKIES"); raw != "" {
		secure, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("WEBPTY_SECURE_COOKIES: %q is not a boolean", raw))
		}
		cfg.SecureCookies = secure
	}
	cfg.RecordingEnabled = true
	if raw := get("WEBPTY_RECORDING_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("WEBPTY_RECORDING_ENABLED: %q is not a boolean", raw))
		}
		cfg.RecordingEnabled = enabled
	}

	durations := []struct {
		key      string
		target   *time.Duration
		fallback time.Duration
	}{
		{"WEBPTY_READ_HEADER_TIMEOUT", &cfg.ReadHeaderTimeout, 5 * time.Second},
		{"WEBPTY_READ_TIMEOUT", &cfg.ReadTimeout, 15 * time.Second},
		{"WEBPTY_WRITE_TIMEOUT", &cfg.WriteTimeout, 30 * time.Second},
		{"WEBPTY_IDLE_TIMEOUT", &cfg.IdleTimeout, 120 * time.Second},
		{"WEBPTY_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout, 10 * time.Second},
		{"WEBPTY_SESSION_TTL", &cfg.SessionTTL, 12 * time.Hour},
		{"WEBPTY_BOOTSTRAP_TTL", &cfg.BootstrapTTL, 10 * time.Minute},
		{"WEBPTY_TERMINAL_IDLE_TIMEOUT", &cfg.TerminalIdle, time.Hour},
		{"WEBPTY_TERMINAL_KILL_GRACE", &cfg.TerminalKillGrace, 5 * time.Second},
		{"WEBPTY_WS_WRITE_TIMEOUT", &cfg.WSWriteTimeout, 10 * time.Second},
		{"WEBPTY_WS_PING_INTERVAL", &cfg.WSPingInterval, 30 * time.Second},
		{"WEBPTY_ACCESS_SESSION_TTL", &cfg.AccessSessionTTL, 12 * time.Hour},
		{"WEBPTY_GRANT_DEFAULT_TTL", &cfg.GrantDefaultTTL, 24 * time.Hour},
		{"WEBPTY_GRANT_MAX_TTL", &cfg.GrantMaxTTL, 7 * 24 * time.Hour},
		{"WEBPTY_RECORDING_FLUSH_INTERVAL", &cfg.RecordingFlushInterval, time.Second},
		{"WEBPTY_RECORDING_RETENTION", &cfg.RecordingRetention, 30 * 24 * time.Hour},
		{"WEBPTY_RECORDING_SHUTDOWN_TIMEOUT", &cfg.RecordingShutdownTimeout, 5 * time.Second},
		{"WEBPTY_RECORDING_START_TIMEOUT", &cfg.RecordingStartTimeout, time.Second},
		{"WEBPTY_SESSION_SWEEP_INTERVAL", &cfg.SessionSweepInterval, 10 * time.Minute},
	}
	for _, d := range durations {
		*d.target = d.fallback
		raw := get(d.key)
		if raw == "" {
			continue
		}
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			errs = append(errs, fmt.Errorf("%s: %q is not a positive duration", d.key, raw))
			continue
		}
		*d.target = value
	}
	if cfg.GrantDefaultTTL < minGrantTTL || cfg.GrantDefaultTTL > cfg.GrantMaxTTL {
		errs = append(errs, errors.New("WEBPTY_GRANT_DEFAULT_TTL must be at least 1m and at most WEBPTY_GRANT_MAX_TTL"))
	}
	cfg.TerminalShutdownTimeout = cfg.TerminalKillGrace + 5*time.Second
	if raw := get("WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT"); raw != "" {
		value, err := time.ParseDuration(raw)
		switch {
		case err != nil || value <= 0:
			errs = append(errs, fmt.Errorf("WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT: %q is not a positive duration", raw))
		case value <= cfg.TerminalKillGrace:
			errs = append(errs, errors.New("WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT must exceed WEBPTY_TERMINAL_KILL_GRACE"))
		default:
			cfg.TerminalShutdownTimeout = value
		}
	}

	integers := []struct {
		key      string
		target   *int
		fallback int
	}{
		{"WEBPTY_MAX_SESSIONS", &cfg.MaxSessions, 16},
		{"WEBPTY_MAX_VIEWERS", &cfg.MaxViewers, 8},
		{"WEBPTY_REPLAY_BYTES", &cfg.ReplayBytes, 256 << 10},
		{"WEBPTY_CLIENT_QUEUE_BYTES", &cfg.ClientQueueBytes, 1 << 20},
		{"WEBPTY_GRANT_MAX_REDEMPTIONS", &cfg.GrantMaxRedemptions, 100},
		{"WEBPTY_RECORDING_QUEUE_BYTES", &cfg.RecordingQueueBytes, 1 << 20},
		{"WEBPTY_RECORDING_CHUNK_BYTES", &cfg.RecordingChunkBytes, 64 << 10},
		{"WEBPTY_RECORDING_CHUNK_EVENTS", &cfg.RecordingChunkEvents, 1024},
	}
	for _, n := range integers {
		*n.target = n.fallback
		raw := get(n.key)
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			errs = append(errs, fmt.Errorf("%s: %q is not a positive integer", n.key, raw))
			continue
		}
		*n.target = value
	}
	if cfg.ClientQueueBytes < cfg.ReplayBytes {
		errs = append(errs, errors.New("WEBPTY_CLIENT_QUEUE_BYTES must be at least WEBPTY_REPLAY_BYTES"))
	}
	if cfg.ReplayBytes < minStreamBufferBytes {
		errs = append(errs, fmt.Errorf("WEBPTY_REPLAY_BYTES must be at least %d", minStreamBufferBytes))
	}
	cfg.RecordingMaxBytes = 256 << 20
	if raw := get("WEBPTY_RECORDING_MAX_BYTES"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			errs = append(errs, fmt.Errorf("WEBPTY_RECORDING_MAX_BYTES: %q is not a positive integer", raw))
		} else {
			cfg.RecordingMaxBytes = value
		}
	}
	if cfg.RecordingQueueBytes < minRecordingQueueBytes {
		errs = append(errs, fmt.Errorf("WEBPTY_RECORDING_QUEUE_BYTES must be at least %d", minRecordingQueueBytes))
	}
	if cfg.RecordingChunkBytes < minRecordingChunkBytes || cfg.RecordingChunkBytes > maxRecordingChunkBytes {
		errs = append(errs, fmt.Errorf("WEBPTY_RECORDING_CHUNK_BYTES must be %d..%d", minRecordingChunkBytes, maxRecordingChunkBytes))
	}
	if cfg.RecordingChunkEvents > maxRecordingChunkEvents {
		errs = append(errs, fmt.Errorf("WEBPTY_RECORDING_CHUNK_EVENTS must be at most %d", maxRecordingChunkEvents))
	}
	if cfg.RecordingMaxBytes < int64(cfg.RecordingChunkBytes) {
		errs = append(errs, errors.New("WEBPTY_RECORDING_MAX_BYTES must be at least WEBPTY_RECORDING_CHUNK_BYTES"))
	}

	if cfg.PublicOrigin == "" && !IsLoopbackAddress(cfg.Address) {
		errs = append(errs, fmt.Errorf("WEBPTY_ADDRESS %q is reachable from other hosts; set WEBPTY_PUBLIC_ORIGIN to the URL browsers use", cfg.Address))
	}
	cfg.CommandAllow = pathList(get("WEBPTY_COMMAND_ALLOW"))
	cfg.CommandDeny = pathList(get("WEBPTY_COMMAND_DENY"))
	for key, paths := range map[string][]string{"WEBPTY_COMMAND_ALLOW": cfg.CommandAllow, "WEBPTY_COMMAND_DENY": cfg.CommandDeny} {
		for _, path := range paths {
			if !filepath.IsAbs(path) {
				errs = append(errs, fmt.Errorf("%s: %q is not an absolute path", key, path))
			}
		}
	}
	if err := cfg.checkCommand(); err != nil {
		errs = append(errs, err)
	}
	for _, name := range nameList(get("WEBPTY_CHILD_ENV_PASSTHROUGH")) {
		if !envName.MatchString(name) || strings.HasPrefix(name, "WEBPTY_") {
			errs = append(errs, fmt.Errorf("WEBPTY_CHILD_ENV_PASSTHROUGH: %q is not a passable variable name", name))
			continue
		}
		cfg.ChildEnvPassthrough = append(cfg.ChildEnvPassthrough, name)
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// WithDefaults returns cfg with every zero-valued field, other than
// booleans, set to its default. It lets callers that build a Config
// directly, rather than through Load, still get safe timeouts and limits.
func WithDefaults(cfg Config) Config {
	defaults, err := LoadFrom(func(string) (string, bool) { return "", false })
	if err != nil {
		panic(fmt.Sprintf("config: defaults are invalid: %v", err))
	}
	out := reflect.ValueOf(&cfg).Elem()
	fallback := reflect.ValueOf(defaults)
	for i := 0; i < out.NumField(); i++ {
		field := out.Field(i)
		if field.Kind() != reflect.Bool && field.IsZero() {
			field.Set(fallback.Field(i))
		}
	}
	return cfg
}

// checkCommand rejects a default command the command policy forbids, so
// the server never starts unable to open its own default terminal.
func (cfg Config) checkCommand() error {
	policy, err := session.NewCommandPolicy(cfg.CommandAllow, cfg.CommandDeny)
	if err != nil {
		return err
	}
	if !policy.Permits(cfg.Command) {
		return fmt.Errorf("default command %q is not permitted by WEBPTY_COMMAND_ALLOW/WEBPTY_COMMAND_DENY", cfg.Command)
	}
	return nil
}

// IsLoopbackAddress reports whether a host:port listen address accepts
// connections only from this machine. An empty host listens everywhere.
func IsLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// pathList splits a PATH-style list, dropping empty entries.
func pathList(raw string) []string {
	var out []string
	for _, entry := range filepath.SplitList(raw) {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

func nameList(raw string) []string {
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

func stringOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func validateOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("WEBPTY_PUBLIC_ORIGIN: %q must be http(s)://host[:port]", origin)
	}
	return nil
}

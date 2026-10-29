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

// Defaults that are not durations or plain integers.
const (
	defaultAddress      = "127.0.0.1:8000"
	defaultDatabasePath = "webpty.db"
	defaultCommand      = "/bin/sh"
	// defaultShutdownMargin is how much longer than the kill grace shutdown
	// waits for terminals, so a SIGTERMed child can still exit on its own.
	defaultShutdownMargin    = 5 * time.Second
	defaultRecordingMaxBytes = 256 << 20
)

// loader reads environment values and collects every validation error, so
// one run reports all bad settings instead of the first one.
type loader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (l *loader) get(key string) string {
	value, _ := l.lookup(key)
	return value
}

func (l *loader) fail(err error) {
	l.errs = append(l.errs, err)
}

func (l *loader) failf(format string, args ...any) {
	l.fail(fmt.Errorf(format, args...))
}

// LoadFrom reads configuration using lookup. Unset or empty values use defaults.
// The steps run in a fixed order so the joined error message is stable.
func LoadFrom(lookup func(string) (string, bool)) (Config, error) {
	l := &loader{lookup: lookup}
	cfg := l.loadStrings()
	l.loadBools(&cfg)
	l.loadDurations(&cfg)
	l.loadIntegers(&cfg)
	l.loadRecordingMaxBytes(&cfg)
	l.checkRecordingBounds(cfg)
	l.loadCommandPolicy(&cfg)
	l.loadChildEnv(&cfg)
	if err := errors.Join(l.errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func (l *loader) loadStrings() Config {
	cfg := Config{
		Address:      stringOr(l.get("WEBPTY_ADDRESS"), defaultAddress),
		DatabasePath: stringOr(l.get("WEBPTY_DATABASE_PATH"), defaultDatabasePath),
		PublicOrigin: l.get("WEBPTY_PUBLIC_ORIGIN"),
		Command:      stringOr(l.get("WEBPTY_COMMAND"), stringOr(l.get("SHELL"), defaultCommand)),
	}
	if strings.IndexByte(cfg.Command, 0) >= 0 {
		l.fail(errors.New("WEBPTY_COMMAND: must not contain a NUL byte"))
	}
	if cfg.PublicOrigin != "" {
		if err := validateOrigin(cfg.PublicOrigin); err != nil {
			l.fail(err)
		}
	}
	return cfg
}

func (l *loader) loadBools(cfg *Config) {
	cfg.SecureCookies = l.boolOr("WEBPTY_SECURE_COOKIES", strings.HasPrefix(cfg.PublicOrigin, "https://"))
	cfg.RecordingEnabled = l.boolOr("WEBPTY_RECORDING_ENABLED", true)
}

// boolOr parses key, keeping fallback when unset. An invalid value is
// reported and, as before this refactor, still assigned as false.
func (l *loader) boolOr(key string, fallback bool) bool {
	raw := l.get(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		l.failf("%s: %q is not a boolean", key, raw)
	}
	return value
}

type durationSetting struct {
	key      string
	target   *time.Duration
	fallback time.Duration
}

func durationSettings(cfg *Config) []durationSetting {
	const day = 24 * time.Hour
	return []durationSetting{
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
		{"WEBPTY_GRANT_DEFAULT_TTL", &cfg.GrantDefaultTTL, day},
		{"WEBPTY_GRANT_MAX_TTL", &cfg.GrantMaxTTL, 7 * day},
		{"WEBPTY_RECORDING_FLUSH_INTERVAL", &cfg.RecordingFlushInterval, time.Second},
		{"WEBPTY_RECORDING_RETENTION", &cfg.RecordingRetention, 30 * day},
		{"WEBPTY_RECORDING_SHUTDOWN_TIMEOUT", &cfg.RecordingShutdownTimeout, 5 * time.Second},
		{"WEBPTY_RECORDING_START_TIMEOUT", &cfg.RecordingStartTimeout, time.Second},
		{"WEBPTY_SESSION_SWEEP_INTERVAL", &cfg.SessionSweepInterval, 10 * time.Minute},
	}
}

func (l *loader) loadDurations(cfg *Config) {
	for _, d := range durationSettings(cfg) {
		*d.target = d.fallback
		if value, ok := l.positiveDuration(d.key); ok {
			*d.target = value
		}
	}
	if cfg.GrantDefaultTTL < minGrantTTL || cfg.GrantDefaultTTL > cfg.GrantMaxTTL {
		l.fail(errors.New("WEBPTY_GRANT_DEFAULT_TTL must be at least 1m and at most WEBPTY_GRANT_MAX_TTL"))
	}
	l.loadTerminalShutdownTimeout(cfg)
}

// positiveDuration parses key; ok is false when it is unset or invalid,
// and an invalid value is reported.
func (l *loader) positiveDuration(key string) (value time.Duration, ok bool) {
	raw := l.get(key)
	if raw == "" {
		return 0, false
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		l.failf("%s: %q is not a positive duration", key, raw)
		return 0, false
	}
	return value, true
}

// loadTerminalShutdownTimeout depends on TerminalKillGrace, so it runs
// after the plain durations.
func (l *loader) loadTerminalShutdownTimeout(cfg *Config) {
	cfg.TerminalShutdownTimeout = cfg.TerminalKillGrace + defaultShutdownMargin
	value, ok := l.positiveDuration("WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT")
	switch {
	case !ok:
	case value <= cfg.TerminalKillGrace:
		l.fail(errors.New("WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT must exceed WEBPTY_TERMINAL_KILL_GRACE"))
	default:
		cfg.TerminalShutdownTimeout = value
	}
}

type integerSetting struct {
	key      string
	target   *int
	fallback int
}

func integerSettings(cfg *Config) []integerSetting {
	return []integerSetting{
		{"WEBPTY_MAX_SESSIONS", &cfg.MaxSessions, 16},
		{"WEBPTY_MAX_VIEWERS", &cfg.MaxViewers, 8},
		{"WEBPTY_REPLAY_BYTES", &cfg.ReplayBytes, 256 << 10},
		{"WEBPTY_CLIENT_QUEUE_BYTES", &cfg.ClientQueueBytes, 1 << 20},
		{"WEBPTY_GRANT_MAX_REDEMPTIONS", &cfg.GrantMaxRedemptions, 100},
		{"WEBPTY_RECORDING_QUEUE_BYTES", &cfg.RecordingQueueBytes, 1 << 20},
		{"WEBPTY_RECORDING_CHUNK_BYTES", &cfg.RecordingChunkBytes, 64 << 10},
		{"WEBPTY_RECORDING_CHUNK_EVENTS", &cfg.RecordingChunkEvents, 1024},
	}
}

func (l *loader) loadIntegers(cfg *Config) {
	for _, n := range integerSettings(cfg) {
		*n.target = n.fallback
		if raw := l.get(n.key); raw != "" {
			if value, err := strconv.Atoi(raw); err != nil || value <= 0 {
				l.failf("%s: %q is not a positive integer", n.key, raw)
			} else {
				*n.target = value
			}
		}
	}
	if cfg.ClientQueueBytes < cfg.ReplayBytes {
		l.fail(errors.New("WEBPTY_CLIENT_QUEUE_BYTES must be at least WEBPTY_REPLAY_BYTES"))
	}
	if cfg.ReplayBytes < minStreamBufferBytes {
		l.failf("WEBPTY_REPLAY_BYTES must be at least %d", minStreamBufferBytes)
	}
}

func (l *loader) loadRecordingMaxBytes(cfg *Config) {
	cfg.RecordingMaxBytes = defaultRecordingMaxBytes
	raw := l.get("WEBPTY_RECORDING_MAX_BYTES")
	if raw == "" {
		return
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		l.failf("WEBPTY_RECORDING_MAX_BYTES: %q is not a positive integer", raw)
		return
	}
	cfg.RecordingMaxBytes = value
}

func (l *loader) checkRecordingBounds(cfg Config) {
	if cfg.RecordingQueueBytes < minRecordingQueueBytes {
		l.failf("WEBPTY_RECORDING_QUEUE_BYTES must be at least %d", minRecordingQueueBytes)
	}
	if cfg.RecordingChunkBytes < minRecordingChunkBytes || cfg.RecordingChunkBytes > maxRecordingChunkBytes {
		l.failf("WEBPTY_RECORDING_CHUNK_BYTES must be %d..%d", minRecordingChunkBytes, maxRecordingChunkBytes)
	}
	if cfg.RecordingChunkEvents > maxRecordingChunkEvents {
		l.failf("WEBPTY_RECORDING_CHUNK_EVENTS must be at most %d", maxRecordingChunkEvents)
	}
	if cfg.RecordingMaxBytes < int64(cfg.RecordingChunkBytes) {
		l.fail(errors.New("WEBPTY_RECORDING_MAX_BYTES must be at least WEBPTY_RECORDING_CHUNK_BYTES"))
	}
}

// loadCommandPolicy also enforces the loopback default: a non-loopback
// listener needs an explicit public origin.
func (l *loader) loadCommandPolicy(cfg *Config) {
	if cfg.PublicOrigin == "" && !IsLoopbackAddress(cfg.Address) {
		l.failf("WEBPTY_ADDRESS %q is reachable from other hosts; set WEBPTY_PUBLIC_ORIGIN to the URL browsers use", cfg.Address)
	}
	cfg.CommandAllow = pathList(l.get("WEBPTY_COMMAND_ALLOW"))
	cfg.CommandDeny = pathList(l.get("WEBPTY_COMMAND_DENY"))
	for key, paths := range map[string][]string{"WEBPTY_COMMAND_ALLOW": cfg.CommandAllow, "WEBPTY_COMMAND_DENY": cfg.CommandDeny} {
		for _, path := range paths {
			if !filepath.IsAbs(path) {
				l.failf("%s: %q is not an absolute path", key, path)
			}
		}
	}
	if err := cfg.checkCommand(); err != nil {
		l.fail(err)
	}
}

func (l *loader) loadChildEnv(cfg *Config) {
	for _, name := range nameList(l.get("WEBPTY_CHILD_ENV_PASSTHROUGH")) {
		if !envName.MatchString(name) || strings.HasPrefix(name, "WEBPTY_") {
			l.failf("WEBPTY_CHILD_ENV_PASSTHROUGH: %q is not a passable variable name", name)
			continue
		}
		cfg.ChildEnvPassthrough = append(cfg.ChildEnvPassthrough, name)
	}
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
	return trimmedNonEmpty(filepath.SplitList(raw))
}

func nameList(raw string) []string {
	return trimmedNonEmpty(strings.Split(raw, ","))
}

func trimmedNonEmpty(entries []string) []string {
	var out []string
	for _, entry := range entries {
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

package app

import (
	"strconv"

	"github.com/0xPiranhaCodes/webpty/internal/config"
	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
)

// runtimeSettings lists the configuration shown to the administrator. It
// never includes the password, tokens, or the command's arguments, which may
// carry credentials; only their count is shown.
func runtimeSettings(cfg config.Config) []httpapi.Setting {
	orDefault := func(value, fallback string) string {
		if value == "" {
			return fallback
		}
		return value
	}
	arguments := strconv.Itoa(len(cfg.CommandArgs)) + " arguments"
	if len(cfg.CommandArgs) == 1 {
		arguments = "1 argument"
	}
	s := func(group, key, label, value string) httpapi.Setting {
		return httpapi.Setting{Key: key, Group: group, Label: label, Value: value, RestartRequired: true}
	}
	return []httpapi.Setting{
		s("Server", "WEBPTY_ADDRESS", "Listen address", cfg.Address),
		s("Server", "WEBPTY_PUBLIC_ORIGIN", "Public origin", orDefault(cfg.PublicOrigin, "Not set (loopback only)")),
		s("Server", "WEBPTY_SECURE_COOKIES", "Secure cookies", strconv.FormatBool(cfg.SecureCookies)),
		s("Server", "WEBPTY_DATABASE_PATH", "Database file", cfg.DatabasePath),
		s("Server", "WEBPTY_SESSION_TTL", "Admin session lifetime", cfg.SessionTTL.String()),
		s("Terminals", "WEBPTY_COMMAND", "Default command", cfg.Command),
		s("Terminals", "--cmd … -- ARGS", "Default command arguments", arguments),
		s("Terminals", "WEBPTY_MAX_SESSIONS", "Maximum running terminals", strconv.Itoa(cfg.MaxSessions)),
		s("Terminals", "WEBPTY_MAX_VIEWERS", "Maximum participants per terminal", strconv.Itoa(cfg.MaxViewers)),
		s("Terminals", "WEBPTY_TERMINAL_IDLE_TIMEOUT", "Idle timeout without participants", cfg.TerminalIdle.String()),
		s("Terminals", "WEBPTY_REPLAY_BYTES", "Reconnect replay buffer (bytes)", strconv.Itoa(cfg.ReplayBytes)),
		s("Sharing", "WEBPTY_GRANT_DEFAULT_TTL", "Default link lifetime", cfg.GrantDefaultTTL.String()),
		s("Sharing", "WEBPTY_GRANT_MAX_TTL", "Longest link lifetime", cfg.GrantMaxTTL.String()),
		s("Sharing", "WEBPTY_GRANT_MAX_REDEMPTIONS", "Maximum uses per link", strconv.Itoa(cfg.GrantMaxRedemptions)),
		s("Sharing", "WEBPTY_ACCESS_SESSION_TTL", "Guest session lifetime", cfg.AccessSessionTTL.String()),
		s("Recording", "WEBPTY_RECORDING_ENABLED", "Record new terminals", strconv.FormatBool(cfg.RecordingEnabled)),
		s("Recording", "WEBPTY_RECORDING_RETENTION", "Retention", cfg.RecordingRetention.String()),
		s("Recording", "WEBPTY_RECORDING_MAX_BYTES", "Maximum recording size (bytes)", strconv.FormatInt(cfg.RecordingMaxBytes, 10)),
	}
}

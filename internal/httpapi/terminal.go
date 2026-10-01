package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/collab"
	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/recording"
	"github.com/0xPiranhaCodes/webpty/internal/session"
)

// TerminalSettings tune the terminal transport. Zero fields receive defaults.
type TerminalSettings struct {
	// WriteTimeout bounds each WebSocket message write.
	WriteTimeout time.Duration
	// PingInterval is how often the peer is probed and the admin session
	// re-validated.
	PingInterval time.Duration
	// Heartbeat returns the ticks that drive each connection's heartbeat
	// and a function that stops them. A time.Ticker is used when nil.
	Heartbeat func(interval time.Duration) (<-chan time.Time, func())
	// MaxMessageBytes limits one client WebSocket message.
	MaxMessageBytes int64
	// InputQueueBytes bounds input one connection may have waiting for the
	// PTY. A client that exceeds it is closed with CloseInputOverflow. One
	// message is always accepted into an empty queue, whatever its size.
	InputQueueBytes int
	// TerminateTimeout bounds how long DELETE waits for the process to exit.
	TerminateTimeout time.Duration
	// Access, when set, enables capability grants: the admin grant routes,
	// guest redemption and sessions, and guest WebSocket connections.
	Access *access.Service
	// Presence tracks participants; a private hub is used when nil.
	Presence *collab.Hub
	// Recordings, when set, warns owners over the WebSocket when the
	// terminal's recording stops early. Guests are never told.
	Recordings *recording.Service
	// RedeemThrottle limits invitation redemption attempts per client.
	RedeemThrottle *auth.Throttle
	Logger         *slog.Logger
}

type terminalAPI struct {
	admin    *adminAPI
	manager  *session.Manager
	access   *access.Service
	presence *collab.Hub
	settings TerminalSettings
}

// WithTerminals mounts the admin terminal REST API and the terminal
// WebSocket, plus the capability-grant routes when settings.Access is set.
func WithTerminals(service *auth.Service, security SecuritySettings, manager *session.Manager, settings TerminalSettings) Option {
	if settings.WriteTimeout <= 0 {
		settings.WriteTimeout = 10 * time.Second
	}
	if settings.PingInterval <= 0 {
		settings.PingInterval = 30 * time.Second
	}
	if settings.Heartbeat == nil {
		settings.Heartbeat = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	if settings.MaxMessageBytes <= 0 {
		settings.MaxMessageBytes = 64 << 10
	}
	if settings.InputQueueBytes <= 0 {
		settings.InputQueueBytes = 256 << 10
	}
	if settings.TerminateTimeout <= 0 {
		settings.TerminateTimeout = 15 * time.Second
	}
	if settings.Logger == nil {
		settings.Logger = slog.Default()
	}
	if settings.Presence == nil {
		settings.Presence = collab.NewHub(collab.Config{})
	}
	if settings.RedeemThrottle == nil {
		settings.RedeemThrottle = auth.NewThrottle(auth.ThrottleConfig{Limit: 30, Window: time.Minute})
	}
	a := &adminAPI{service: service, security: security, guests: settings.Access}
	api := &terminalAPI{admin: a, manager: manager, access: settings.Access, presence: settings.Presence, settings: settings}
	read, mutate := a.adminRoutes()
	return func(mux *http.ServeMux) {
		mux.Handle("POST /api/v1/admin/terminals", mutate(api.create))
		mux.Handle("GET /api/v1/admin/terminals", read(api.list))
		mux.Handle("GET /api/v1/admin/terminals/{id}", read(api.get))
		mux.Handle("DELETE /api/v1/admin/terminals/{id}", mutate(api.terminate))
		// Browsers cannot attach CSRF headers to WebSocket handshakes; the
		// SameSite=Strict cookies and the enforced Origin protect it instead.
		mux.Handle("GET /api/v1/terminals/{id}/ws", a.trustedHost(a.sameOrigin(api.terminalViewer(http.HandlerFunc(api.websocket)))))
		if api.access != nil {
			api.mountAccess(mux, read, mutate)
		}
	}
}

type terminalJSON struct {
	ID             string     `json:"id"`
	State          string     `json:"state"`
	Command        string     `json:"command"`
	Args           []string   `json:"args"`
	Rows           int        `json:"rows"`
	Cols           int        `json:"cols"`
	CreatedAt      time.Time  `json:"createdAt"`
	StartedAt      *time.Time `json:"startedAt"`
	EndedAt        *time.Time `json:"endedAt"`
	LastActivityAt time.Time  `json:"lastActivityAt"`
	ExitCode       *int       `json:"exitCode"`
	ExitSignal     string     `json:"exitSignal,omitempty"`
	Failure        string     `json:"failure,omitempty"`
}

func toTerminalJSON(info session.Info) terminalJSON {
	optional := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		utc := t.UTC()
		return &utc
	}
	args := info.Args
	if args == nil {
		args = []string{}
	}
	return terminalJSON{
		ID: info.PublicID, State: string(info.State), Command: info.Command, Args: args,
		Rows: info.Rows, Cols: info.Cols, CreatedAt: info.CreatedAt.UTC(), StartedAt: optional(info.StartedAt),
		EndedAt: optional(info.EndedAt), LastActivityAt: info.LastActivityAt.UTC(), ExitCode: info.ExitCode,
		ExitSignal: info.ExitSignal, Failure: info.Failure,
	}
}

// adminTerminalJSON is terminal metadata plus live participant counts, which
// only administrators see.
type adminTerminalJSON struct {
	terminalJSON
	Participants int `json:"participants"`
}

func (api *terminalAPI) adminTerminal(info session.Info) adminTerminalJSON {
	return adminTerminalJSON{toTerminalJSON(info), api.presence.ParticipantCount(info.PublicID)}
}

func (api *terminalAPI) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Rows    int      `json:"rows"`
		Cols    int      `json:"cols"`
		// Record, when false, opts this terminal out of recording.
		Record *bool `json:"record"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	info, err := api.manager.Create(r.Context(), session.CreateRequest{
		Command:     pty.Command{Path: body.Command, Args: body.Args},
		Rows:        body.Rows,
		Cols:        body.Cols,
		RemoteAddr:  remoteIP(r),
		NoRecording: body.Record != nil && !*body.Record,
	})
	if err != nil {
		api.writeError(w, "create terminal", err)
		return
	}
	writeJSON(w, http.StatusCreated, api.adminTerminal(info))
}

func (api *terminalAPI) list(w http.ResponseWriter, r *http.Request) {
	sessions, err := api.manager.List(r.Context())
	if err != nil {
		api.writeError(w, "list terminals", err)
		return
	}
	body := struct {
		Sessions []adminTerminalJSON `json:"sessions"`
	}{Sessions: make([]adminTerminalJSON, 0, len(sessions))}
	for _, info := range sessions {
		body.Sessions = append(body.Sessions, api.adminTerminal(info))
	}
	writeJSON(w, http.StatusOK, body)
}

func (api *terminalAPI) get(w http.ResponseWriter, r *http.Request) {
	info, err := api.manager.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		api.writeError(w, "get terminal", err)
		return
	}
	writeJSON(w, http.StatusOK, api.adminTerminal(info))
}

func (api *terminalAPI) terminate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), api.settings.TerminateTimeout)
	defer cancel()
	info, err := api.manager.Terminate(ctx, r.PathValue("id"), remoteIP(r))
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusAccepted, api.adminTerminal(info))
	case err != nil:
		api.writeError(w, "terminate terminal", err)
	default:
		writeJSON(w, http.StatusOK, api.adminTerminal(info))
	}
}

// writeError maps session errors to stable status codes and error codes.
// Messages are fixed strings so process and store details never leak.
func (api *terminalAPI) writeError(w http.ResponseWriter, operation string, err error) {
	status, code, message := http.StatusInternalServerError, "internal", "internal error"
	switch {
	case errors.Is(err, session.ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", "terminal session not found"
	case errors.Is(err, session.ErrCommandDenied):
		status, code, message = http.StatusForbidden, "command_denied", "command is not permitted on this server"
	case errors.Is(err, session.ErrSessionLimit):
		status, code, message = http.StatusTooManyRequests, "session_limit", "active terminal session limit reached"
	case errors.Is(err, session.ErrViewerLimit):
		status, code, message = http.StatusTooManyRequests, "viewer_limit", "terminal viewer limit reached"
	case errors.Is(err, session.ErrInvalidState):
		status, code, message = http.StatusConflict, "invalid_state", "terminal session is not running"
	case errors.Is(err, session.ErrInvalidArgument):
		status, code, message = http.StatusBadRequest, "invalid_argument", "invalid terminal request"
	case errors.Is(err, session.ErrReplayGap):
		status, code, message = http.StatusConflict, "replay_gap", "requested output is no longer buffered"
	case errors.Is(err, session.ErrSlowConsumer):
		status, code, message = http.StatusConflict, "slow_consumer", "viewer fell behind"
	case errors.Is(err, session.ErrProcessFailed):
		status, code, message = http.StatusUnprocessableEntity, "process_failed", "terminal process failed"
	case errors.Is(err, session.ErrClosed):
		status, code, message = http.StatusServiceUnavailable, "unavailable", "server is shutting down"
	default:
		api.settings.Logger.Error("terminal api", "operation", operation, "error", err)
	}
	writeJSON(w, status, struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{message, code})
}

package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/collab"
	"github.com/0xPiranhaCodes/webpty/internal/config"
	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/recording"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
	"github.com/0xPiranhaCodes/webpty/internal/webassets"
)

// Run listens on cfg.Address and serves until ctx is done.
func Run(ctx context.Context, cfg config.Config) error {
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Printf("webpty listening on %s", listener.Addr())
	return Serve(ctx, cfg, listener)
}

// terminateMargin is added to the kill grace when an admin terminates a
// terminal, so the request waits for the SIGKILL that follows SIGTERM.
const terminateMargin = 5 * time.Second

// Serve opens the store, serves on listener, and shuts down gracefully when
// ctx is done. It takes ownership of listener. Zero fields of cfg take their
// defaults.
func Serve(ctx context.Context, cfg config.Config, listener net.Listener) error {
	cfg = config.WithDefaults(cfg)
	fail := func(err error) error {
		_ = listener.Close()
		return err
	}
	policy, err := preflight(cfg, listener.Addr())
	if err != nil {
		return fail(err)
	}
	unlock, err := store.LockDatabase(cfg.DatabasePath)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = unlock() }()
	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	rt, err := newRuntime(ctx, cfg, db, policy)
	if err != nil {
		return fail(err)
	}
	defer rt.hub.Close()
	return rt.serve(ctx, listener)
}

// preflight enforces the loopback default against the address actually
// bound, and validates the command policy before anything is opened.
func preflight(cfg config.Config, addr net.Addr) (*session.CommandPolicy, error) {
	if cfg.PublicOrigin == "" && !config.IsLoopbackAddress(addr.String()) {
		return nil, fmt.Errorf("listening on %s, which other hosts can reach, requires WEBPTY_PUBLIC_ORIGIN", addr)
	}
	return session.NewCommandPolicy(cfg.CommandAllow, cfg.CommandDeny)
}

// runtime holds the long-lived services Serve wires together.
type runtime struct {
	cfg        config.Config
	db         *store.Store
	logger     *slog.Logger
	auth       *auth.Service
	recordings *recording.Service
	hub        *collab.Hub
	grants     *access.Service
	manager    *session.Manager
}

// newRuntime builds the services in dependency order. On error it closes
// the hub it created; the caller owns db.
func newRuntime(ctx context.Context, cfg config.Config, db *store.Store, policy *session.CommandPolicy) (_ *runtime, err error) {
	rt := &runtime{cfg: cfg, db: db, logger: slog.Default()}
	rt.auth = auth.NewService(auth.ServiceConfig{Store: db, SessionTTL: cfg.SessionTTL, BootstrapTTL: cfg.BootstrapTTL})
	if rt.recordings, err = recording.New(ctx, rt.recordingConfig()); err != nil {
		return nil, err
	}
	rt.hub = collab.NewHub(collab.Config{MaxParticipants: cfg.MaxViewers, Observer: rt.recordings})
	defer func() {
		if err != nil {
			rt.hub.Close()
		}
	}()
	if rt.grants, err = access.NewService(rt.accessConfig()); err != nil {
		return nil, err
	}
	if rt.manager, err = session.NewManager(ctx, rt.sessionConfig(policy)); err != nil {
		return nil, err
	}
	return rt, nil
}

func (rt *runtime) recordingConfig() recording.Config {
	cfg := rt.cfg
	return recording.Config{
		Store:             rt.db,
		Disabled:          !cfg.RecordingEnabled,
		QueueBytes:        cfg.RecordingQueueBytes,
		ChunkBytes:        cfg.RecordingChunkBytes,
		ChunkEvents:       cfg.RecordingChunkEvents,
		FlushInterval:     cfg.RecordingFlushInterval,
		Retention:         cfg.RecordingRetention,
		MaxRecordingBytes: cfg.RecordingMaxBytes,
		ShutdownTimeout:   cfg.RecordingShutdownTimeout,
		StartTimeout:      cfg.RecordingStartTimeout,
		Logger:            rt.logger,
	}
}

func (rt *runtime) accessConfig() access.Config {
	return access.Config{
		Store:           rt.db,
		Listener:        rt.hub,
		SessionTTL:      rt.cfg.AccessSessionTTL,
		DefaultGrantTTL: rt.cfg.GrantDefaultTTL,
		MaxGrantTTL:     rt.cfg.GrantMaxTTL,
		MaxRedemptions:  rt.cfg.GrantMaxRedemptions,
	}
}

func (rt *runtime) sessionConfig(policy *session.CommandPolicy) session.Config {
	cfg := rt.cfg
	return session.Config{
		Store:            rt.db,
		Starter:          pty.Unix{EnvPassthrough: cfg.ChildEnvPassthrough},
		DefaultCommand:   pty.Command{Path: cfg.Command, Args: cfg.CommandArgs},
		CommandPolicy:    policy,
		MaxSessions:      cfg.MaxSessions,
		MaxViewers:       cfg.MaxViewers,
		IdleTimeout:      cfg.TerminalIdle,
		KillGrace:        cfg.TerminalKillGrace,
		ReplayBytes:      cfg.ReplayBytes,
		ClientQueueBytes: cfg.ClientQueueBytes,
		Logger:           rt.logger,
		OnEnd:            rt.hub.EndTerminal,
		Recorders:        rt.recordings,
	}
}

func (rt *runtime) router() http.Handler {
	security := httpapi.SecuritySettings{
		PublicOrigin:  rt.cfg.PublicOrigin,
		SecureCookies: rt.cfg.SecureCookies,
	}
	return httpapi.NewRouter(
		httpapi.WithAdminAuth(rt.auth, security, rt.grants),
		httpapi.WithTerminals(rt.auth, security, rt.manager, rt.terminalSettings()),
		httpapi.WithRecordings(rt.auth, security, rt.recordings),
		httpapi.WithConsole(rt.auth, security, rt.db, runtimeSettings(rt.cfg)),
		httpapi.WithSPA(webassets.Dist()),
	)
}

func (rt *runtime) terminalSettings() httpapi.TerminalSettings {
	return httpapi.TerminalSettings{
		WriteTimeout:     rt.cfg.WSWriteTimeout,
		PingInterval:     rt.cfg.WSPingInterval,
		TerminateTimeout: rt.cfg.TerminalKillGrace + terminateMargin,
		Logger:           rt.logger,
		Access:           rt.grants,
		Presence:         rt.hub,
		Recordings:       rt.recordings,
	}
}

// serve runs the HTTP server and background jobs until ctx is done or the
// server fails, then closes everything in order.
func (rt *runtime) serve(ctx context.Context, listener net.Listener) error {
	drain := httpapi.NewDrain()
	server := NewServer(rt.cfg, drain.Wrap(rt.router()))
	stopLoops := rt.startLoops()
	closeAll := func() error {
		stopLoops()
		return rt.close(drain)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case err := <-serveErr:
		return errors.Join(fmt.Errorf("serve: %w", err), closeAll())
	case <-ctx.Done():
	}
	return shutdown(server, rt.cfg.ShutdownTimeout, closeAll, serveErr)
}

// startLoops starts the periodic jobs; the returned func stops them and
// waits for both to return.
func (rt *runtime) startLoops() (stop func()) {
	loopCtx, cancel := context.WithCancel(context.Background())
	retentionDone := goDone(func() {
		runEvery(loopCtx, retentionInterval, func(ctx context.Context) { runRetention(ctx, rt.recordings, rt.logger) })
	})
	sweepDone := goDone(func() {
		runEvery(loopCtx, rt.cfg.SessionSweepInterval, func(ctx context.Context) { sweepSessions(ctx, rt.db, rt.logger) })
	})
	return func() {
		cancel()
		<-retentionDone
		<-sweepDone
	}
}

// goDone runs fn in a goroutine and returns a channel closed when it returns.
func goDone(fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

// close ends sessions, then handlers, then recordings: recordings finish
// only after their sessions' final lifecycle events, and every handler,
// including hijacked WebSockets, ends before the store closes.
func (rt *runtime) close(drain *httpapi.Drain) error {
	err := closeSessions(rt.manager, rt.cfg)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), rt.cfg.ShutdownTimeout)
	if drainErr := drain.Close(drainCtx); drainErr != nil {
		err = errors.Join(err, fmt.Errorf("close handlers: %w", drainErr))
	}
	cancelDrain()
	if closeErr := rt.recordings.Close(context.Background()); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	return err
}

// shutdown stops the server gracefully, forcing it closed after timeout.
func shutdown(server *http.Server, timeout time.Duration, closeAll func() error, serveErr <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = fmt.Errorf("shutdown: %w", shutdownErr)
		_ = server.Close()
	}
	// Hijacked WebSocket connections are not tracked by Shutdown; closeAll
	// ends them through the drain.
	if err := closeAll(); err != nil {
		return errors.Join(shutdownErr, err)
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// closeSessions gives terminal sessions their own shutdown budget, after
// which the manager SIGKILLs whatever is left.
func closeSessions(manager *session.Manager, cfg config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.TerminalShutdownTimeout)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		return fmt.Errorf("terminate sessions: %w", err)
	}
	return nil
}

// retentionInterval is how often expired recordings are deleted.
const retentionInterval = time.Hour

// runEvery calls fn now and then every interval until ctx is done.
func runEvery(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runRetention deletes expired recordings.
func runRetention(ctx context.Context, recordings *recording.Service, logger *slog.Logger) {
	deleted, err := recordings.RunRetention(ctx)
	switch {
	case err != nil && ctx.Err() == nil:
		logger.Error("recording retention", "error", err)
	case deleted > 0:
		logger.Info("recording retention", "deleted", deleted)
	}
}

type sessionSweeper interface {
	DeleteExpiredSessions(context.Context, time.Time) (int64, error)
}

// sweepSessions deletes expired admin and guest sessions.
func sweepSessions(ctx context.Context, db sessionSweeper, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	deleted, err := db.DeleteExpiredSessions(ctx, time.Now())
	switch {
	case err != nil && ctx.Err() == nil:
		logger.Error("expired session cleanup", "error", err)
	case deleted > 0:
		logger.Info("expired session cleanup", "deleted", deleted)
	}
}

// NewServer returns an http.Server with the configured timeouts.
func NewServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Address,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
}

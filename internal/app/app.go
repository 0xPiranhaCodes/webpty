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

// Serve opens the store, serves on listener, and shuts down gracefully when
// ctx is done. It takes ownership of listener. Zero fields of cfg take their
// defaults.
func Serve(ctx context.Context, cfg config.Config, listener net.Listener) error {
	cfg = config.WithDefaults(cfg)
	if cfg.PublicOrigin == "" && !config.IsLoopbackAddress(listener.Addr().String()) {
		_ = listener.Close()
		return fmt.Errorf("listening on %s, which other hosts can reach, requires WEBPTY_PUBLIC_ORIGIN", listener.Addr())
	}
	policy, err := session.NewCommandPolicy(cfg.CommandAllow, cfg.CommandDeny)
	if err != nil {
		_ = listener.Close()
		return err
	}
	unlock, err := store.LockDatabase(cfg.DatabasePath)
	if err != nil {
		_ = listener.Close()
		return err
	}
	defer func() { _ = unlock() }()
	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		_ = listener.Close()
		return err
	}
	defer db.Close()

	service := auth.NewService(auth.ServiceConfig{
		Store:        db,
		SessionTTL:   cfg.SessionTTL,
		BootstrapTTL: cfg.BootstrapTTL,
	})
	logger := slog.Default()
	recordings, err := recording.New(ctx, recording.Config{
		Store:             db,
		Disabled:          !cfg.RecordingEnabled,
		QueueBytes:        cfg.RecordingQueueBytes,
		ChunkBytes:        cfg.RecordingChunkBytes,
		ChunkEvents:       cfg.RecordingChunkEvents,
		FlushInterval:     cfg.RecordingFlushInterval,
		Retention:         cfg.RecordingRetention,
		MaxRecordingBytes: cfg.RecordingMaxBytes,
		ShutdownTimeout:   cfg.RecordingShutdownTimeout,
		StartTimeout:      cfg.RecordingStartTimeout,
		Logger:            logger,
	})
	if err != nil {
		_ = listener.Close()
		return err
	}
	hub := collab.NewHub(collab.Config{MaxParticipants: cfg.MaxViewers, Observer: recordings})
	defer hub.Close()
	grants, err := access.NewService(access.Config{
		Store:           db,
		Listener:        hub,
		SessionTTL:      cfg.AccessSessionTTL,
		DefaultGrantTTL: cfg.GrantDefaultTTL,
		MaxGrantTTL:     cfg.GrantMaxTTL,
		MaxRedemptions:  cfg.GrantMaxRedemptions,
	})
	if err != nil {
		_ = listener.Close()
		return err
	}
	manager, err := session.NewManager(ctx, session.Config{
		Store:            db,
		Starter:          pty.Unix{EnvPassthrough: cfg.ChildEnvPassthrough},
		DefaultCommand:   pty.Command{Path: cfg.Command, Args: cfg.CommandArgs},
		CommandPolicy:    policy,
		MaxSessions:      cfg.MaxSessions,
		MaxViewers:       cfg.MaxViewers,
		IdleTimeout:      cfg.TerminalIdle,
		KillGrace:        cfg.TerminalKillGrace,
		ReplayBytes:      cfg.ReplayBytes,
		ClientQueueBytes: cfg.ClientQueueBytes,
		Logger:           logger,
		OnEnd:            hub.EndTerminal,
		Recorders:        recordings,
	})
	if err != nil {
		_ = listener.Close()
		return err
	}
	security := httpapi.SecuritySettings{
		PublicOrigin:  cfg.PublicOrigin,
		SecureCookies: cfg.SecureCookies,
	}
	drain := httpapi.NewDrain()
	handler := drain.Wrap(httpapi.NewRouter(
		httpapi.WithAdminAuth(service, security, grants),
		httpapi.WithTerminals(service, security, manager, httpapi.TerminalSettings{
			WriteTimeout:     cfg.WSWriteTimeout,
			PingInterval:     cfg.WSPingInterval,
			TerminateTimeout: cfg.TerminalKillGrace + 5*time.Second,
			Logger:           logger,
			Access:           grants,
			Presence:         hub,
			Recordings:       recordings,
		}),
		httpapi.WithRecordings(service, security, recordings),
		httpapi.WithConsole(service, security, db, runtimeSettings(cfg)),
		httpapi.WithSPA(webassets.Dist()),
	))
	server := NewServer(cfg, handler)

	loopCtx, stopLoops := context.WithCancel(context.Background())
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		runEvery(loopCtx, retentionInterval, func(ctx context.Context) { runRetention(ctx, recordings, logger) })
	}()
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		runEvery(loopCtx, cfg.SessionSweepInterval, func(ctx context.Context) { sweepSessions(ctx, db, logger) })
	}()
	// Recordings finish only after their sessions' final lifecycle events,
	// and every handler, including hijacked WebSockets, ends before the
	// store closes.
	closeAll := func() error {
		stopLoops()
		<-retentionDone
		<-sweepDone
		err := closeSessions(manager, cfg)
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		if drainErr := drain.Close(drainCtx); drainErr != nil {
			err = errors.Join(err, fmt.Errorf("close handlers: %w", drainErr))
		}
		cancelDrain()
		if closeErr := recordings.Close(context.Background()); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return err
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case err := <-serveErr:
		return errors.Join(fmt.Errorf("serve: %w", err), closeAll())
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
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

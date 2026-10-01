package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// Drain tracks in-flight requests, including WebSocket handlers whose
// connections http.Server no longer tracks once hijacked, so shutdown can
// cancel them and wait before closing what they use.
type Drain struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// NewDrain returns an open Drain.
func NewDrain() *Drain {
	ctx, cancel := context.WithCancel(context.Background())
	return &Drain{ctx: ctx, cancel: cancel}
}

// Wrap counts next's requests. Each request's context is also cancelled by
// Close; requests arriving after Close get 503.
func (d *Drain) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			writeError(w, http.StatusServiceUnavailable, "server is shutting down")
			return
		}
		d.wg.Add(1)
		d.mu.Unlock()
		defer d.wg.Done()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(d.ctx, cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Close rejects new requests, cancels in-flight ones, and waits until they
// return or ctx is done. It is idempotent.
func (d *Drain) Close(ctx context.Context) error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.cancel()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("httpapi: handlers still running: %w", ctx.Err())
	}
}

package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
)

func TestDrainCancelsAndWaitsForInFlightHandlers(t *testing.T) {
	drain := httpapi.NewDrain()
	entered := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	handler := drain.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		// Work after cancellation, such as a final audit write, must still
		// finish before Close returns.
		<-release
		finished.Store(true)
	}))
	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- drain.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while a handler was still running", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if !finished.Load() {
		t.Fatal("Close returned before the handler finished")
	}

	late := httptest.NewRecorder()
	handler.ServeHTTP(late, httptest.NewRequest(http.MethodGet, "/", nil))
	if late.Code != http.StatusServiceUnavailable {
		t.Fatalf("request after Close = %d, want 503", late.Code)
	}
}

func TestDrainCloseHonoursDeadline(t *testing.T) {
	drain := httpapi.NewDrain()
	entered := make(chan struct{})
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	handler := drain.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-stuck
	}))
	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := drain.Close(ctx); err == nil {
		t.Fatal("Close with an expired context and a stuck handler returned nil")
	}
}

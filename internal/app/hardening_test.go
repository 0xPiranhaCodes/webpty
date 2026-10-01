package app_test

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/app"
	"github.com/0xPiranhaCodes/webpty/internal/config"
)

func TestServeRefusesNonLoopbackListenerWithoutPublicOrigin(t *testing.T) {
	cfg := testConfig(t)
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	err = app.Serve(context.Background(), cfg, listener)
	if err == nil || !strings.Contains(err.Error(), "WEBPTY_PUBLIC_ORIGIN") {
		t.Fatalf("Serve = %v, want a public origin error", err)
	}
	if _, err := listener.Accept(); err == nil {
		t.Fatal("listener left open")
	}
}

func TestServeWithZeroValueTimeoutsShutsDownGracefully(t *testing.T) {
	cfg := config.Config{DatabasePath: testConfig(t).DatabasePath}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, cfg, listener) }()
	c := &client{t: t, base: "http://" + listener.Addr().String(), http: &http.Client{}}
	eventuallyApp(t, func() bool {
		response, err := c.http.Get(c.base + "/healthz")
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v, want a graceful shutdown with default timeouts", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func eventuallyApp(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

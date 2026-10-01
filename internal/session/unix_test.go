//go:build unix

package session_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func newUnixManager(t *testing.T) *session.Manager {
	t.Helper()
	return newUnixManagerWithGrace(t, 200*time.Millisecond)
}

func newUnixManagerWithGrace(t *testing.T, killGrace time.Duration) *session.Manager {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "webpty.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := session.NewManager(context.Background(), session.Config{
		Store:          s,
		Starter:        pty.Unix{},
		DefaultCommand: pty.Command{Path: "/bin/sh"},
		KillGrace:      killGrace,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
		_ = s.Close()
	})
	return m
}

func readUntilExit(t *testing.T, sub *session.Subscription) (string, session.Exit) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out strings.Builder
	for {
		event, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v (output so far %q)", err, out.String())
		}
		if event.Kind == session.EventExit {
			return out.String(), event.Exit
		}
		out.Write(event.Data)
	}
}

func TestUnixSessionOutputResizeAndExit(t *testing.T) {
	m := newUnixManager(t)
	info, err := m.Create(context.Background(), session.CreateRequest{
		Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "stty size; read line; stty size; exit 4"}},
		Rows:    20, Cols: 70,
	})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := m.Subscribe(context.Background(), info.PublicID, session.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var seen strings.Builder
	for !strings.Contains(seen.String(), "20 70") {
		event, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("waiting for initial size: %v (%q)", err, seen.String())
		}
		seen.Write(event.Data)
	}
	if err := m.Resize(info.PublicID, 33, 111); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(context.Background(), info.PublicID, []byte("\r")); err != nil {
		t.Fatal(err)
	}
	out, exit := readUntilExit(t, sub)
	if !strings.Contains(out, "33 111") {
		t.Fatalf("output %q lacks resized dimensions", out)
	}
	if exit.State != store.TerminalExited || exit.Code == nil || *exit.Code != 4 {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestUnixSessionTerminateReapsProcess(t *testing.T) {
	m := newUnixManager(t)
	info, err := m.Create(context.Background(), session.CreateRequest{
		Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", `trap "" TERM HUP; echo ready; exec sleep 60`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := m.Subscribe(context.Background(), info.PublicID, session.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var seen strings.Builder
	for !strings.Contains(seen.String(), "ready") {
		event, err := sub.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seen.Write(event.Data)
	}

	final, err := m.Terminate(ctx, info.PublicID, "")
	if err != nil {
		t.Fatal(err)
	}
	if final.State != store.TerminalTerminated || final.ExitSignal != "SIGKILL" {
		t.Fatalf("final = %+v, want SIGKILL escalation", final)
	}
	_, exit := readUntilExit(t, sub)
	if exit.State != store.TerminalTerminated {
		t.Fatalf("exit = %+v", exit)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestUnixCloseKillsTermIgnoringProcessGroupWhenContextExpires(t *testing.T) {
	m := newUnixManagerWithGrace(t, time.Hour)
	info, err := m.Create(context.Background(), session.CreateRequest{
		Command: pty.Command{Path: "/bin/sh", Args: []string{"-c",
			`trap "" TERM HUP; sleep 60 & echo "pids $$ $! end"; exec sleep 60`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := m.Subscribe(context.Background(), info.PublicID, session.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var seen strings.Builder
	for !strings.Contains(seen.String(), " end") {
		event, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("waiting for pids: %v (%q)", err, seen.String())
		}
		seen.Write(event.Data)
	}
	var leader, background int
	out := seen.String()
	if _, err := fmt.Sscanf(out[strings.Index(out, "pids "):], "pids %d %d end", &leader, &background); err != nil {
		t.Fatalf("parse pids from %q: %v", out, err)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer closeCancel()
	if err := m.Close(closeCtx); err != nil {
		t.Fatalf("Close = %v, want the forced kill to reap the session", err)
	}
	if alive(leader) {
		t.Fatalf("session leader %d survived Close", leader)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(background) {
		if time.Now().After(deadline) {
			t.Fatalf("background child %d survived Close", background)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stored, err := m.Get(context.Background(), info.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != store.TerminalTerminated || stored.ExitSignal != "SIGKILL" {
		t.Fatalf("stored = %+v, want terminated by SIGKILL", stored)
	}
}

package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/session"
)

func TestCommandPolicyDeniesBeforeStarting(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-tool")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	policy, err := session.NewCommandPolicy([]string{"/bin/default-shell", "/bin/allowed"}, []string{real})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *session.Config) { c.CommandPolicy = policy })

	for _, path := range []string{"/bin/not-listed", real, link} {
		_, err := h.m.Create(context.Background(), session.CreateRequest{Command: pty.Command{Path: path}, RemoteAddr: "192.0.2.4"})
		if !errors.Is(err, session.ErrCommandDenied) {
			t.Fatalf("Create(%s) = %v, want ErrCommandDenied", path, err)
		}
	}
	if p := h.starter.Next(time.Millisecond); p != nil {
		t.Fatal("a denied command was started")
	}
	event := h.auditEvent("terminal.session.denied")
	if event.Details["command"] != "/bin/not-listed" || event.RemoteAddr != "192.0.2.4" {
		t.Fatalf("audit = %+v", event)
	}
	h.create(session.CreateRequest{Command: pty.Command{Path: "/bin/allowed"}})
	h.create(session.CreateRequest{})
}

// CommandPolicy is optional: without one, sessions run any command.
func TestManagerWithoutACommandPolicyAllowsEveryCommand(t *testing.T) {
	h := newHarness(t, func(c *session.Config) { c.CommandPolicy = nil })
	if _, p := h.create(session.CreateRequest{}); p.Command.Path != "/bin/default-shell" {
		t.Fatalf("default session runs %q", p.Command.Path)
	}
	if _, p := h.create(session.CreateRequest{Command: pty.Command{Path: "/usr/bin/env", Args: []string{"true"}}}); p.Command.Path != "/usr/bin/env" {
		t.Fatalf("explicit session runs %q", p.Command.Path)
	}
	for _, p := range []*session.CommandPolicy{nil, session.AllowAllCommands(), {}} {
		if !p.Permits("/bin/sh") {
			t.Errorf("policy %+v denies /bin/sh", p)
		}
	}
}

func TestNewCommandPolicyRejectsRelativeEntries(t *testing.T) {
	if _, err := session.NewCommandPolicy([]string{"sh"}, nil); err == nil {
		t.Fatal("relative allow entry accepted")
	}
	if _, err := session.NewCommandPolicy(nil, []string{"./sudo"}); err == nil {
		t.Fatal("relative deny entry accepted")
	}
}

package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(terminalWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *terminalHarness) state(id string) store.TerminalState {
	h.t.Helper()
	info, err := h.manager.Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return info.State
}

func TestWebSocketStaysResponsiveWhileInputBlocked(t *testing.T) {
	h := newTerminalAPI(t, func(c *session.Config) {
		c.MaxViewers = 1
		c.IdleTimeout = 300 * time.Millisecond
	}, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	release := p.BlockWrites()
	defer release()
	server := h.server()

	client := h.dial(server, id, "")
	client.readType("ready")
	client.send(`{"type":"input","data":"never read"}`)
	eventually(t, "blocked PTY write", func() bool { return p.BlockedWriters() == 1 })

	client.send(`{"type":"ping"}`)
	client.readType("pong")
	client.send(`{"type":"resize","rows":33,"cols":99}`)
	client.send(`{"type":"ping"}`)
	client.readType("pong")
	if sizes := p.Sizes(); sizes[len(sizes)-1] != (pty.Size{Rows: 33, Cols: 99}) {
		t.Fatalf("resize not applied while input blocked: %v", sizes)
	}
	p.Emit("output flows")
	if got := decodeOutput(t, client.readType("output")); got != "output flows" {
		t.Fatalf("output = %q", got)
	}

	// Disconnecting releases the viewer slot even though the write is stuck.
	client.conn.CloseNow()
	var second *websocket.Conn
	eventually(t, "viewer slot release", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
		defer cancel()
		conn, _, err := websocket.Dial(ctx, wsURL(server, id, ""), dialOptions(server, h.cookie, server.URL))
		if err != nil {
			return false
		}
		second = conn
		return true
	})
	second.CloseNow()

	// With every viewer gone the idle timeout still terminates the session,
	// which closes the PTY and releases the blocked write.
	eventually(t, "idle termination", func() bool { return h.state(id) == store.TerminalTerminated })
	eventually(t, "blocked write release", func() bool { return p.BlockedWriters() == 0 })
}

func TestWebSocketBlockedInputReleasedOnTermination(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	release := p.BlockWrites()
	defer release()
	server := h.server()

	client := h.dial(server, id, "")
	client.readType("ready")
	client.send(`{"type":"input","data":"stuck"}`)
	eventually(t, "blocked PTY write", func() bool { return p.BlockedWriters() == 1 })

	if response := h.admin(http.MethodDelete, "/api/v1/admin/terminals/"+id, ""); response.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", response.Code, response.Body)
	}
	if exit := client.readType("exit"); exit["state"] != "terminated" {
		t.Fatalf("exit = %v", exit)
	}
	if code, _ := client.closeStatus(); code != websocket.StatusNormalClosure {
		t.Fatalf("close code = %v", code)
	}
	eventually(t, "blocked write release", func() bool { return p.BlockedWriters() == 0 })
}

func TestWebSocketInputBacklogClosesOnlyThatClient(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{InputQueueBytes: 16})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	release := p.BlockWrites()
	defer release()
	server := h.server()

	flooder := h.dial(server, id, "")
	flooder.readType("ready")
	bystander := h.dial(server, id, "")
	bystander.readType("ready")

	for i := 0; i < 4; i++ {
		flooder.send(`{"type":"input","data":"0123456789"}`)
	}
	if msg := flooder.readType("error"); msg["code"] != "input_overflow" {
		t.Fatalf("error = %v", msg)
	}
	if code, _ := flooder.closeStatus(); code != httpapi.CloseInputOverflow {
		t.Fatalf("close code = %v, want %v", code, httpapi.CloseInputOverflow)
	}

	p.Emit("still flowing")
	if got := decodeOutput(t, bystander.readType("output")); got != "still flowing" {
		t.Fatalf("bystander output = %q", got)
	}
	bystander.send(`{"type":"ping"}`)
	bystander.readType("pong")
	if state := h.state(id); state != store.TerminalRunning {
		t.Fatalf("session state = %q after one client overflowed", state)
	}
}

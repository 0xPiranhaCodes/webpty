package httpapi_test

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
)

// rawFrame is one server-to-client WebSocket frame; server frames are never masked.
type rawFrame struct {
	opcode  byte
	payload []byte
}

func readRawFrame(r *bufio.Reader) (rawFrame, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return rawFrame{}, err
	}
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return rawFrame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return rawFrame{}, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return rawFrame{}, err
	}
	return rawFrame{opcode: head[0] & 0x0f, payload: payload}, nil
}

func writeMaskedClose(conn net.Conn, payload []byte) error {
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x88, 0x80 | byte(len(payload))}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := conn.Write(frame)
	return err
}

// Browsers fail the connection (reporting 1006 instead of the application
// code) when a second close frame follows the first, so a revoked guest
// would never learn why it was disconnected.
func TestRevocationSendsExactlyOneCloseFrame(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{PingInterval: time.Hour})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	cookie, grantID := h.guest(id, "viewer")

	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(terminalWait))
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/terminals/"+id+"/ws", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Protocol", httpapi.TerminalProtocol)
	request.Header.Set("Origin", server.URL)
	request.AddCookie(cookie)
	if err := request.Write(conn); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake = %v %v", response, err)
	}

	// Wait for the server to be serving the stream before revoking.
	if frame, err := readRawFrame(reader); err != nil || !strings.Contains(string(frame.payload), `"ready"`) {
		t.Fatalf("first frame = %q %v", frame.payload, err)
	}
	if response := h.admin(http.MethodDelete, "/api/v1/admin/terminals/"+id+"/grants/"+grantID, ""); response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}

	var closeFrames []rawFrame
	for {
		frame, err := readRawFrame(reader)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if frame.opcode != 0x8 {
			continue
		}
		closeFrames = append(closeFrames, frame)
		if len(closeFrames) == 1 {
			// Echo the close frame as a browser does.
			if err := writeMaskedClose(conn, frame.payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(closeFrames) != 1 {
		t.Fatalf("server sent %d close frames, want 1", len(closeFrames))
	}
	code := binary.BigEndian.Uint16(closeFrames[0].payload[:2])
	if code != uint16(httpapi.CloseUnauthorized) || string(closeFrames[0].payload[2:]) != "access revoked" {
		t.Fatalf("close = %d %q", code, closeFrames[0].payload[2:])
	}
}

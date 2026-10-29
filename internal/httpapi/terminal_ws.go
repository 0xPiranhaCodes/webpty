package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/0xPiranhaCodes/webpty/internal/collab"
	"github.com/0xPiranhaCodes/webpty/internal/recording"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// TerminalProtocol is the WebSocket subprotocol clients must offer.
const TerminalProtocol = "webpty.terminal.v1"

const protocolVersion = 1

// auditTimeout bounds an audit write that runs after the request may be gone.
const auditTimeout = 5 * time.Second

// Application close codes.
const (
	CloseSlowConsumer  websocket.StatusCode = 4000
	CloseReplayGap     websocket.StatusCode = 4001
	CloseUnauthorized  websocket.StatusCode = 4003
	CloseInputOverflow websocket.StatusCode = 4004
)

// Client messages: {"type":"input","data":"..."},
// {"type":"resize","rows":R,"cols":C}, {"type":"ping"}.
type clientMessage struct {
	Type string  `json:"type"`
	Data *string `json:"data"`
	Rows *int    `json:"rows"`
	Cols *int    `json:"cols"`
}

type readyMessage struct {
	Type        string          `json:"type"`
	Version     int             `json:"version"`
	Session     any             `json:"session"`
	Seq         uint64          `json:"seq"`
	Role        collab.Role     `json:"role"`
	Permissions permissionsJSON `json:"permissions"`
}

type outputMessage struct {
	Type string `json:"type"`
	Seq  uint64 `json:"seq"`
	Data string `json:"data"`
}

type resizeMessage struct {
	Type string `json:"type"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

type exitMessage struct {
	Type     string `json:"type"`
	State    string `json:"state"`
	ExitCode *int   `json:"exitCode"`
	Signal   string `json:"signal,omitempty"`
}

type errorMessage struct {
	Type     string  `json:"type"`
	Code     string  `json:"code"`
	Message  string  `json:"message"`
	Action   string  `json:"action,omitempty"`
	FirstSeq *uint64 `json:"firstSeq,omitempty"`
	LastSeq  *uint64 `json:"lastSeq,omitempty"`
}

type pongMessage struct {
	Type string `json:"type"`
}

// websocket upgrades an authorized viewer to the terminal stream. The
// steps run in a fixed order: subscribe, reserve a participant slot,
// re-check guest access, accept the connection, then activate. Deferred
// cleanups unwind in the reverse order.
func (api *terminalAPI) websocket(w http.ResponseWriter, r *http.Request) {
	opts, ok := resumeOptions(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	sub, err := api.manager.Subscribe(r.Context(), id, opts)
	var gap *session.ReplayGapError
	if err != nil && !errors.As(err, &gap) {
		api.writeError(w, "subscribe terminal", err)
		return
	}
	if sub != nil {
		defer sub.Close()
	}
	v := viewerFrom(r.Context())
	participant, ok := api.reserveParticipant(w, r, id, v)
	if !ok {
		return
	}
	defer participant.Leave()
	conn, ok := api.accept(w, r, participant)
	if !ok {
		return
	}
	defer conn.CloseNow()
	api.runStream(r, conn, id, v, participant, sub, gap)
}

// resumeOptions reads the optional afterSeq resume point.
func resumeOptions(w http.ResponseWriter, r *http.Request) (session.SubscribeOptions, bool) {
	if !offersProtocol(r.Header, TerminalProtocol) {
		writeError(w, http.StatusBadRequest, "websocket subprotocol "+TerminalProtocol+" required")
		return session.SubscribeOptions{}, false
	}
	raw := r.URL.Query().Get("afterSeq")
	if raw == "" {
		return session.SubscribeOptions{}, true
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "afterSeq must be a non-negative integer")
		return session.SubscribeOptions{}, false
	}
	return session.SubscribeOptions{Resume: true, AfterSeq: seq}, true
}

// reserveParticipant holds a slot so the viewer limit and revocations
// apply; the participant is announced only once the connection is up.
func (api *terminalAPI) reserveParticipant(w http.ResponseWriter, r *http.Request, id string, v viewer) (*collab.Participant, bool) {
	participant, err := api.presence.Reserve(collab.JoinRequest{
		TerminalID: id, Role: v.role(), GrantID: v.guest.Grant.PublicID, SessionID: v.guest.ID, ExpiresAt: v.guest.ExpiresAt,
	})
	switch {
	case errors.Is(err, collab.ErrFull):
		api.writeError(w, "join terminal", session.ErrViewerLimit)
		return nil, false
	case err != nil:
		api.writeError(w, "join terminal", session.ErrClosed)
		return nil, false
	}
	// A revocation committed before the reservation was not delivered to
	// this participant, so a guest is checked again now that it is registered.
	if !v.owner {
		if _, err := api.access.Authenticate(r.Context(), v.token); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			participant.Leave()
			return nil, false
		}
	}
	return participant, true
}

// accept upgrades the connection and activates the participant. On false
// the connection, if any, is already closed.
func (api *terminalAPI) accept(w http.ResponseWriter, r *http.Request, participant *collab.Participant) (*websocket.Conn, bool) {
	// Hijacking clears the server's request deadlines; each message write
	// carries its own deadline instead.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{TerminalProtocol},
		// Origin was enforced by sameOrigin, which also honours PublicOrigin.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return nil, false
	}
	conn.SetReadLimit(api.settings.MaxMessageBytes)
	var revoked *collab.RevokedError
	switch err := participant.Activate(); {
	case errors.As(err, &revoked):
		_ = conn.Close(CloseUnauthorized, "access "+revoked.Reason)
	case err != nil:
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
	default:
		return conn, true
	}
	conn.CloseNow()
	return nil, false
}

// runStream serves an accepted connection, auditing the join and leave.
// A resume point that is no longer buffered is reported and closed.
func (api *terminalAPI) runStream(r *http.Request, conn *websocket.Conn, id string, v viewer,
	participant *collab.Participant, sub *session.Subscription, gap *session.ReplayGapError) {
	stream := &terminalStream{api: api, conn: conn, id: id, viewer: v, participant: participant,
		remote: remoteIP(r), input: newInputQueue(api.settings.InputQueueBytes), denied: map[collab.Action]bool{}}
	stream.auditParticipant("access.participant.joined", nil)
	defer func() {
		stream.auditParticipant("access.participant.left", map[string]string{"reason": stream.leaveReason()})
	}()
	if gap != nil {
		first, last := gap.FirstSeq, gap.LastSeq
		_ = stream.write(r.Context(), errorMessage{Type: "error", Code: "replay_gap",
			Message: "requested output is no longer buffered", FirstSeq: &first, LastSeq: &last})
		_ = conn.Close(CloseReplayGap, "replay gap")
		return
	}
	stream.serve(r.Context(), sub)
}

func offersProtocol(header http.Header, protocol string) bool {
	for _, value := range header.Values("Sec-WebSocket-Protocol") {
		for _, offered := range strings.Split(value, ",") {
			if strings.TrimSpace(offered) == protocol {
				return true
			}
		}
	}
	return false
}

// closeStatusFor maps a subscription's terminal error to a close frame.
func closeStatusFor(err error) (websocket.StatusCode, string) {
	switch {
	case errors.Is(err, session.ErrSlowConsumer):
		return CloseSlowConsumer, "slow consumer"
	case errors.Is(err, io.EOF):
		return websocket.StatusNormalClosure, "session ended"
	default:
		return websocket.StatusGoingAway, "stream closed"
	}
}

type terminalStream struct {
	api         *terminalAPI
	conn        *websocket.Conn
	id          string
	viewer      viewer
	participant *collab.Participant
	remote      string

	input *inputQueue

	// denied records actions already audited as denied; only readLoop uses it.
	denied map[collab.Action]bool

	once   sync.Once
	code   websocket.StatusCode
	reason string
	stop   context.CancelFunc
}

// allow reports whether the participant may take action now, answering a
// denied action with a permission_denied error.
func (s *terminalStream) allow(ctx context.Context, action collab.Action) bool {
	if s.participant.Allowed(action) {
		return true
	}
	_ = s.write(ctx, errorMessage{Type: "error", Code: "permission_denied", Action: string(action),
		Message: "your access does not permit " + string(action)})
	if !s.denied[action] {
		s.denied[action] = true
		s.auditParticipant("terminal.input.denied", map[string]string{"action": string(action)})
	}
	return false
}

// auditParticipant records a participant event. Details never include
// credentials or terminal content.
func (s *terminalStream) auditParticipant(eventType string, extra map[string]string) {
	if s.api.access == nil {
		return
	}
	details := map[string]string{
		"terminalId": s.id, "participantId": s.participant.ID(), "role": string(s.participant.Role()),
	}
	if s.viewer.guest.Grant.PublicID != "" {
		details["grantId"] = s.viewer.guest.Grant.PublicID
	}
	for k, v := range extra {
		details[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()
	if err := s.api.access.Audit(ctx, eventType, s.remote, details); err != nil {
		s.api.settings.Logger.Error("append audit event", "type", eventType, "error", err)
	}
}

func (s *terminalStream) leaveReason() string {
	if s.reason != "" {
		return s.reason
	}
	return collab.ReasonDisconnected
}

// presenceLoop forwards presence events, and ends the stream once the
// participant loses access, after its final permission_changed is sent.
func (s *terminalStream) presenceLoop(ctx context.Context) {
	for {
		event, err := s.participant.Next(ctx)
		var revoked *collab.RevokedError
		switch {
		case ctx.Err() != nil:
			return
		case errors.As(err, &revoked):
			s.end(CloseUnauthorized, "access "+revoked.Reason)
			return
		case errors.Is(err, collab.ErrSlowConsumer):
			s.end(CloseSlowConsumer, "slow consumer")
			return
		case err != nil:
			s.end(websocket.StatusGoingAway, "server shutting down")
			return
		}
		// Cancelling an in-flight write tears down the connection, which would
		// lose the close frame of whichever goroutine ended the stream.
		if err := s.write(context.WithoutCancel(ctx), event); err != nil {
			s.end(0, "")
			return
		}
	}
}

// inputQueue holds client input until the connection's writer hands it to
// the session, so the reader never waits on the PTY.
type inputQueue struct {
	mu     sync.Mutex
	chunks [][]byte
	bytes  int
	limit  int
	ready  chan struct{}
}

func newInputQueue(limit int) *inputQueue {
	return &inputQueue{limit: limit, ready: make(chan struct{}, 1)}
}

// push reports false if data would exceed the byte limit.
func (q *inputQueue) push(data []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	// A single message larger than the budget still fits an empty queue,
	// so every message the read limit admits can be delivered.
	if len(q.chunks) > 0 && q.bytes+len(data) > q.limit {
		return false
	}
	q.chunks = append(q.chunks, data)
	q.bytes += len(data)
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return true
}

// pop waits for the oldest chunk until ctx is done.
func (q *inputQueue) pop(ctx context.Context) ([]byte, bool) {
	for {
		q.mu.Lock()
		if len(q.chunks) > 0 {
			data := q.chunks[0]
			q.chunks[0] = nil
			q.chunks = q.chunks[1:]
			q.bytes -= len(data)
			q.mu.Unlock()
			return data, true
		}
		q.mu.Unlock()
		select {
		case <-q.ready:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// writeLoop forwards queued input to the session until the stream ends.
func (s *terminalStream) writeLoop(ctx context.Context) {
	for {
		data, ok := s.input.pop(ctx)
		if !ok {
			return
		}
		// Input queued before a revocation is dropped, not delivered; the
		// session checks again just before the bytes reach the PTY.
		allowed := func() bool { return s.participant.Allowed(collab.ActionInput) }
		if !allowed() {
			continue
		}
		err := s.api.manager.WriteAuthorized(ctx, s.id, data, allowed)
		if err != nil && !errors.Is(err, session.ErrInvalidState) && !errors.Is(err, session.ErrInputRevoked) && ctx.Err() == nil {
			s.api.settings.Logger.Warn("terminal input rejected", "sessionId", s.id, "error", err)
		}
	}
}

// end records why the stream stops. Code 0 means the connection is already
// unusable and no close frame is sent. Only the first call counts.
func (s *terminalStream) end(code websocket.StatusCode, reason string) {
	s.once.Do(func() {
		s.code, s.reason = code, reason
		s.stop()
	})
}

func (s *terminalStream) write(ctx context.Context, message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.api.settings.WriteTimeout)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, data)
}

// serve runs the stream's goroutines and forwards session events until
// the stream ends, then sends the chosen close frame and waits for them.
func (s *terminalStream) serve(ctx context.Context, sub *session.Subscription) {
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	s.stop = stop
	// Writes are bounded by the write timeout rather than by streamCtx: the
	// library closes the connection outright when an in-flight write's
	// context is cancelled, which would lose the close frame that end() chose.
	writeCtx := context.WithoutCancel(streamCtx)

	readerDone := goDone(func() { s.readLoop(ctx) })
	writerDone := goDone(func() { s.writeLoop(streamCtx) })
	go s.heartbeat(streamCtx)
	s.sendReady(writeCtx, sub)
	presenceDone := goDone(func() { s.presenceLoop(streamCtx) })
	statusDone := goDone(func() { s.statusLoop(streamCtx) })
	for streamCtx.Err() == nil {
		event, err := sub.Next(streamCtx)
		if err != nil {
			if streamCtx.Err() == nil {
				s.end(closeStatusFor(err))
			}
			continue
		}
		s.forward(writeCtx, event)
	}
	s.closeConn()
	for _, done := range []<-chan struct{}{readerDone, writerDone, presenceDone, statusDone} {
		<-done
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

// sendReady sends the session snapshot; guests get the shared view, which
// omits the command and its arguments.
func (s *terminalStream) sendReady(ctx context.Context, sub *session.Subscription) {
	var info any = toSharedTerminalJSON(sub.Info())
	if s.viewer.owner {
		info = toTerminalJSON(sub.Info())
	}
	role := s.participant.Role()
	if err := s.write(ctx, readyMessage{Type: "ready", Version: protocolVersion,
		Session: info, Seq: sub.LastSeq(), Role: role, Permissions: permissionsFor(role)}); err != nil {
		s.end(0, "")
	}
}

// forward writes one session event; exit also ends the stream normally.
func (s *terminalStream) forward(ctx context.Context, event session.Event) {
	var message any
	switch event.Kind {
	case session.EventOutput:
		message = outputMessage{Type: "output", Seq: event.Seq, Data: base64.StdEncoding.EncodeToString(event.Data)}
	case session.EventResize:
		message = resizeMessage{Type: "resize", Rows: event.Rows, Cols: event.Cols}
	case session.EventExit:
		message = exitMessage{Type: "exit", State: string(event.Exit.State), ExitCode: event.Exit.Code, Signal: event.Exit.Signal}
	default:
		return
	}
	if err := s.write(ctx, message); err != nil {
		s.end(0, "")
		return
	}
	if event.Kind == session.EventExit {
		s.end(closeStatusFor(io.EOF))
	}
}

// closeConn sends the close frame end() chose, or drops the connection
// when it is already unusable.
func (s *terminalStream) closeConn() {
	// Synchronises with whichever goroutine ended the stream.
	s.end(0, "")
	if s.code != 0 {
		_ = s.conn.Close(s.code, s.reason)
	} else {
		_ = s.conn.CloseNow()
	}
}

type recordingStatusMessage struct {
	Type        string `json:"type"`
	RecordingID string `json:"recordingId"`
	Status      string `json:"status"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

var recordingStatusMessages = map[string]string{
	recording.CodeQueueOverflow:   "recording stopped: output arrived faster than it could be stored",
	recording.CodeStorage:         "recording stopped: it could not be stored",
	recording.CodeEncoding:        "recording stopped: output could not be encoded",
	recording.CodeSizeLimit:       "recording stopped: the recording size limit was reached",
	recording.CodeShutdownTimeout: "recording stopped: the server shut down before it was stored",
}

// statusLoop warns the owner when the terminal's recording stops early.
// Guests are never told about recordings.
func (s *terminalStream) statusLoop(ctx context.Context) {
	if !s.viewer.owner || s.api.settings.Recordings == nil {
		return
	}
	statuses, cancel := s.api.settings.Recordings.Watch(s.id)
	defer cancel()
	for {
		var st recording.Status
		select {
		case <-ctx.Done():
			return
		case st = <-statuses:
		}
		message, ok := recordingStatusMessages[st.Code]
		if !ok {
			message = "recording stopped early"
		}
		if err := s.write(context.WithoutCancel(ctx), recordingStatusMessage{Type: "recording_status",
			RecordingID: st.RecordingID, Status: string(st.State), Code: st.Code, Message: message}); err != nil {
			s.end(0, "")
			return
		}
	}
}

// readLoop handles client messages until the connection fails or a protocol
// violation ends the stream.
func (s *terminalStream) readLoop(ctx context.Context) {
	for {
		message, ok := s.readMessage(ctx)
		if !ok {
			return
		}
		if !s.allow(ctx, collab.Action(message.Type)) {
			continue
		}
		switch message.Type {
		case "input":
			if !s.queueInput(ctx, *message.Data) {
				return
			}
		case "resize":
			s.resize(ctx, *message.Rows, *message.Cols)
		case "ping":
			_ = s.write(ctx, pongMessage{Type: "pong"})
		}
	}
}

// readMessage reads and decodes one client message, ending the stream on
// a read error or a protocol violation.
func (s *terminalStream) readMessage(ctx context.Context) (clientMessage, bool) {
	typ, data, err := s.conn.Read(ctx)
	switch {
	case err != nil:
		s.end(0, "")
	case typ != websocket.MessageText:
		s.end(websocket.StatusUnsupportedData, "binary messages are not supported")
	case !utf8.Valid(data):
		s.end(websocket.StatusInvalidFramePayloadData, "invalid UTF-8")
	default:
		message, ok := decodeClientMessage(data)
		if ok {
			return message, true
		}
		_ = s.write(ctx, errorMessage{Type: "error", Code: "invalid_message", Message: "malformed or unsupported message"})
		s.end(websocket.StatusPolicyViolation, "invalid message")
	}
	return clientMessage{}, false
}

// queueInput reports false once the input backlog has ended the stream.
// The input itself is never logged.
func (s *terminalStream) queueInput(ctx context.Context, data string) bool {
	if data == "" || s.input.push([]byte(data)) {
		return true
	}
	s.api.settings.Logger.Warn("terminal viewer disconnected: input backlog",
		"sessionId", s.id, "queueLimitBytes", s.input.limit)
	_ = s.write(ctx, errorMessage{Type: "error", Code: "input_overflow",
		Message: "terminal is not accepting input fast enough"})
	s.end(CloseInputOverflow, "input backlog")
	return false
}

func (s *terminalStream) resize(ctx context.Context, rows, cols int) {
	err := s.api.manager.Resize(s.id, rows, cols)
	if errors.Is(err, session.ErrInvalidArgument) {
		_ = s.write(ctx, errorMessage{Type: "error", Code: "invalid_resize",
			Message: "rows and cols must be between 1 and " + strconv.Itoa(session.MaxRows)})
	} else if err != nil && !errors.Is(err, session.ErrInvalidState) {
		s.api.settings.Logger.Warn("terminal resize rejected", "sessionId", s.id, "error", err)
	}
}

// clientFields lists the members each client message type must carry; no
// other member is allowed.
var clientFields = map[string][]string{
	"input":  {"type", "data"},
	"resize": {"type", "rows", "cols"},
	"ping":   {"type"},
}

// decodeClientMessage accepts exactly one JSON object whose members are
// exactly those of its type, each present once, with exact (case-sensitive)
// names and non-null values of the right JSON type.
func decodeClientMessage(data []byte) (clientMessage, bool) {
	raw, ok := decodeObject(data)
	if !ok {
		return clientMessage{}, false
	}
	var message clientMessage
	if !decodeStrict(raw["type"], &message.Type) {
		return clientMessage{}, false
	}
	fields, known := clientFields[message.Type]
	if !known || len(raw) != len(fields) {
		return clientMessage{}, false
	}
	for _, field := range fields {
		value, present := raw[field]
		if !present || !message.setField(field, value) {
			return clientMessage{}, false
		}
	}
	return message, true
}

// decodeObject reads exactly one JSON object with no duplicate members and
// nothing after it.
func decodeObject(data []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	raw := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		key, isKey := token.(string)
		if err != nil || !isKey {
			return nil, false
		}
		var value json.RawMessage
		if _, dup := raw[key]; dup || decoder.Decode(&value) != nil {
			return nil, false
		}
		raw[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return raw, true
}

// setField decodes one member into m; "type" was decoded already.
func (m *clientMessage) setField(field string, value json.RawMessage) (ok bool) {
	switch field {
	case "type":
		return true
	case "data":
		m.Data = new(string)
		return decodeStrict(value, m.Data)
	case "rows":
		m.Rows, ok = decodeInt(value)
	case "cols":
		m.Cols, ok = decodeInt(value)
	}
	return ok
}

// decodeStrict decodes a JSON string into target, rejecting null and other
// types.
func decodeStrict(value json.RawMessage, target *string) bool {
	if len(value) == 0 || value[0] != '"' {
		return false
	}
	return json.Unmarshal(value, target) == nil
}

// decodeInt decodes a JSON integer, rejecting null, fractions, and strings.
func decodeInt(value json.RawMessage) (*int, bool) {
	if len(value) == 0 || (value[0] != '-' && (value[0] < '0' || value[0] > '9')) {
		return nil, false
	}
	var n int
	if err := json.Unmarshal(value, &n); err != nil {
		return nil, false
	}
	return &n, true
}

// heartbeat probes the peer and ends the stream once the admin or access
// session that opened it is revoked or expires.
func (s *terminalStream) heartbeat(ctx context.Context) {
	ticks, stop := s.api.settings.Heartbeat(s.api.settings.PingInterval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if !s.authorized(ctx) {
			s.end(CloseUnauthorized, "session expired")
			return
		}
		// A ping whose context is cancelled mid-flight tears down the
		// connection, losing the close frame of whoever ended the stream;
		// the write timeout bounds it instead.
		pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.api.settings.WriteTimeout)
		err := s.conn.Ping(pingCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.end(0, "")
			return
		}
	}
}

func (s *terminalStream) authorized(ctx context.Context) bool {
	if !s.viewer.owner {
		current, err := s.api.access.Authenticate(ctx, s.viewer.token)
		return err == nil && current.Grant.TerminalID == s.id
	}
	service := s.api.admin.service
	current, err := service.Authenticate(ctx, s.viewer.token)
	return err == nil && current.Kind == store.SessionKindAdmin && current.ExpiresAt.After(service.Now())
}

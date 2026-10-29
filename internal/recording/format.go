package recording

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// Stored format. Each chunk is a gzip stream of newline-terminated JSON
// records {"v":1,"seq":N,"ms":N,"kind":K,"data":{...}}; its checksum is the
// SHA-256 of the compressed bytes.
const (
	FormatVersion = 1
	Codec         = "gzip"

	// MaxOutputEventBytes bounds the raw output in one event; longer PTY
	// reads are split.
	MaxOutputEventBytes = 8 << 10
	// MaxChunkBytes bounds a chunk's uncompressed size, and so what playback
	// will ever decompress.
	MaxChunkBytes = 1 << 20
	// maxDecodedChunk leaves room for one oversized event past ChunkBytes.
	maxDecodedChunk = MaxChunkBytes + 4*MaxOutputEventBytes
)

// Kind is an event type.
type Kind string

const (
	KindOutput    Kind = "output"
	KindResize    Kind = "resize"
	KindLifecycle Kind = "lifecycle"
	KindPresence  Kind = "presence"
)

// Event is one recorded event. Exactly one of the kind-specific fields is
// set: Output, Rows and Cols, Lifecycle, or Presence.
type Event struct {
	Seq        int64
	OffsetMS   int64
	Kind       Kind
	Output     []byte
	Rows, Cols int
	Lifecycle  *Lifecycle
	Presence   *Presence
}

// Lifecycle is a terminal state change. It never carries addresses,
// commands, or error text.
type Lifecycle struct {
	State    string `json:"state"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Presence is a participant joining, leaving, or losing permissions. It
// carries only the participant's public ID and role.
type Presence struct {
	Event         string `json:"event"`
	ParticipantID string `json:"participantId"`
	Role          string `json:"role"`
	Reason        string `json:"reason,omitempty"`
}

type outputData struct {
	Data []byte `json:"data"`
}

type resizeData struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// MarshalJSON renders the event as {"seq","offsetMs","kind","data"}, with
// output as base64.
func (e Event) MarshalJSON() ([]byte, error) {
	data, err := e.data()
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Seq      int64 `json:"seq"`
		OffsetMS int64 `json:"offsetMs"`
		Kind     Kind  `json:"kind"`
		Data     any   `json:"data"`
	}{e.Seq, e.OffsetMS, e.Kind, data})
}

func (e Event) data() (any, error) {
	switch e.Kind {
	case KindOutput:
		return outputData{Data: e.Output}, nil
	case KindResize:
		return resizeData{Rows: e.Rows, Cols: e.Cols}, nil
	case KindLifecycle:
		if e.Lifecycle != nil {
			return e.Lifecycle, nil
		}
	case KindPresence:
		if e.Presence != nil {
			return e.Presence, nil
		}
	}
	return nil, fmt.Errorf("recording: malformed %q event", e.Kind)
}

type record struct {
	V    int             `json:"v"`
	Seq  int64           `json:"seq"`
	MS   int64           `json:"ms"`
	Kind Kind            `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func encodeEvent(e Event) ([]byte, error) {
	data, err := e.data()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	line, err := json.Marshal(record{V: FormatVersion, Seq: e.Seq, MS: e.OffsetMS, Kind: e.Kind, Data: raw})
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

var (
	lifecycleStates = map[string]bool{
		string(store.TerminalRunning): true, string(store.TerminalExited): true,
		string(store.TerminalFailed): true, string(store.TerminalTerminated): true,
	}
	presenceEvents = map[string]bool{"joined": true, "left": true, "permission_changed": true}
	presenceRoles  = map[string]bool{"owner": true, "editor": true, "viewer": true}
)

const maxLabel = 64

func decodeLine(line []byte) (Event, error) {
	var r record
	if err := strictDecode(line, &r); err != nil {
		return Event{}, corrupt("decode")
	}
	if r.V != FormatVersion {
		return Event{}, corrupt("version")
	}
	e := Event{Seq: r.Seq, OffsetMS: r.MS, Kind: r.Kind}
	if !e.decodePayload(r.Data) {
		return Event{}, corrupt("decode")
	}
	return e, nil
}

// decodePayload fills the kind-specific fields from raw and reports whether
// they are valid; an unknown kind is invalid.
func (e *Event) decodePayload(raw []byte) bool {
	switch e.Kind {
	case KindOutput:
		return e.decodeOutput(raw)
	case KindResize:
		return e.decodeResize(raw)
	case KindLifecycle:
		return e.decodeLifecycle(raw)
	case KindPresence:
		return e.decodePresence(raw)
	}
	return false
}

func (e *Event) decodeOutput(raw []byte) bool {
	var d outputData
	ok := strictDecode(raw, &d) == nil && len(d.Data) > 0 && len(d.Data) <= MaxOutputEventBytes
	e.Output = d.Data
	return ok
}

func (e *Event) decodeResize(raw []byte) bool {
	var d resizeData
	ok := strictDecode(raw, &d) == nil && inRange(d.Rows, session.MaxRows) && inRange(d.Cols, session.MaxCols)
	e.Rows, e.Cols = d.Rows, d.Cols
	return ok
}

func (e *Event) decodeLifecycle(raw []byte) bool {
	var d Lifecycle
	ok := strictDecode(raw, &d) == nil && lifecycleStates[d.State] && len(d.Signal) <= maxLabel && len(d.Reason) <= maxLabel
	e.Lifecycle = &d
	return ok
}

func (e *Event) decodePresence(raw []byte) bool {
	var d Presence
	ok := strictDecode(raw, &d) == nil && presenceEvents[d.Event] && presenceRoles[d.Role] &&
		d.ParticipantID != "" && len(d.ParticipantID) <= maxLabel && len(d.Reason) <= maxLabel
	e.Presence = &d
	return ok
}

func inRange(v, max int) bool { return v >= 1 && v <= max }

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data")
	}
	return nil
}

// chunkBuilder accumulates encoded events until they are sealed into a chunk.
type chunkBuilder struct {
	buf         bytes.Buffer
	first, last Event
	count       int64
}

func (b *chunkBuilder) add(line []byte, e Event) {
	if b.count == 0 {
		b.first = e
	}
	b.last = e
	b.count++
	b.buf.Write(line)
}

func (b *chunkBuilder) len() int { return b.buf.Len() }

func (b *chunkBuilder) seal(index int64) (store.RecordingChunk, error) {
	compressed, err := gzipBytes(b.buf.Bytes())
	if err != nil {
		return store.RecordingChunk{}, err
	}
	sum := sha256.Sum256(compressed)
	c := store.RecordingChunk{
		Index: index, FirstSeq: b.first.Seq, LastSeq: b.last.Seq, FirstOffsetMS: b.first.OffsetMS,
		LastOffsetMS: b.last.OffsetMS, EventCount: b.count, UncompressedBytes: int64(b.buf.Len()),
		Checksum: sum[:], Codec: Codec, Data: compressed,
	}
	b.buf.Reset()
	b.count = 0
	return c, nil
}

func gzipBytes(plain []byte) ([]byte, error) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

// decodeChunk verifies c's checksum, codec, size, and contents, and returns
// its events. It never returns events from a chunk that fails any check.
func decodeChunk(c store.RecordingChunk) ([]Event, error) {
	if err := checkChunkHeader(c); err != nil {
		return nil, err
	}
	plain, err := inflateExact(c)
	if err != nil {
		return nil, err
	}
	events, err := decodeLines(c, plain)
	if err != nil {
		return nil, err
	}
	if int64(len(events)) != c.EventCount || events[len(events)-1].Seq != c.LastSeq {
		return nil, corrupt("count")
	}
	if events[0].OffsetMS != c.FirstOffsetMS || events[len(events)-1].OffsetMS != c.LastOffsetMS {
		return nil, corrupt("offset")
	}
	return events, nil
}

// checkChunkHeader verifies the codec, checksum, and declared sizes before
// anything is decompressed.
func checkChunkHeader(c store.RecordingChunk) error {
	if c.Codec != Codec {
		return corrupt("codec")
	}
	sum := sha256.Sum256(c.Data)
	if subtle.ConstantTimeCompare(sum[:], c.Checksum) != 1 {
		return corrupt("checksum")
	}
	if c.UncompressedBytes <= 0 || c.UncompressedBytes > maxDecodedChunk || c.EventCount <= 0 {
		return corrupt("decompress_limit")
	}
	return nil
}

// inflateExact decompresses c, reading at most one byte past the declared
// size so a decompression bomb is caught without being expanded.
func inflateExact(c store.RecordingChunk) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(c.Data))
	if err != nil {
		return nil, corrupt("decompress")
	}
	plain, err := io.ReadAll(io.LimitReader(r, c.UncompressedBytes+1))
	if err != nil {
		return nil, corrupt("decompress")
	}
	if int64(len(plain)) > c.UncompressedBytes {
		return nil, corrupt("decompress_limit")
	}
	if int64(len(plain)) != c.UncompressedBytes || len(plain) == 0 || plain[len(plain)-1] != '\n' {
		return nil, corrupt("decompress")
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(r, 1)); err != nil {
		return nil, corrupt("decompress")
	}
	return plain, nil
}

// decodeLines decodes each line of plain, checking sequence and offset
// order against the chunk's bounds.
func decodeLines(c store.RecordingChunk, plain []byte) ([]Event, error) {
	events := make([]Event, 0, c.EventCount)
	for _, line := range bytes.SplitAfter(plain[:len(plain)-1], []byte("\n")) {
		e, err := decodeLine(line)
		if err != nil {
			return nil, err
		}
		want := c.FirstSeq + int64(len(events))
		if e.Seq != want || e.Seq > c.LastSeq {
			return nil, corrupt("sequence")
		}
		if e.OffsetMS < c.FirstOffsetMS || e.OffsetMS > c.LastOffsetMS ||
			(len(events) > 0 && e.OffsetMS < events[len(events)-1].OffsetMS) {
			return nil, corrupt("offset")
		}
		events = append(events, e)
	}
	return events, nil
}

// chunkCursor checks that chunks follow one another without gaps.
type chunkCursor struct {
	rec      store.Recording
	index    int64
	seq      int64
	offsetMS int64
}

func newChunkCursor(rec store.Recording, prev *store.RecordingChunk) chunkCursor {
	cur := chunkCursor{rec: rec}
	if prev != nil {
		cur.index, cur.seq, cur.offsetMS = prev.Index+1, prev.LastSeq, prev.LastOffsetMS
	}
	return cur
}

func (cur *chunkCursor) next(c store.RecordingChunk) ([]Event, error) {
	if c.Index != cur.index || c.FirstSeq != cur.seq+1 || c.FirstOffsetMS < cur.offsetMS {
		return nil, corrupt("chunk_order")
	}
	if c.Index >= cur.rec.ChunkCount || c.LastSeq > cur.rec.EventCount {
		return nil, corrupt("count")
	}
	if cur.rec.Status != store.RecordingActive && c.Index == cur.rec.ChunkCount-1 && c.LastSeq != cur.rec.EventCount {
		return nil, corrupt("count")
	}
	events, err := decodeChunk(c)
	if err != nil {
		return nil, err
	}
	cur.index, cur.seq, cur.offsetMS = c.Index+1, c.LastSeq, c.LastOffsetMS
	return events, nil
}

// complete reports whether every chunk of an ended recording was seen.
func (cur *chunkCursor) complete() bool {
	return cur.rec.Status == store.RecordingActive || (cur.index == cur.rec.ChunkCount && cur.seq == cur.rec.EventCount)
}

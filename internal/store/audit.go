package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// AuditEvent is an append-only security record. Details must never contain secrets.
type AuditEvent struct {
	OccurredAt time.Time
	Type       string
	RemoteAddr string
	Details    map[string]string
}

// AppendAuditEvent records an audit event.
func (s *Store) AppendAuditEvent(ctx context.Context, event AuditEvent) error {
	return appendAuditEvent(ctx, s.db, event)
}

// AppendAuditEvent records an audit event that commits or rolls back with
// the transaction.
func (t *Tx) AppendAuditEvent(ctx context.Context, event AuditEvent) error {
	return appendAuditEvent(ctx, t.tx, event)
}

func appendAuditEvent(ctx context.Context, db execer, event AuditEvent) error {
	details := event.Details
	if details == nil {
		details = map[string]string{}
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode audit details: %w", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO audit_events (occurred_at, event_type, remote_addr, details) VALUES (?, ?, ?, ?)`,
		toDB(event.OccurredAt), event.Type, event.RemoteAddr, string(encoded))
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// MaxAuditPage is the largest page AuditPage returns.
const MaxAuditPage = 200

// AuditRecord is a stored audit event with its row ID.
type AuditRecord struct {
	ID int64
	AuditEvent
}

// AuditEventPage is one page of audit events, newest first. Next is the
// cursor for the following page, or 0 when there is none.
type AuditEventPage struct {
	Events []AuditRecord
	Next   int64
}

// AuditPage lists up to limit audit events older than cursor, newest first.
// A zero cursor starts at the newest event.
func (s *Store) AuditPage(ctx context.Context, cursor int64, limit int) (AuditEventPage, error) {
	if limit < 1 || limit > MaxAuditPage || cursor < 0 {
		return AuditEventPage{}, fmt.Errorf("list audit events: limit must be 1..%d and cursor non-negative", MaxAuditPage)
	}
	query := `SELECT id, occurred_at, event_type, remote_addr, details FROM audit_events ORDER BY id DESC LIMIT ?`
	args := []any{limit + 1}
	if cursor > 0 {
		query = `SELECT id, occurred_at, event_type, remote_addr, details FROM audit_events WHERE id < ? ORDER BY id DESC LIMIT ?`
		args = []any{cursor, limit + 1}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return AuditEventPage{}, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	page := AuditEventPage{Events: make([]AuditRecord, 0, limit)}
	for rows.Next() {
		var record AuditRecord
		var occurredAt int64
		var details string
		if err := rows.Scan(&record.ID, &occurredAt, &record.Type, &record.RemoteAddr, &details); err != nil {
			return AuditEventPage{}, fmt.Errorf("scan audit event: %w", err)
		}
		record.OccurredAt = fromDB(occurredAt)
		if err := json.Unmarshal([]byte(details), &record.Details); err != nil {
			return AuditEventPage{}, fmt.Errorf("decode audit details: %w", err)
		}
		page.Events = append(page.Events, record)
	}
	if err := rows.Err(); err != nil {
		return AuditEventPage{}, fmt.Errorf("list audit events: %w", err)
	}
	if len(page.Events) > limit {
		page.Events = page.Events[:limit]
		page.Next = page.Events[limit-1].ID
	}
	return page, nil
}

// AuditEvents lists audit events in insertion order.
func (s *Store) AuditEvents(ctx context.Context) ([]AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT occurred_at, event_type, remote_addr, details FROM audit_events ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	var events []AuditEvent
	for rows.Next() {
		var event AuditEvent
		var occurredAt int64
		var details string
		if err := rows.Scan(&occurredAt, &event.Type, &event.RemoteAddr, &details); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		event.OccurredAt = fromDB(occurredAt)
		if err := json.Unmarshal([]byte(details), &event.Details); err != nil {
			return nil, fmt.Errorf("decode audit details: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

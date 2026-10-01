package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func TestTransactionalAuditCommitsAndRollsBackWithTheTransaction(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	rollback := errors.New("roll back")

	err := s.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.AppendAuditEvent(ctx, store.AuditEvent{OccurredAt: at, Type: "discarded"}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("WithTx = %v", err)
	}
	if err := s.WithTx(ctx, func(tx *store.Tx) error {
		return tx.AppendAuditEvent(ctx, store.AuditEvent{OccurredAt: at, Type: "kept", Details: map[string]string{"k": "v"}})
	}); err != nil {
		t.Fatal(err)
	}

	events, err := s.AuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "kept" || events[0].Details["k"] != "v" || !events[0].OccurredAt.Equal(at) {
		t.Fatalf("events = %+v, want only the committed one", events)
	}
}

func TestWithTxRollsBackWhenCancelledBeforeCommit(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := s.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.AppendAuditEvent(ctx, store.AuditEvent{OccurredAt: time.Now(), Type: "cancelled"}); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithTx = %v, want context.Canceled", err)
	}
	events, err := s.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

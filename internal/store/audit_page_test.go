package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func TestAuditPageListsNewestFirstWithCursor(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	for i := range 5 {
		if err := s.AppendAuditEvent(ctx, store.AuditEvent{
			OccurredAt: base.Add(time.Duration(i) * time.Second),
			Type:       fmt.Sprintf("event.%d", i),
			RemoteAddr: "192.0.2.1",
			Details:    map[string]string{"n": fmt.Sprint(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.AuditPage(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := types(first.Events); got != "event.4,event.3" {
		t.Fatalf("first page = %s", got)
	}
	if first.Next == 0 {
		t.Fatal("first page has no next cursor")
	}
	if first.Events[0].ID <= first.Events[1].ID || first.Events[0].RemoteAddr != "192.0.2.1" ||
		first.Events[0].Details["n"] != "4" || !first.Events[0].OccurredAt.Equal(base.Add(4*time.Second)) {
		t.Fatalf("first event = %+v", first.Events[0])
	}

	second, err := s.AuditPage(ctx, first.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := types(second.Events); got != "event.2,event.1" {
		t.Fatalf("second page = %s", got)
	}
	last, err := s.AuditPage(ctx, second.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := types(last.Events); got != "event.0" || last.Next != 0 {
		t.Fatalf("last page = %s next=%d", got, last.Next)
	}
}

func TestAuditPageExactPageHasNoNextCursor(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for range 2 {
		if err := s.AppendAuditEvent(ctx, store.AuditEvent{OccurredAt: time.Unix(1, 0), Type: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.AuditPage(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Next != 0 {
		t.Fatalf("page = %d events next=%d", len(page.Events), page.Next)
	}
}

func TestAuditPageRejectsInvalidLimit(t *testing.T) {
	s := openTestStore(t)
	for _, limit := range []int{0, -1, store.MaxAuditPage + 1} {
		if _, err := s.AuditPage(context.Background(), 0, limit); err == nil {
			t.Errorf("limit %d accepted", limit)
		}
	}
}

func types(events []store.AuditRecord) string {
	out := ""
	for i, e := range events {
		if i > 0 {
			out += ","
		}
		out += e.Type
	}
	return out
}

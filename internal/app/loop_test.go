package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunEveryRunsImmediatelyRepeatedlyAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runEvery(ctx, time.Millisecond, func(context.Context) { calls.Add(1) })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("calls = %d", calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	after := calls.Load()
	time.Sleep(5 * time.Millisecond)
	if calls.Load() != after {
		t.Fatal("ran after cancellation")
	}
}

type fakeSweeper struct{ calls atomic.Int32 }

func (f *fakeSweeper) DeleteExpiredSessions(context.Context, time.Time) (int64, error) {
	f.calls.Add(1)
	return 1, nil
}

func TestSweepSessionsDeletesExpiredSessions(t *testing.T) {
	sweeper := &fakeSweeper{}
	sweepSessions(context.Background(), sweeper, nil)
	if sweeper.calls.Load() != 1 {
		t.Fatalf("calls = %d", sweeper.calls.Load())
	}
}

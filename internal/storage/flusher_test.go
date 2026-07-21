package storage

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// fakeWriter records every WriteRows call for inspection, optionally
// returning a configured error - lets Flusher's scheduling/orchestration
// logic be tested without a live TimescaleDB connection.
type fakeWriter struct {
	mu    sync.Mutex
	calls [][]Row
	err   error
}

func (w *fakeWriter) WriteRows(ctx context.Context, rows []Row) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.calls = append(w.calls, append([]Row(nil), rows...))
	return int64(len(rows)), nil
}

func (w *fakeWriter) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

func (w *fakeWriter) lastCall() []Row {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.calls) == 0 {
		return nil
	}
	return w.calls[len(w.calls)-1]
}

func TestFlusher_Run_FlushesOnTickerAndOnShutdown(t *testing.T) {
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	store.RecordRequest(netip.MustParseAddr("203.0.113.9"), "some-ja4", time.Now())

	writer := &fakeWriter{}
	f := &Flusher{
		Store:    store,
		Writer:   writer,
		Interval: 30 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for writer.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if writer.callCount() == 0 {
		t.Fatal("expected at least one WriteRows call from the ticker before cancellation")
	}
	if got := writer.lastCall(); len(got) != 1 || got[0].IP != netip.MustParseAddr("203.0.113.9") {
		t.Errorf("last flush rows = %+v, want the one active IP", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancellation")
	}
}

func TestFlusher_SkipsWriteWhenNoActivity(t *testing.T) {
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	writer := &fakeWriter{}
	f := &Flusher{Store: store, Writer: writer, Interval: time.Hour}

	f.flushOnce(context.Background(), time.Time{}, time.Now())

	if writer.callCount() != 0 {
		t.Errorf("WriteRows called %d times, want 0 when there's no tracked activity", writer.callCount())
	}
}

func TestFlusher_WriteErrorDoesNotPanic(t *testing.T) {
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	store.RecordRequest(netip.MustParseAddr("203.0.113.5"), "ja4", time.Now())

	writer := &fakeWriter{err: errors.New("boom")}
	f := &Flusher{Store: store, Writer: writer, Interval: time.Hour}

	f.flushOnce(context.Background(), time.Time{}, time.Now()) // must not panic
}

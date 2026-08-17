package storage

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
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
		// Full mode, which masks the stored address like every mode and
		// adds a token beside it. This test is about which address the
		// flusher picked up, so it compares the masked form.
		IPMode:    privacy.IPFull,
		IPHashKey: testHashKey,
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
	if got := writer.lastCall(); len(got) != 1 || got[0].IP != netip.MustParseAddr("203.0.113.0") {
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

func TestFlusher_EnrichesRowsWithResolverWhenSet(t *testing.T) {
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	store.RecordRequest(netip.MustParseAddr("8.8.8.8"), "ja4", time.Now())

	writer := &fakeWriter{}
	f := &Flusher{
		Store:    store,
		Writer:   writer,
		Interval: time.Hour,
		Resolver: fakeResolver{result: asnlookup.Result{
			IP: netip.MustParseAddr("8.8.8.8"), Country: "US", ASN: 15169, ASNName: "GOOGLE", Found: true,
		}},
	}

	f.flushOnce(context.Background(), time.Time{}, time.Now())

	rows := writer.lastCall()
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Country != "US" || rows[0].ASN != 15169 || rows[0].ASNName != "GOOGLE" {
		t.Errorf("row Country/ASN/ASNName = %q/%d/%q, want US/15169/GOOGLE", rows[0].Country, rows[0].ASN, rows[0].ASNName)
	}
}

func TestFlusher_NilResolverLeavesGeoFieldsZeroValue(t *testing.T) {
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	store.RecordRequest(netip.MustParseAddr("203.0.113.7"), "ja4", time.Now())

	writer := &fakeWriter{}
	// Resolver deliberately left unset - the shape a Flusher is in when
	// asn_lookup.enabled = false (see main.go).
	f := &Flusher{Store: store, Writer: writer, Interval: time.Hour}

	f.flushOnce(context.Background(), time.Time{}, time.Now())

	rows := writer.lastCall()
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Country != "" || rows[0].ASN != 0 || rows[0].ASNName != "" {
		t.Errorf("row Country/ASN/ASNName = %q/%d/%q, want all zero value with no Resolver set", rows[0].Country, rows[0].ASN, rows[0].ASNName)
	}
}

func TestFlusher_StampsSiteIDOnWrittenRows(t *testing.T) {
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	store.RecordRequest(netip.MustParseAddr("203.0.113.11"), "ja4", time.Now())

	writer := &fakeWriter{}
	f := &Flusher{Store: store, SiteID: "ahmetteknoloji", Writer: writer, Interval: time.Hour}

	f.flushOnce(context.Background(), time.Time{}, time.Now())

	rows := writer.lastCall()
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].SiteID != "ahmetteknoloji" {
		t.Errorf("row SiteID = %q, want ahmetteknoloji", rows[0].SiteID)
	}
}

func TestFlusher_KnownBotASNsAddsScoreBonus(t *testing.T) {
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	store.RecordRequest(netip.MustParseAddr("203.0.113.9"), "ja4", time.Now())

	writer := &fakeWriter{}
	f := &Flusher{
		Store:        store,
		Writer:       writer,
		Interval:     time.Hour,
		Resolver:     fakeResolver{result: asnlookup.Result{ASN: 64512, Found: true}},
		KnownBotASNs: map[int]struct{}{64512: {}},
	}

	f.flushOnce(context.Background(), time.Time{}, time.Now())

	rows := writer.lastCall()
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if !rows[0].IsKnownBotASN {
		t.Error("row IsKnownBotASN = false, want true (ASN matches KnownBotASNs)")
	}
	if rows[0].BotScore <= 0 {
		t.Errorf("row BotScore = %d, want > 0 for a known-bot ASN", rows[0].BotScore)
	}
}

func TestFlusher_NilKnownBotASNsNoScoreBonusEvenWithResolver(t *testing.T) {
	// The shape asn_lookup.apply_to_scoring = false (the default) leaves
	// main.go in: Resolver may still be set (storage enrichment), but
	// KnownBotASNs stays nil, so ASN must never affect BotScore.
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	store.RecordRequest(netip.MustParseAddr("203.0.113.10"), "ja4", time.Now())

	writer := &fakeWriter{}
	f := &Flusher{
		Store:    store,
		Writer:   writer,
		Interval: time.Hour,
		Resolver: fakeResolver{result: asnlookup.Result{ASN: 64512, Found: true}},
	}

	f.flushOnce(context.Background(), time.Time{}, time.Now())

	rows := writer.lastCall()
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].IsKnownBotASN || rows[0].BotScore != 0 {
		t.Errorf("row IsKnownBotASN = %v, BotScore = %d, want false/0 with nil KnownBotASNs", rows[0].IsKnownBotASN, rows[0].BotScore)
	}
}

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

// TestSetKnownBotASNsReplacesTheSignalWhileFlushing.
//
// A5.2 made this list changeable from the panel. The field it replaces
// is read once per flush, so a list arriving mid-flush has to apply to
// the next one rather than to half of this one - and the read has to be
// safe while a poll is publishing a new list, which is what -race is
// here to say.
func TestSetKnownBotASNsReplacesTheSignalWhileFlushing(t *testing.T) {
	f := &Flusher{SiteID: "test-site"}

	// Built with nothing, so the field is nil and the signal is off.
	if got := f.knownBotASNs(); got != nil {
		t.Errorf("a Flusher built with no list reports %v, want nil", got)
	}

	f.SetKnownBotASNs(map[int]struct{}{64512: {}})
	if _, ok := f.knownBotASNs()[64512]; !ok {
		t.Error("the list just published is not in force")
	}

	// And back off, which is what apply_to_scoring = false means.
	f.SetKnownBotASNs(nil)
	if got := f.knownBotASNs(); got != nil {
		t.Errorf("turning the signal off left %v in force", got)
	}
}

// TestSetKnownBotASNsAppliesASwapThatKeepsTheSameLength.
//
// The case that was actually broken, and not here: cmd/collector applied
// this list only when it decided the list had changed, and decided that
// by comparing lengths. Replacing one ASN with another is the single
// most likely edit a support call produces - "it is not that network, it
// is this one" - and it is exactly the edit that keeps the length. The
// collector would have kept scoring against the old ASN while the panel
// showed the new one.
//
// The gate is gone (the collector now applies every poll and compares
// only to decide whether to log it), so what is left to hold is this:
// the setter itself must replace, not merge. A merge would leave the old
// ASN flagged forever, which is the same customer-visible symptom
// arrived at from the other direction.
func TestSetKnownBotASNsAppliesASwapThatKeepsTheSameLength(t *testing.T) {
	f := &Flusher{SiteID: "test-site"}

	const was, now = 64512, 64513
	f.SetKnownBotASNs(map[int]struct{}{was: {}})
	f.SetKnownBotASNs(map[int]struct{}{now: {}})

	if _, ok := f.knownBotASNs()[now]; !ok {
		t.Errorf("AS%d is not in force after a same-length swap", now)
	}
	if _, ok := f.knownBotASNs()[was]; ok {
		t.Errorf("AS%d is still in force after being swapped out; the setter merged instead of replacing", was)
	}
}

// TestTheFieldStillWorksForCallersThatNeverSetIt.
//
// Every existing caller builds a Flusher with the field and never calls
// the setter. Adding a live path must not change what they read - the
// atomic is consulted first and falls through when nothing was ever
// published.
func TestTheFieldStillWorksForCallersThatNeverSetIt(t *testing.T) {
	f := &Flusher{SiteID: "test-site", KnownBotASNs: map[int]struct{}{64512: {}}}
	if _, ok := f.knownBotASNs()[64512]; !ok {
		t.Error("a Flusher built with the field no longer reads it")
	}
}

// TestSetKnownBotASNsIsSafeAlongsideReads, under -race.
func TestSetKnownBotASNsIsSafeAlongsideReads(t *testing.T) {
	f := &Flusher{SiteID: "test-site"}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = f.knownBotASNs()
				}
			}
		}()
	}
	for i := range 200 {
		// A fresh map each time. Mutating a published one would be a
		// data race no atomic pointer can fix, which is why the setter's
		// contract says the caller builds and hands over.
		f.SetKnownBotASNs(map[int]struct{}{64512 + i: {}})
	}
	close(stop)
	wg.Wait()
}

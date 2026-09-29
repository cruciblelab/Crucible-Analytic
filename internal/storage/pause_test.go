package storage

import (
	"context"
	"net/netip"
	"sort"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// pauseFixture is a flusher over a memory store whose addresses are seen
// at chosen moments, and a pause.
type pauseFixture struct {
	store  *ratestore.MemoryRateStore
	writer *fakeWriter
	pause  *Pause
	f      *Flusher
}

func newPauseFixture(t *testing.T) *pauseFixture {
	t.Helper()
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	t.Cleanup(store.Close)
	writer := &fakeWriter{}
	pause := &Pause{}
	return &pauseFixture{store: store, writer: writer, pause: pause,
		f: &Flusher{Store: store, SiteID: "duraklat", Writer: writer, Interval: time.Hour, Pause: pause}}
}

func (p *pauseFixture) seen(ip string, at time.Time) {
	p.store.RecordRequest(netip.MustParseAddr(ip), "ja4", at)
}

// written is every address the writer has been given, sorted - as the
// rows carry it, masked to its /24, which is why each address the tests
// use sits in a /24 of its own.
func (p *pauseFixture) written() []string {
	p.writer.mu.Lock()
	defer p.writer.mu.Unlock()
	var out []string
	for _, call := range p.writer.calls {
		for _, row := range call {
			out = append(out, row.IP.String())
		}
	}
	sort.Strings(out)
	return out
}

func sameAddresses(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestAPauseHoldsWhatWasLastSeenInsideIt (PLAN §4, #3): a row belongs to
// the moment of its address's latest request, so a flush holds back
// exactly the addresses last seen inside the pause's span - asked on both
// sides of both edges, with the flush running after the pause has ended.
//
// The flush runs after the end on purpose. The first version asked
// whether the flush itself ran paused, and a flush after the end then
// wrote the visits made in the pause's final interval; a test that only
// flushed inside the pause could not tell the two apart.
func TestAPauseHoldsWhatWasLastSeenInsideIt(t *testing.T) {
	p := newPauseFixture(t)
	t0 := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	start, end := t0.Add(10*time.Second), t0.Add(70*time.Second)
	p.pause.SetUntil(end, start)

	p.seen("198.18.1.5", start.Add(-time.Nanosecond)) // before: recorded
	p.seen("198.18.2.5", start)                       // the first moment held
	p.seen("198.18.3.5", end.Add(-time.Nanosecond))   // the last moment held
	p.seen("198.18.4.5", end)                         // recording again

	p.f.flushOnce(context.Background(), t0, end.Add(5*time.Second))
	if got := p.written(); !sameAddresses(got, "198.18.1.0", "198.18.4.0") {
		t.Errorf("written %v; want the address seen before the pause and the one seen at its end", got)
	}
	if p.pause.Held() != 2 {
		t.Errorf("held %d rows, want the 2 last seen inside the pause", p.pause.Held())
	}
}

// TestAPauseBeginsWhenItIsApplied: the span starts when this process first
// hears of the pause, and hearing again - the settings poll repeats every
// minute - neither restarts it nor loses it.
func TestAPauseBeginsWhenItIsApplied(t *testing.T) {
	p := newPauseFixture(t)
	t0 := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	end := t0.Add(time.Hour)
	p.pause.SetUntil(end, t0)
	p.pause.SetUntil(end, t0.Add(time.Minute))                  // the next poll
	p.pause.SetUntil(end.Add(time.Hour), t0.Add(2*time.Minute)) // extended

	p.seen("198.18.5.5", t0.Add(30*time.Second))
	p.f.flushOnce(context.Background(), t0, t0.Add(3*time.Minute))
	if got := p.written(); len(got) != 0 {
		t.Errorf("written %v; a repeated or extended pause is the same span", got)
	}
	// Everything held is no write at all, rather than an empty one.
	if n := p.writer.callCount(); n != 0 {
		t.Errorf("the writer was called %d times with nothing to write", n)
	}
}

// TestLiftingAPauseEndsItThen: resuming stores no end - the setting goes
// back to empty - so the span ends when the lift is applied, and what was
// last seen before that stays held even when the flush runs after.
func TestLiftingAPauseEndsItThen(t *testing.T) {
	for _, lift := range []struct {
		name string
		end  func(now time.Time) time.Time
	}{
		{"emptied", func(time.Time) time.Time { return time.Time{} }},
		{"set to a moment already past", func(now time.Time) time.Time { return now.Add(-time.Minute) }},
	} {
		t.Run(lift.name, func(t *testing.T) {
			p := newPauseFixture(t)
			t0 := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
			lifted := t0.Add(time.Minute)
			p.pause.SetUntil(t0.Add(time.Hour), t0)
			p.pause.SetUntil(lift.end(lifted), lifted)
			p.pause.SetUntil(lift.end(lifted.Add(time.Minute)), lifted.Add(time.Minute)) // the next poll

			p.seen("198.18.6.5", lifted.Add(-time.Nanosecond))
			p.seen("198.18.7.5", lifted)
			p.f.flushOnce(context.Background(), t0, lifted.Add(2*time.Minute))
			if got := p.written(); !sameAddresses(got, "198.18.7.0") {
				t.Errorf("written %v; want only the address seen once the lift applied", got)
			}
		})
	}
}

// TestAPauseThatEndedIsNotReopenedByItsOwnValue: after the end passes, the
// setting still holds that moment and every poll hands it back. That must
// not start a new span - nor, when a new pause does come, may the old
// one's rows be judged by it.
func TestAPauseThatEndedIsNotReopenedByItsOwnValue(t *testing.T) {
	p := newPauseFixture(t)
	t0 := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	end := t0.Add(time.Minute)
	p.pause.SetUntil(end, t0)
	p.pause.SetUntil(end, end.Add(time.Minute)) // a poll after the end

	p.seen("198.18.8.5", end.Add(30*time.Second))
	p.f.flushOnce(context.Background(), end, end.Add(2*time.Minute))
	if got := p.written(); !sameAddresses(got, "198.18.8.0") {
		t.Errorf("written %v; a pause whose end had passed held a later visit", got)
	}
}

// TestAFlusherWithoutAPauseRecords: nil is never paused - every test that
// builds a Flusher, and every build before the pause, reads so - and a
// Pause never set holds nothing.
func TestAFlusherWithoutAPauseRecords(t *testing.T) {
	var none *Pause
	if none.Holds(time.Now()) || none.Held() != 0 {
		t.Error("a nil pause holds something")
	}
	if (&Pause{}).Holds(time.Now()) {
		t.Error("a pause never set holds the present")
	}
	p := newPauseFixture(t)
	p.f.Pause = nil
	now := time.Now()
	p.seen("198.18.9.5", now)
	p.f.flushOnce(context.Background(), time.Time{}, now)
	if got := p.written(); !sameAddresses(got, "198.18.9.0") {
		t.Errorf("written %v without a pause", got)
	}
}

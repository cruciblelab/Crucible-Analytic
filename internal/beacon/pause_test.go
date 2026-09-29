package beacon

import (
	"net/http"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

func eventFor(site string) string {
	return `{"site":"` + site + `","type":"pageview","url":"/"}`
}

// TestAPausedSiteIsAnsweredAndNotRecorded (PLAN §4, #3): the event is
// answered as an accepted one and nothing reaches the sink; another
// site goes on being recorded; an unknown site is refused exactly as
// before - paused or not, so a pause is never a way to learn which site
// names are accepted; and the heartbeat says what the pause held back.
func TestAPausedSiteIsAnsweredAndNotRecorded(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.SetSites([]string{"acme", "diger"})
	s.SetPaused(map[string]time.Time{"acme": s.now().Add(time.Hour), "yok": s.now().Add(time.Hour)})

	if w := post(t, s, eventFor("acme")); w.Code != http.StatusNoContent {
		t.Errorf("a paused site's event answered %d, want 204 as an accepted one", w.Code)
	}
	if len(sink.rows) != 0 {
		t.Fatalf("%d rows reached the sink from a paused site", len(sink.rows))
	}
	if w := post(t, s, eventFor("diger")); w.Code != http.StatusNoContent || len(sink.rows) != 1 {
		t.Errorf("another site's event answered %d with %d rows; a pause is per site", w.Code, len(sink.rows))
	}
	if w := post(t, s, eventFor("yok")); w.Code != http.StatusForbidden {
		t.Errorf("an unknown site answered %d; a pause must not change what is refused", w.Code)
	}

	row := HeartbeatCounters(s, fixedWriter{})
	if row[heartbeat.CounterPaused] != 1 || row[heartbeat.CounterAccepted] != 1 {
		t.Errorf("heartbeat: %d held, %d accepted; want 1 and 1", row[heartbeat.CounterPaused], row[heartbeat.CounterAccepted])
	}
}

// TestAPauseEndsAtItsMoment: the end is read against the clock at each
// event, not at the next settings poll - and asked from both sides.
func TestAPauseEndsAtItsMoment(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	base := s.now()
	clock := base
	s.Now = func() time.Time { return clock }
	end := base.Add(time.Minute)
	s.SetPaused(map[string]time.Time{"acme": end})

	clock = end.Add(-time.Nanosecond)
	post(t, s, eventFor("acme"))
	if len(sink.rows) != 0 || s.Held() != 1 {
		t.Fatalf("just before the end: %d rows, %d held; want 0 and 1", len(sink.rows), s.Held())
	}
	clock = end
	post(t, s, eventFor("acme"))
	if len(sink.rows) != 1 || s.Held() != 1 {
		t.Errorf("at the end: %d rows, %d held; want 1 and 1", len(sink.rows), s.Held())
	}
}

// TestThePausedSitesAreCopied: a caller that reuses its map cannot change
// what the server holds back while it serves.
func TestThePausedSitesAreCopied(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	ends := map[string]time.Time{"acme": s.now().Add(time.Hour)}
	s.SetPaused(ends)
	delete(ends, "acme")
	post(t, s, eventFor("acme"))
	if len(sink.rows) != 0 {
		t.Error("changing the caller's map after SetPaused lifted the pause")
	}
}

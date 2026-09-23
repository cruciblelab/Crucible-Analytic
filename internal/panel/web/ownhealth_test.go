package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

// ownLog is a panel log copy with a fixed answer.
type ownLog struct {
	lost uint64
	text string
	at   time.Time
}

func (o ownLog) Lost() uint64                   { return o.lost }
func (o ownLog) LastError() (string, time.Time) { return o.text, o.at }

// The panel's heartbeat counts what its deadline answered.
//
// Taken through withDeadline - the wrapper the panel's handler chain
// actually uses - with a request whose deadline has already passed, so
// the real TimeoutHandler answers at once. The handler waits for the
// test to see the answer first, because one that returned the moment its
// context ended would race TimeoutHandler's select and lose half the
// time.
func TestThePanelsHeartbeatCountsItsDeadline(t *testing.T) {
	s := newTestServer(t)
	if got := s.Counters()[heartbeat.CounterDeadline]; got != 0 {
		t.Fatalf("a fresh panel reports %d requests past deadline", got)
	}

	release := make(chan struct{})
	h := s.withDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		<-release
	}))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
	close(release)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the deadline's 503", w.Code)
	}

	counters := s.Counters()
	if counters[heartbeat.CounterDeadline] != 1 {
		t.Errorf("counters = %v, want %s = 1", counters, heartbeat.CounterDeadline)
	}
	// Only its own number: the loss count and the last error are the
	// reporter's to add, from the log copy, as for every service.
	if len(counters) != 1 {
		t.Errorf("counters = %v, want only the deadline's", counters)
	}
}

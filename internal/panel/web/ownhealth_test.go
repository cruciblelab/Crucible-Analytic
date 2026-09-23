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

// The panel's own row, which until Z6 was a version and a sentence while
// the process held both numbers: one WARN line per request its deadline
// answered, and a log-loss counter nothing read.
//
// The deadline count is taken through withDeadline - the wrapper the
// panel's handler chain actually uses - with a request whose deadline
// has already passed, so the real TimeoutHandler answers at once. The
// handler waits for the test to see the answer first, because one that
// returned the moment its context ended would race TimeoutHandler's
// select and lose half the time.
func TestThePanelsRowCarriesItsOwnNumbers(t *testing.T) {
	s := newTestServer(t)
	at := time.Date(2026, 9, 23, 9, 47, 22, 0, time.UTC)
	s.OwnLog = ownLog{lost: 2, text: "panel: reading storage facts: permission denied", at: at}

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

	lang := s.Renderer.Catalogs().ByCode("tr")
	row := s.panelRow(lang)
	got := map[string]int64{}
	for _, c := range row.Counters {
		got[c.Label] = c.Value
	}
	want := map[string]int64{
		lang.T("saglik.sayac." + heartbeat.CounterDeadline): 1,
		lang.T("saglik.sayac." + heartbeat.CounterLogLost):  2,
	}
	if len(got) != len(want) {
		t.Errorf("the panel's row draws %v, want %v", got, want)
	}
	for label, v := range want {
		if got[label] != v {
			t.Errorf("%s = %d, want %d (row: %v)", label, got[label], v, got)
		}
	}
	if row.LastError != "panel: reading storage facts: permission denied" || !row.LastErrorAt.Equal(at) {
		t.Errorf("last error = %q at %v, want the log copy's", row.LastError, row.LastErrorAt)
	}
}

// A panel started without a log copy - by hand, or by a test - draws its
// deadline count and nothing it cannot know: no loss count, no last
// error. Zero lost is a claim; an absent number is not one.
func TestThePanelsRowWithoutALogCopySaysOnlyWhatItKnows(t *testing.T) {
	s := newTestServer(t)
	row := s.panelRow(s.Renderer.Catalogs().ByCode("tr"))
	if len(row.Counters) != 1 {
		t.Errorf("the row draws %d counters without a log copy, want only the deadline's: %v",
			len(row.Counters), row.Counters)
	}
	if row.LastError != "" {
		t.Errorf("last error = %q without a log copy", row.LastError)
	}
}

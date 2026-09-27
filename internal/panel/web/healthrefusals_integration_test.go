//go:build integration

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// keepEverything is a beacon sink with room for every row.
type keepEverything struct{}

func (keepEverything) Enqueue(beacon.Row) bool { return true }

// idleWriter is a beacon writer that has written nothing.
type idleWriter struct{}

func (idleWriter) Counters() (written, dropped uint64) { return 0, 0 }

// refusingBeacon is a real beacon server that has refused what the
// bodies ask it to refuse, and its heartbeat counters.
//
// The producer rather than a map typed here, and that is the point of
// the test. A counter map written in this file would agree with the page
// it was written for and not necessarily with the beacon - a fixture
// that spells out a wire format is right the day it is written and only
// by accident after.
func refusingBeacon(t *testing.T, bodies ...string) map[string]int64 {
	t.Helper()
	b := &beacon.Server{Sites: []string{"acme"}, Sink: keepEverything{}}
	for _, body := range bodies {
		r := httptest.NewRequest(http.MethodPost, beacon.DefaultPathPrefix+"/event", strings.NewReader(body))
		r.RemoteAddr = "203.0.113.9:41234"
		b.Handler().ServeHTTP(httptest.NewRecorder(), r)
	}
	return beacon.HeartbeatCounters(b, idleWriter{})
}

// TestTheHealthPageSaysWhyEventsWereRefused: the beacon's refusals reach
// the health page by reason, and the unknown-site ones with the sentence
// that says what to do - through the whole path: the beacon's server
// refusing, its own function turning that into heartbeat counters, the
// reporter writing them as beacon_writer, and the page reading the row.
func TestTheHealthPageSaysWhyEventsWereRefused(t *testing.T) {
	server, client, store := healthServer(t)

	counters := refusingBeacon(t,
		`{"site":"baskasinin-sitesi","type":"pageview","url":"/"}`,
		`{"site":"baskasinin-sitesi","type":"pageview","url":"/"}`,
		`{"site":`,
		`{"site":"acme","type":"pageview","url":"/"}`,
	)
	writeBeatDetail(t, store, testdb.Beacon, "saglik-b3b-red", time.Now().Add(-time.Hour),
		counters, nil, "", heartbeat.TokenKeyUnknown)

	status, body := get(t, client, server.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d", status)
	}
	for _, want := range []string{
		"Reddedilen: 3",
		"<li>bilinmeyen site: 2</li>",
		"<li>okunamayan istek: 1</li>",
		`<span class="uyari satir-ici">Bilinmeyen site</span>`,
		"Servis başladığından beri 2 olay",
		"beacon.sites",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
	// The reasons that did not happen are not drawn.
	for _, absent := range []string{"geçersiz olay:", "kapasite dolu:", "diğer:"} {
		if strings.Contains(body, absent) {
			t.Errorf("the page draws %q, a reason with nothing under it", absent)
		}
	}
}

// TestNoUnknownSiteNoSentence: the sentence is about the allowlist, so
// it appears only when the allowlist turned something away. Refusals of
// another kind are drawn by reason and say nothing about the allowlist.
//
// Without this, a page that always printed the sentence would pass the
// test above - it asks only that the sentence is there when it should be.
func TestNoUnknownSiteNoSentence(t *testing.T) {
	server, client, store := healthServer(t)

	counters := refusingBeacon(t, `{"site":`, `{"site":"acme","type":"click","url":"/"}`)
	writeBeatDetail(t, store, testdb.Beacon, "saglik-b3b-yok", time.Now().Add(-time.Hour),
		counters, nil, "", heartbeat.TokenKeyUnknown)

	_, body := get(t, client, server.URL+HealthPath)
	if !strings.Contains(body, "<li>okunamayan istek: 1</li>") || !strings.Contains(body, "<li>geçersiz olay: 1</li>") {
		t.Fatalf("the page does not draw the two refusals this test made; the row is not the one it wrote")
	}
	if strings.Contains(body, `<span class="uyari satir-ici">Bilinmeyen site</span>`) {
		t.Error("the page says events were turned away for an unknown site, and none were")
	}
}

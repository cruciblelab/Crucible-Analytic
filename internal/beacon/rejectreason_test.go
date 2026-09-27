package beacon

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
)

// TestEveryRefusalIsCountedUnderItsReason: each way the beacon refuses an
// event lands in its own counter, and in no other.
//
// "In no other" is half the claim. A refusal counted under the wrong
// reason sends the operator to the wrong fix - an unknown site filed as
// "unreadable" has them debugging the snippet while the allowlist is the
// problem - so every case also asks that the other reasons stayed at
// zero, and that the total is exactly the refusals the responses show.
func TestEveryRefusalIsCountedUnderItsReason(t *testing.T) {
	const valid = `{"site":"acme","type":"pageview","url":"/"}`
	cases := []struct {
		name   string
		reason string
		status int
		setup  func(*Server)
		body   string
		mutate func(*http.Request)
		// repeat is for the limiter, which lets the first request in.
		repeat int
	}{
		{name: "a site the allowlist does not name", reason: heartbeat.CounterRejectedUnknownSite,
			status: http.StatusForbidden, body: `{"site":"baskasinin-sitesi","type":"pageview","url":"/"}`},
		{name: "a body that is not JSON", reason: heartbeat.CounterRejectedMalformed,
			status: http.StatusBadRequest, body: `{"site":`},
		{name: "a body larger than the beacon reads", reason: heartbeat.CounterRejectedMalformed,
			status: http.StatusBadRequest,
			body:   `{"site":"acme","type":"pageview","url":"/` + strings.Repeat("a", maxBodyBytes) + `"}`},
		{name: "an event of a type the beacon does not take", reason: heartbeat.CounterRejectedInvalid,
			status: http.StatusBadRequest, body: `{"site":"acme","type":"click","url":"/"}`},
		{name: "an event naming no site", reason: heartbeat.CounterRejectedInvalid,
			status: http.StatusBadRequest, body: `{"type":"pageview","url":"/"}`},
		{name: "a limiter that refuses", reason: heartbeat.CounterRejectedOverCapacity,
			status: http.StatusTooManyRequests, body: valid, repeat: 20,
			setup: func(s *Server) {
				s.Limiter = limiter.New(limiter.Config{MaxRequestsPerSecond: 1, Policy: limiter.PolicyFailClosed})
			}},
		{name: "an address that does not resolve", reason: heartbeat.CounterRejectedOther,
			status: http.StatusBadRequest, body: valid,
			mutate: func(r *http.Request) { r.RemoteAddr = "not-an-address" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, &fakeSink{})
			if tc.setup != nil {
				tc.setup(s)
			}
			var mutate []func(*http.Request)
			if tc.mutate != nil {
				mutate = append(mutate, tc.mutate)
			}
			refused := 0
			for range max(tc.repeat, 1) {
				if post(t, s, tc.body, mutate...).Code == tc.status {
					refused++
				}
			}
			if refused == 0 {
				t.Fatalf("no request was answered %d; the case did not produce the refusal it is about", tc.status)
			}

			got := s.RejectionCounters()
			if got[tc.reason] != int64(refused) {
				t.Errorf("%s = %d after %d refusals", tc.reason, got[tc.reason], refused)
			}
			for key, n := range got {
				if key != tc.reason && n != 0 {
					t.Errorf("%s = %d; this refusal was also counted as a reason it is not", key, n)
				}
			}
			if _, _, total := s.Counters(); total != uint64(refused) {
				t.Errorf("the rejected total is %d after %d refusals", total, refused)
			}
		})
	}
}

// TestEveryReasonHasItsOwnCounter: every reason is reported, under a key
// of its own, and the keys are exactly the set the heartbeat declares.
//
// The other direction matters as much: a key the heartbeat declares and
// no reason reports is a line the page can draw and nothing will ever
// fill, which reads as "this never happens" when the truth is "this is
// never counted". Which reason lands under which key is the refusal
// test's question; two reasons under one key does not compile.
func TestEveryReasonHasItsOwnCounter(t *testing.T) {
	got := newTestServer(t, &fakeSink{}).RejectionCounters()
	if len(got) != int(rejectReasons) {
		t.Errorf("RejectionCounters reports %d keys and there are %d reasons; a reason without a key "+
			"is counted and never reported", len(got), rejectReasons)
	}
	declared := map[string]bool{}
	for _, key := range heartbeat.RejectionCounters {
		declared[key] = true
		if _, ok := got[key]; !ok {
			t.Errorf("heartbeat declares %q and no reason here reports it", key)
		}
	}
	for key := range got {
		if !declared[key] {
			t.Errorf("%q is reported here and missing from heartbeat.RejectionCounters, so no page draws it", key)
		}
	}
}

// fixedWriter is a writer whose counters are whatever the test says.
type fixedWriter struct{ written, dropped uint64 }

func (w fixedWriter) Counters() (written, dropped uint64) { return w.written, w.dropped }

// TestTheHeartbeatCarriesTheReasons: what the beacon's row says is the
// server's refusals by reason as well as their total, and the two agree.
//
// This is the function main calls, and the reason it is not a closure
// in main any more: the merge of the reasons into the row is the step a
// main could leave out and still compile.
func TestTheHeartbeatCarriesTheReasons(t *testing.T) {
	s := newTestServer(t, &fakeSink{})
	post(t, s, `{"site":"baskasinin-sitesi","type":"pageview","url":"/"}`)
	post(t, s, `{"site":"baskasinin-sitesi","type":"pageview","url":"/"}`)
	post(t, s, `{"site":`)
	post(t, s, `{"site":"acme","type":"pageview","url":"/"}`)

	row := HeartbeatCounters(s, fixedWriter{written: 7, dropped: 2})

	want := map[string]int64{
		heartbeat.CounterAccepted:             1,
		heartbeat.CounterRejected:             3,
		heartbeat.CounterWritten:              7,
		heartbeat.CounterDropped:              2,
		heartbeat.CounterRejectedUnknownSite:  2,
		heartbeat.CounterRejectedMalformed:    1,
		heartbeat.CounterRejectedInvalid:      0,
		heartbeat.CounterRejectedOverCapacity: 0,
		heartbeat.CounterRejectedOther:        0,
	}
	for key, n := range want {
		got, present := row[key]
		if !present {
			t.Errorf("the heartbeat row has no %q", key)
			continue
		}
		if got != n {
			t.Errorf("%s = %d, want %d", key, got, n)
		}
	}
	if len(row) != len(want) {
		t.Errorf("the heartbeat row has %d counters, want %d: %v", len(row), len(want), row)
	}
	var parts int64
	for _, key := range heartbeat.RejectionCounters {
		parts += row[key]
	}
	if parts != row[heartbeat.CounterRejected] {
		t.Errorf("the reasons add up to %d and the total says %d", parts, row[heartbeat.CounterRejected])
	}
}

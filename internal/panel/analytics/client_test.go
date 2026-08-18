package analytics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTheThreeFailuresAreToldApart.
//
// Each one sends the reader somewhere different - wait, fix the token,
// check the site id - so a client that collapsed them into one error
// would make the page unable to say anything useful. Driven through a
// real server rather than a stub: the mapping being checked is from HTTP
// status to error, and a stub would be testing the stub's idea of it.
func TestTheThreeFailuresAreToldApart(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrRefused},
		{http.StatusForbidden, ErrRefused},
		{http.StatusNotFound, ErrNoSite},
		{http.StatusInternalServerError, ErrUnavailable},
		{http.StatusBadGateway, ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := mustClient(t, srv.URL, "jeton")
			board := c.FetchDashboard(context.Background(), "bir", hourFrom, hourTo)
			if !errors.Is(board.TrafficErr, tc.want) {
				t.Errorf("traffic error = %v, want %v", board.TrafficErr, tc.want)
			}
			if !errors.Is(board.BeaconErr, tc.want) {
				t.Errorf("beacon error = %v, want %v", board.BeaconErr, tc.want)
			}
		})
	}
}

// TestAnUnreachableAPIIsNotAPanic. The panel has to render its page.
func TestAnUnreachableAPIIsNotAPanic(t *testing.T) {
	// A port nothing is listening on.
	c := mustClient(t, "http://127.0.0.1:1", "jeton")
	board := c.FetchDashboard(context.Background(), "bir", hourFrom, hourTo)
	if !errors.Is(board.TrafficErr, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", board.TrafficErr)
	}
	// And the zero values are zero, which the page must not read as
	// measurements - see the dashboard's own tests.
	if board.Traffic.Snapshots != 0 || board.Beacon.Pageviews != 0 {
		t.Error("a failed fetch produced numbers")
	}
}

// TestANilClientAnswersRatherThanCrashing.
//
// A panel configured before group D existed has no API address, and a
// deployment halfway through installation has none either. Both must
// render the page that says so.
func TestANilClientAnswersRatherThanCrashing(t *testing.T) {
	c, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Configured() {
		t.Fatal("an empty address produced a configured client")
	}
	board := c.FetchDashboard(context.Background(), "bir", hourFrom, hourTo)
	if !errors.Is(board.TrafficErr, ErrUnavailable) || !errors.Is(board.BeaconErr, ErrUnavailable) {
		t.Errorf("a nil client returned %v / %v", board.TrafficErr, board.BeaconErr)
	}
	if _, _, err := c.KnownSites(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("KnownSites on a nil client returned %v", err)
	}
}

// TestTheTwoSummariesAreFetchedConcurrently.
//
// Not an optimisation to check off: a page that cost the sum of its
// calls would take twice as long the day the API is slow, which is
// exactly the day somebody is watching it. Measured by making both
// handlers sleep and requiring the pair to finish in well under twice
// one of them.
func TestTheTwoSummariesAreFetchedConcurrently(t *testing.T) {
	const delay = 200 * time.Millisecond
	var inFlight, peak atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(delay)
		inFlight.Add(-1)
		_, _ = w.Write([]byte(`{"site_id":"bir"}`))
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL, "jeton")
	started := time.Now()
	board := c.FetchDashboard(context.Background(), "bir", hourFrom, hourTo)
	elapsed := time.Since(started)

	if board.TrafficErr != nil || board.BeaconErr != nil {
		t.Fatalf("errors: %v / %v", board.TrafficErr, board.BeaconErr)
	}
	if peak.Load() < 2 {
		t.Errorf("the two calls never overlapped; the page costs the sum of its requests")
	}
	if elapsed >= 2*delay {
		t.Errorf("the pair took %v, which is at least both delays end to end", elapsed)
	}
}

// TestAWholePageHasADeadline.
//
// A dashboard held open by a wedged API is a browser tab that never
// finishes and a panel goroutine that never returns. The bound has to
// exist even when nothing errors.
func TestAWholePageHasADeadline(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(blocked)

	c := mustClient(t, srv.URL, "jeton")
	// A tighter deadline than the package's own, so the test does not
	// spend RequestTimeout proving a timeout exists.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	started := time.Now()
	board := c.FetchDashboard(ctx, "bir", hourFrom, hourTo)
	if elapsed := time.Since(started); elapsed > RequestTimeout {
		t.Errorf("the fetch took %v; the caller's deadline was ignored", elapsed)
	}
	if !errors.Is(board.TrafficErr, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", board.TrafficErr)
	}
}

// TestTheTokenIsSentAndTheRangeIsUTC.
//
// The range crosses a process boundary, so it goes in one unambiguous
// form. The panel computes whole days in the site's zone and this
// converts them - a local timestamp with no offset would be read as
// whatever the API's own clock thinks.
func TestTheTokenIsSentAndTheRangeIsUTC(t *testing.T) {
	// Guarded, because the two calls are concurrent by design and the
	// handler runs once per goroutine. The first version of this test
	// wrote these without a lock and -race caught it - which is the
	// concurrency the test above asserts, arriving as a failure here.
	var (
		mu                               sync.Mutex
		gotAuth, gotFrom, gotTo, gotPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotFrom = r.URL.Query().Get("from")
		gotTo = r.URL.Query().Get("to")
		if strings.HasSuffix(r.URL.Path, "/summary") && !strings.Contains(r.URL.Path, "beacon") {
			gotPath = r.URL.Path
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"site_id":"bir"}`))
	}))
	defer srv.Close()

	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Skip("this machine has no timezone database")
	}
	from := time.Date(2026, 8, 12, 0, 0, 0, 0, istanbul)
	to := time.Date(2026, 8, 19, 0, 0, 0, 0, istanbul)

	c := mustClient(t, srv.URL, "gizli-jeton")
	c.FetchDashboard(context.Background(), "bir site", from, to)

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer gizli-jeton" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.HasSuffix(gotFrom, "Z") || !strings.HasSuffix(gotTo, "Z") {
		t.Errorf("the range was not sent in UTC: %s .. %s", gotFrom, gotTo)
	}
	// Midnight in Istanbul is 21:00 the previous day in UTC. The instant
	// has to survive the conversion, not the wall clock.
	if want := "2026-08-11T21:00:00Z"; gotFrom != want {
		t.Errorf("from = %s, want %s", gotFrom, want)
	}
	// A site id with a space in it has to reach the path escaped, or the
	// request never arrives at all.
	if want := "/api/v1/sites/bir%20site/summary"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// TestAnEnormousResponseIsRefused.
//
// The API is not hostile, but it is a separate process that can be
// upgraded independently, misconfigured, or replaced by whatever else
// ends up listening on that port. A summary is a few hundred bytes.
func TestAnEnormousResponseIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("A", 64<<10)
		for range (maxResponseBytes / len(chunk)) + 2 {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL, "jeton")
	board := c.FetchDashboard(context.Background(), "bir", hourFrom, hourTo)
	if !errors.Is(board.TrafficErr, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", board.TrafficErr)
	}
}

// TestGarbageThatIsNotJSONIsNotANumber. A 200 that does not decode is
// something other than this API answering on that port, and from the
// reader's side it is the same fact: no numbers.
func TestGarbageThatIsNotJSONIsNotANumber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL, "jeton")
	board := c.FetchDashboard(context.Background(), "bir", hourFrom, hourTo)
	if !errors.Is(board.TrafficErr, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", board.TrafficErr)
	}
}

func TestNewRefusesAnAddressThatIsNotOne(t *testing.T) {
	for _, base := range []string{"ftp://example.invalid", "example.invalid", "http://"} {
		if _, err := New(base, "x"); err == nil {
			t.Errorf("New accepted %q", base)
		}
	}
}

func mustClient(t *testing.T, base, token string) *Client {
	t.Helper()
	c, err := New(base, token)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("New returned no client for a real address")
	}
	return c
}

// hourFrom and hourTo spread that range across the two arguments
// FetchDashboard takes.
var hourFrom, hourTo = func() (time.Time, time.Time) {
	to := time.Now().Truncate(time.Hour)
	return to.Add(-time.Hour), to
}()

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// waitingStore answers Sites only after the request is over and the
// test has seen the answer, so the request is decided by how its context
// ended and never by a race with the handler.
//
// Not merely "after ctx.Done()": TimeoutHandler selects between the
// handler finishing and the context ending, and a handler that returns
// the instant its context ends makes both ready at once - Go then picks
// one at random, and a test would see the deadline's 503 half the time
// and the handler's own 500 the other half.
type waitingStore struct {
	*fakeStore
	release chan struct{}
}

func newWaitingStore() waitingStore {
	return waitingStore{fakeStore: &fakeStore{}, release: make(chan struct{})}
}

func (s waitingStore) Sites(ctx context.Context) ([]string, error) {
	<-ctx.Done()
	<-s.release
	return nil, ctx.Err()
}

// safeBuffer is a log destination the handler's goroutine and the test
// can share - TimeoutHandler runs the handler on a goroutine of its own.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// levels is each line's level and message.
func (b *safeBuffer) levels(t *testing.T) []string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", sc.Text())
		}
		out = append(out, m["level"].(string)+" "+m["msg"].(string))
	}
	return out
}

// countingServer is a server over store whose own log and the default
// log (writeJSON's) both go to one buffer.
func countingServer(t *testing.T, store Querier) (*Server, *safeBuffer) {
	t.Helper()
	logs := &safeBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	auth, err := NewAuthenticator([]Token{testToken("panel", "panel-secret", WildcardSite)})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Store: store, Auth: auth, Logger: logger, Now: func() time.Time { return fixedNow }}, logs
}

func sitesRequest(ctx context.Context) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sites", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer panel-secret")
	return r
}

// What the health page's two API counters mean, asked of the handler the
// binary serves - deadline and all - in the three ways a query can end
// without an answer. Measured on the real binary before Z6: all three
// wrote "api: query failed" at ERROR, the second and third twice over,
// and the heartbeat row said nothing about any of them.
func TestTheAPICountsFailuresAndDeadlinesApart(t *testing.T) {
	t.Run("a query that fails is a failure", func(t *testing.T) {
		srv, logs := countingServer(t, &fakeStore{err: errors.New("permission denied for table beacon_events")})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, sitesRequest(context.Background()))

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
		if failed, past := srv.Counters(); failed != 1 || past != 0 {
			t.Errorf("counters = failed %d, past deadline %d; want 1 and 0", failed, past)
		}
		if got := logs.levels(t); len(got) != 1 || got[0] != "ERROR api: query failed" {
			t.Errorf("log = %q, want the one ERROR", got)
		}
	})

	// A request whose deadline has already passed when it arrives: the
	// deadline's own context is born done, so the real TimeoutHandler
	// answers at once - the whole chain in microseconds rather than 55
	// seconds, and through srv.Handler(), so the counter is asked of the
	// wiring the binary has rather than of a copy.
	t.Run("a query the deadline stopped is a deadline and not a failure", func(t *testing.T) {
		store := newWaitingStore()
		srv, logs := countingServer(t, store)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, sitesRequest(ctx))

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want the deadline's 503", w.Code)
		}
		close(store.release)
		waitForHandler(t, logs, 1)
		if failed, past := srv.Counters(); failed != 0 || past != 1 {
			t.Errorf("counters = failed %d, past deadline %d; want 0 and 1", failed, past)
		}
		if got := logs.levels(t); len(got) != 1 || got[0] != "WARN request ran past its deadline and was answered 503" {
			t.Errorf("log = %q, want only the deadline's WARN - an ERROR here is the second line "+
				"measured before Z6, and it would be the service's last error", got)
		}
	})

	t.Run("a client that went away is neither", func(t *testing.T) {
		store := newWaitingStore()
		srv, logs := countingServer(t, store)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		srv.Handler().ServeHTTP(httptest.NewRecorder(), sitesRequest(ctx))

		close(store.release)
		waitForHandler(t, logs, 1)
		if failed, past := srv.Counters(); failed != 0 || past != 0 {
			t.Errorf("counters = failed %d, past deadline %d; want 0 and 0", failed, past)
		}
		if got := logs.levels(t); len(got) != 1 || got[0] != "INFO api: the client went away before the answer" {
			t.Errorf("log = %q, want one INFO line - the client left, the service did not fail", got)
		}
	})
}

// waitForHandler waits until the log holds n lines, and then a moment
// more. TimeoutHandler returns as soon as it has answered, while the
// handler it abandoned is still finishing on its own goroutine; what the
// handler logs arrives after ServeHTTP has returned, and the moment is
// for a line that should not exist to have the time to appear.
func waitForHandler(t *testing.T, logs *safeBuffer, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(logs.levels(t)) >= n {
			time.Sleep(100 * time.Millisecond)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the log has %d lines after 2s, want %d: %q", len(logs.levels(t)), n, logs.levels(t))
}

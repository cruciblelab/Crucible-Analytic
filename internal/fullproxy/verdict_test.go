package fullproxy

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
	"github.com/cruciblelab/crucible-analytic/internal/verdict"
)

// headerBackend records the verdict header of every request it receives,
// by path.
type headerBackend struct {
	mu   sync.Mutex
	seen map[string]string
}

func (b *headerBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen[r.Method+" "+r.URL.Path] = r.Header.Get(verdict.Header)
}

func (b *headerBackend) header(t *testing.T, key string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	h, ok := b.seen[key]
	if !ok {
		t.Fatalf("the backend never saw %s", key)
	}
	return h
}

// scoreCall is one call the proxy made to Score.
type scoreCall struct {
	ip    netip.Addr
	stats ratestore.WindowStats
	ja4   string
}

func startVerdictProxy(t *testing.T, key []byte) (addr string, backend *headerBackend, calls func() []scoreCall) {
	t.Helper()
	backend = &headerBackend{seen: map[string]string{}}
	be := &http.Server{Handler: backend}
	beLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = be.Serve(beLn) }()
	t.Cleanup(func() { _ = be.Close() })

	var mu sync.Mutex
	var got []scoreCall
	certFile, keyFile := writeCertKeyFiles(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		BackendAddr: beLn.Addr().String(),
		CertFile:    certFile,
		KeyFile:     keyFile,
		Store:       ratestore.NewMemoryRateStore(10*time.Second, time.Minute, time.Minute),
		DialTimeout: 2 * time.Second,
		VerdictKey:  key,
		// The score is the call number, so a header can be traced back to
		// the call that produced it.
		Score: func(ip netip.Addr, stats ratestore.WindowStats, ja4 string) int {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, scoreCall{ip, stats, ja4})
			return 60 + len(got)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ln.Addr().String(), backend, func() []scoreCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]scoreCall(nil), got...)
	}
}

func send(t *testing.T, c *http.Client, method, url, forged string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if forged != "" {
		req.Header.Set(verdict.Header, forged)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// The visitor's own header never reaches the backend, on any method; a
// POST arrives carrying the collector's, signed for the visitor's address
// with the score the collector gave this request - the store's stats for
// it, not zero.
func TestTheProxySignsItsVerdictAndNeverForwardsTheVisitors(t *testing.T) {
	key := verdict.Key(bytes.Repeat([]byte("k"), privacy.MinHashKeyLen))
	addr, backend, calls := startVerdictProxy(t, key)
	c := newInsecureClient()
	// A header the collector really wrote, for this visitor, would still
	// be the visitor's to send: the collector deletes, it does not judge.
	forged := verdict.Sign(key, 0, time.Now(), netip.MustParseAddr("127.0.0.1"))

	send(t, c, http.MethodGet, "https://"+addr+"/sayfa", forged)
	send(t, c, http.MethodPost, "https://"+addr+"/_ca/event", forged)
	send(t, c, http.MethodPost, "https://"+addr+"/_ca/event2", "")

	if h := backend.header(t, "GET /sayfa"); h != "" {
		t.Errorf("a GET reached the backend with %q - the visitor's header was forwarded", h)
	}
	made := calls()
	if len(made) != 2 {
		t.Fatalf("Score was called %d times, want 2 (once per POST, never for the GET)", len(made))
	}
	for i, path := range []string{"POST /_ca/event", "POST /_ca/event2"} {
		h := backend.header(t, path)
		if h == forged {
			t.Fatalf("%s: the visitor's header reached the backend unchanged", path)
		}
		score, out := verdict.Check(key, h, netip.MustParseAddr("127.0.0.1"), time.Now())
		if out != verdict.Verified || score != 61+i {
			t.Errorf("%s: header %q checks as (%d, %v), want (%d, Verified)", path, h, score, out, 61+i)
		}
		if made[i].stats.CurrWindowCount != 2+i {
			t.Errorf("%s: scored with CurrWindowCount %d, want %d - the stats RecordRequest reported for this request",
				path, made[i].stats.CurrWindowCount, 2+i)
		}
	}
}

// No key, no verdict - and the visitor's header is removed all the same.
func TestWithoutAKeyTheProxyOnlyRemovesTheHeader(t *testing.T) {
	addr, backend, calls := startVerdictProxy(t, nil)
	send(t, newInsecureClient(), http.MethodPost, "https://"+addr+"/_ca/event", "v1 0 0 127.0.0.1 x")
	if h := backend.header(t, "POST /_ca/event"); h != "" {
		t.Errorf("the backend saw %q", h)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("Score was called %d times with no key", n)
	}
}

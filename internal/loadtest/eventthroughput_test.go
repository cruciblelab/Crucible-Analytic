//go:build loadtest

// This product beside another one, on the same machine, measured the
// same way.
//
// # Why this file exists
//
// The owner asked whether this is more performant than most analytics
// products. Y1 measured this product and answered the first half of the
// question; the second half was left explicitly unanswered, because a
// number measured here beside a number somebody else published is not a
// comparison. This file removes that excuse: it installs a real
// competitor, runs it on the same CPUs against the same PostgreSQL, and
// drives both with the same load generator and the same protocol.
//
// The competitor is Umami (github.com/umami-software/umami), MIT, one of
// the most widely self-hosted analytics products. Plausible needs
// ClickHouse and Matomo needs MySQL; neither is installable on this
// machine, and Umami runs on Node with PostgreSQL, which is here. Its
// version, commit and configuration are printed by the test so the
// numbers can never be quoted without them.
//
// # What is comparable, and what is not
//
// Umami is an event collector: a JS snippet POSTs a page view to
// /api/send and the server writes a row. The comparable half of this
// product is therefore the BEACON (internal/beacon), not the collector
// proxy. The proxy has no counterpart in Umami at all - Umami never
// sees a request unless JavaScript runs, so it cannot count a bot, a
// crawler or a visitor with scripting off. That is a feature difference,
// not a speed difference, and comparing the proxy's throughput against
// /api/send would be a category error.
//
// # The number is persisted rows, not accepted requests
//
// This is the whole fairness question and it decides the result.
//
// The beacon buffers rows in memory and writes them with COPY in
// batches; Umami writes inside the request. So "requests per second" is
// not the same work on the two sides: this product can accept a request
// by putting it in a queue, and past a full queue it DROPS (see
// internal/beacon.Writer, which is explicit about the trade). Reporting
// accepted requests would credit this product for work it had not done.
//
// So the headline figure is rows that reached PostgreSQL, counted with
// the same instrument on both sides: count before the drive, drive,
// wait until the count stops moving, count again. Accepted requests are
// reported next to it, because the gap between the two is a real
// property of each design and hiding it would be its own dishonesty.
//
// # The rig
//
// Same as Y1's, extended by one part: PostgreSQL is pinned to the
// server's CPUs as well, so the database is inside the machine being
// measured rather than beside it. Both products talk to the same
// postmaster.
//
//   - app pinned to CPUs 0..n-1, PostgreSQL pinned to the same set
//   - load generator on the remaining CPUs, at every size that fits
//   - capacity is the best any generator size got, reported as "at least"
//   - latency at one connection, where nothing queues anywhere
//   - every pinning read back from /proc and failed on if wrong
//
// # Configuration: each product as it installs
//
// Neither side is tuned. Both run their default configuration, and the
// differences that follow from that are named in the output rather than
// smoothed over:
//
//   - Umami's build downloads MaxMind GeoLite2-City and resolves a city
//     per event. This product's default install profile ("hafif") loads
//     no range tables, so it resolves no country - the operator turns
//     that on with --profile. The cost of that difference is not
//     measured here; it is named.
//   - Umami trusts X-Forwarded-For unconditionally. This product trusts
//     it only from named proxies, so the rig names 127.0.0.0/8 - which
//     is the real deployment (behind nginx on localhost) and makes both
//     sides do the same per-visitor work.
//   - Umami answers /api/send with a signed cache token which its
//     tracker then replays, so a returning visitor's request skips a
//     session lookup. Both paths are measured, because picking one
//     silently would hide a factor in the number.
//
// Run it:
//
//	CA_EVENT_THROUGHPUT=1 \
//	CA_SUPERUSER_DSN=postgres://postgres@127.0.0.1:5432/analytics \
//	CA_EVENT_UMAMI_DIR=/var/tmp/bench/umami/.next/standalone \
//	CA_EVENT_UMAMI_DSN=postgres://umami:umami@127.0.0.1:5432/umami \
//	CA_EVENT_UMAMI_WEBSITE=<uuid> CA_EVENT_UMAMI_NODE=/opt/node22/bin/node \
//	go test -tags loadtest ./internal/loadtest/ -run Comparison -v -timeout 60m
//
// Without the Umami variables it measures this product alone and says
// so. It never pretends a competitor was measured.
package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
)

const (
	// benchSite is the site_id this test writes under, so its rows can be
	// removed without touching anything else in the shared database.
	benchSite = "loadtest-comparison"
	// browserUA is a real browser's user agent. Both products classify
	// user agents and Umami discards bots outright, so a Go client's
	// default UA would have measured the rejection path on one side and
	// the accept path on the other.
	browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

// eventTarget is one thing that can be driven and counted.
type eventTarget struct {
	label string
	url   string
	// body builds the payload for client number i, which varies the
	// visitor so that neither product is measured on a single hot
	// session row.
	body func(i int) []byte
	// header adds per-client headers, including the claimed address.
	header func(i int, h http.Header)
	// persisted reports how many events this product has stored.
	persisted func() (int64, error)
}

// eventResult is one drive of one target.
type eventResult struct {
	label    string
	clients  int
	accepted int64
	rows     int64
	dur      time.Duration
	settle   time.Duration
	p50, p99 time.Duration
	errors   int64
	statuses map[int]int64
}

func (r eventResult) acceptedPerSecond() float64 {
	return float64(r.accepted) / r.dur.Seconds()
}

func (r eventResult) rowsPerSecond() float64 {
	return float64(r.rows) / r.dur.Seconds()
}

// settleUntilQuiet waits for a product's writes to finish landing.
//
// Polling until the count stops moving rather than sleeping a fixed
// time, because the two designs need different waits and a wait chosen
// for one of them would be an unfair instrument for the other: this
// product may hold ten thousand rows in a buffer, Umami holds none.
func settleUntilQuiet(t *testing.T, count func() (int64, error)) (int64, time.Duration) {
	t.Helper()
	began := time.Now()
	last, err := count()
	if err != nil {
		t.Fatalf("counting persisted rows: %v", err)
	}
	quiet := 0
	for time.Since(began) < 60*time.Second {
		time.Sleep(250 * time.Millisecond)
		now, err := count()
		if err != nil {
			t.Fatalf("counting persisted rows: %v", err)
		}
		if now == last {
			if quiet++; quiet >= 3 {
				return now, time.Since(began)
			}
		} else {
			quiet = 0
		}
		last = now
	}
	t.Fatalf("rows never stopped arriving after 60s (last %d); "+
		"a product that never catches up has no throughput figure", last)
	return 0, 0
}

// driveEvents posts events from p.clients connections and reports both
// what was accepted and what was stored.
//
// The stored figure covers the whole drive, warm-up included, and is
// divided by the whole drive's duration. That is deliberate: trying to
// isolate a window inside a buffered pipeline means guessing which
// enqueued rows belonged to it, and a guess in the numerator is worth
// less than a second of warm-up in the denominator.
func driveEvents(t *testing.T, target eventTarget, p plan) eventResult {
	t.Helper()

	before, _ := settleUntilQuiet(t, target.persisted)

	var (
		accepted atomic.Int64
		errors   atomic.Int64
		mu       sync.Mutex
		samples  []time.Duration
		statuses = map[int]int64{}
	)
	stop := make(chan struct{})

	began := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < p.clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := &http.Client{
				Transport: &http.Transport{
					MaxIdleConns:        4,
					MaxIdleConnsPerHost: 4,
					DisableCompression:  true,
				},
				Timeout: 30 * time.Second,
			}
			defer c.CloseIdleConnections()
			local := make([]time.Duration, 0, 8192)
			localStatus := map[int]int64{}
			payload := target.body(i)
			for {
				select {
				case <-stop:
					mu.Lock()
					samples = append(samples, local...)
					for k, v := range localStatus {
						statuses[k] += v
					}
					mu.Unlock()
					return
				default:
				}
				req, err := http.NewRequest(http.MethodPost, target.url,
					bytes.NewReader(payload))
				if err != nil {
					errors.Add(1)
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("User-Agent", browserUA)
				target.header(i, req.Header)

				at := time.Now()
				resp, err := c.Do(req)
				if err != nil {
					errors.Add(1)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				took := time.Since(at)
				localStatus[resp.StatusCode]++
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					accepted.Add(1)
					local = append(local, took)
				} else {
					errors.Add(1)
				}
			}
		}(i)
	}

	time.Sleep(p.warm + p.load)
	dur := time.Since(began)
	close(stop)
	wg.Wait()

	after, settle := settleUntilQuiet(t, target.persisted)

	mu.Lock()
	defer mu.Unlock()
	sortDurations(samples)
	out := eventResult{
		label: target.label, clients: p.clients,
		accepted: accepted.Load(), rows: after - before,
		dur: dur, settle: settle, errors: errors.Load(), statuses: statuses,
	}
	if n := len(samples); n > 0 {
		out.p50 = samples[n/2]
		out.p99 = samples[(n*99)/100]
	}
	return out
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

// crucibleTarget POSTs page views at this product's beacon.
func crucibleTarget(base string, count func() (int64, error)) eventTarget {
	return eventTarget{
		label: "Crucible beacon",
		url:   base + beacon.DefaultPathPrefix + "/event",
		body: func(i int) []byte {
			b, _ := json.Marshal(map[string]any{
				"site": benchSite, "type": beacon.TypePageview,
				"url": fmt.Sprintf("/p/%d", i%17), "referrer": "",
				"title": "Bench", "screen_w": 1920, "screen_h": 1080,
				"language": "en-US",
			})
			return b
		},
		header: func(i int, h http.Header) {
			h.Set("X-Forwarded-For", claimedIP(i))
		},
		persisted: count,
	}
}

// umamiTarget POSTs page views at Umami's /api/send.
//
// cached decides whether the request carries the x-umami-cache token its
// tracker replays after the first response. Both are measured.
func umamiTarget(base, website string, cached bool, tokens []string,
	count func() (int64, error)) eventTarget {
	label := "Umami /api/send"
	if cached {
		label += " (jetonlu)"
	}
	return eventTarget{
		label: label,
		url:   base + "/api/send",
		body: func(i int) []byte {
			b, _ := json.Marshal(map[string]any{
				"type": "event",
				"payload": map[string]any{
					"website": website, "hostname": "bench.local",
					"screen": "1920x1080", "language": "en-US",
					"title": "Bench", "url": fmt.Sprintf("/p/%d", i%17),
					"referrer": "",
				},
			})
			return b
		},
		header: func(i int, h http.Header) {
			h.Set("X-Forwarded-For", claimedIP(i))
			if cached && i < len(tokens) && tokens[i] != "" {
				h.Set("x-umami-cache", tokens[i])
			}
		},
		persisted: count,
	}
}

// claimedIP gives client i its own address, so both products do
// per-visitor work rather than hammering one session row.
func claimedIP(i int) string {
	return fmt.Sprintf("203.0.%d.%d", (i/250)%250+1, i%250+1)
}

// counterFor returns a row counter over one table.
func counterFor(pool *pgxpool.Pool, query string) func() (int64, error) {
	return func() (int64, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var n int64
		err := pool.QueryRow(ctx, query).Scan(&n)
		return n, err
	}
}

// beaconServeMode is the child process holding this product's beacon.
func beaconServeMode(cpus string) {
	die := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "beacon child: %s: %v\n", what, err)
		os.Exit(1)
	}
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	writer, err := beacon.NewWriter(ctx, os.Getenv("CA_EVENT_BEACON_DSN"),
		beacon.WriterConfig{Logger: quiet})
	if err != nil {
		die("open writer", err)
	}
	go writer.Run(ctx)

	srv := &beacon.Server{
		Sites: []string{benchSite},
		Sink:  writer,
		// The real deployment: behind a local reverse proxy, so
		// forwarded addresses from loopback are believed. Without this
		// every visitor would be 127.0.0.1 and this side would be doing
		// less per-visitor work than the side it is compared against.
		ClientIP: beacon.ClientIPResolver{
			TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		},
		IPMode: privacy.IPMasked,
		Logger: quiet,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("listen", err)
	}
	go func() { _ = http.Serve(ln, srv.Handler()) }()

	fmt.Printf("READY procs=%d cpus=%s beacon=%s\n",
		runtime.GOMAXPROCS(0), cpus, ln.Addr().String())
	select {}
}

// pinProcess restricts an already-running process to cpus, and reads the
// result back out of /proc rather than trusting the call.
func pinProcess(t *testing.T, what string, pid int, cpus string) {
	t.Helper()
	out, err := exec.Command("taskset", "-a", "-p", "-c", cpus,
		strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		t.Fatalf("pinning %s (pid %d) to %s: %v: %s", what, pid, cpus, err, out)
	}
	verifyAffinity(t, what, pid, cpus)
}

// verifyAffinity fails unless the kernel agrees about where a process may
// run.
//
// Separate from pinProcess because a child started under taskset was
// never asked to move, and a pinning nobody checked is a pinning that
// may not have happened - which fails by quietly reporting a bigger
// machine than the one named in the output.
func verifyAffinity(t *testing.T, what string, pid int, cpus string) {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatalf("reading %s affinity back: %v", what, err)
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(ln, "Cpus_allowed_list:"); ok {
			if got := strings.TrimSpace(v); got != cpus {
				t.Fatalf("%s: asked for CPUs %s, kernel says %s", what, cpus, got)
			}
			return
		}
	}
	t.Fatalf("%s: /proc/%d/status has no Cpus_allowed_list", what, pid)
}

// postmasterPID asks the database where it lives and reads its pid file,
// so the process being pinned is the one actually serving these queries
// and not a number typed into a script.
func postmasterPID(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var dir string
	if err := pool.QueryRow(ctx,
		"select setting from pg_settings where name = 'data_directory'").
		Scan(&dir); err != nil {
		t.Fatalf("asking PostgreSQL for its data directory: %v", err)
	}
	b, err := os.ReadFile(dir + "/postmaster.pid")
	if err != nil {
		t.Fatalf("reading %s/postmaster.pid: %v", dir, err)
	}
	first := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0]
	pid, err := strconv.Atoi(first)
	if err != nil {
		t.Fatalf("postmaster.pid holds %q: %v", first, err)
	}
	return pid
}

// warmUntilStable drives a target until warming has stopped changing the
// answer.
//
// A fixed warm-up is a guess, and the first version's guess was wrong in
// a way that would have decided this comparison. Next.js compiles a
// route on its first request: three seconds of load measured Umami's
// compiler and reported 8 rows/s for a product that does far more, while
// this product - compiled ahead of time - was at full speed in its first
// second. Warming until two consecutive bursts agree is a statement
// about the process rather than about how long I felt like waiting.
//
// The same trap caught a hand measurement on the way here: a public
// address looked 3.1 times more expensive than a private one, which
// would have been a finding about Umami's geo lookup. Warmed, the
// difference vanished - it was the first-request compile, and the geo
// path costs 0.064 ms (canBindToIp 0.054, MaxMind 0.010), measured
// separately.
func warmUntilStable(t *testing.T, target eventTarget) {
	t.Helper()
	const (
		bursts    = 8
		tolerance = 0.15
	)
	prev := 0.0
	for i := 0; i < bursts; i++ {
		r := driveEvents(t, target, plan{clients: 8, load: 3 * time.Second})
		rate := r.rowsPerSecond()
		t.Logf("isinma %-26s tur %d  %8.0f satir/s", target.label, i+1, rate)
		if prev > 0 && rate > 0 {
			if d := rate/prev - 1; d > -tolerance && d < tolerance {
				return
			}
		}
		prev = rate
	}
	t.Fatalf("%s never settled after %d warm-up bursts; a product whose rate "+
		"keeps moving has no steady-state figure to report", target.label, bursts)
}

// umamiVersionAndMode reports which Umami this is and refuses to measure
// one that would not be running in production mode.
//
// The version is printed because a throughput number without one is not
// a fact about anything. The mode is checked because a competitor
// measured in development mode is not a competitor measured: Next.js
// compiles on demand there, and a comparison against that would be a
// comparison against a build step. Next's standalone entry point pins
// NODE_ENV itself, and this reads that line back rather than trusting
// the claim.
func umamiVersionAndMode(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("reading Umami's package.json: %v", err)
	}
	var pkg struct {
		Name    string
		Version string
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatalf("parsing Umami's package.json: %v", err)
	}
	server, err := os.ReadFile(filepath.Join(dir, "server.js"))
	if err != nil {
		t.Fatalf("reading Umami's server entry point: %v", err)
	}
	if !strings.Contains(string(server), `process.env.NODE_ENV = 'production'`) {
		t.Fatalf("%s/server.js does not pin NODE_ENV to production; a competitor "+
			"measured in development mode is not a competitor measured", dir)
	}
	return fmt.Sprintf("%s v%s (production)", pkg.Name, pkg.Version)
}

// startPinnedUmami runs Umami's own production entry point on cpus and
// waits for it to answer, returning its base URL.
//
// Its configuration is Umami's default: the standalone build the
// project's own Dockerfile ships, the DATABASE_URL it documents, and
// nothing tuned. The port is chosen by the kernel so two runs cannot
// collide.
func startPinnedUmami(t *testing.T, cpus, node, dir, dsn string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cmd := exec.Command("taskset", "-c", cpus, node, "server.js")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PORT="+strconv.Itoa(port),
		"HOSTNAME=127.0.0.1",
		"DATABASE_URL="+dsn,
		"APP_SECRET=loadtest-comparison-secret",
		"DISABLE_TELEMETRY=1")
	log, err := os.CreateTemp("", "umami-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting Umami: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Close()
		os.Remove(log.Name())
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/api/heartbeat")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				// Checked here rather than straight after Start: taskset
				// sets the mask and then execs, so a process asked about
				// too early still carries the affinity it inherited. The
				// process that answered is the process to check.
				verifyAffinity(t, "Umami", cmd.Process.Pid, cpus)
				return base
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	b, _ := os.ReadFile(log.Name())
	t.Fatalf("Umami never answered on %s within 90s:\n%s", base, b)
	return ""
}

// warmUmamiTokens collects one cache token per client, the way the real
// tracker gets them: from a first, uncached response.
func warmUmamiTokens(t *testing.T, base, website string, n int) []string {
	t.Helper()
	tokens := make([]string, n)
	c := &http.Client{Timeout: 30 * time.Second}
	target := umamiTarget(base, website, false, nil, nil)
	for i := 0; i < n; i++ {
		req, err := http.NewRequest(http.MethodPost, target.url,
			bytes.NewReader(target.body(i)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", browserUA)
		target.header(i, req.Header)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("warming Umami token %d: %v", i, err)
		}
		var body struct{ Cache string }
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if body.Cache == "" {
			t.Fatalf("Umami returned no cache token for client %d (status %d); "+
				"the cached path cannot be measured without one", i, resp.StatusCode)
		}
		tokens[i] = body.Cache
	}
	return tokens
}

func mustBeAnEventMeasurement(t *testing.T, r eventResult) {
	t.Helper()
	if r.accepted == 0 {
		t.Fatalf("%s accepted nothing at %d connections (statuses %v)",
			r.label, r.clients, r.statuses)
	}
	if r.errors*100 > r.accepted {
		t.Fatalf("%s at %d connections: %d failures against %d accepted "+
			"(statuses %v); a run this broken is not a measurement",
			r.label, r.clients, r.errors, r.accepted, r.statuses)
	}
	if r.rows == 0 {
		t.Fatalf("%s accepted %d requests and stored NO rows; "+
			"an endpoint that persists nothing has no throughput to report",
			r.label, r.accepted)
	}
}

// TestComparisonOfEventIngest is the comparison.
func TestComparisonOfEventIngest(t *testing.T) {
	if cpus := os.Getenv("CA_EVENT_SERVE"); cpus != "" {
		beaconServeMode(cpus)
		return
	}
	if os.Getenv("CA_EVENT_THROUGHPUT") == "" {
		t.Skip("set CA_EVENT_THROUGHPUT=1; this takes tens of minutes and wants the machine quiet")
	}

	total := runtime.NumCPU()
	sizes := serverSizes(total)
	if len(sizes) == 0 {
		t.Skipf("%d CPUs is not enough to put the load generator somewhere else", total)
	}

	adminDSN := os.Getenv("CA_SUPERUSER_DSN")
	if adminDSN == "" {
		t.Skip("set CA_SUPERUSER_DSN; the row counts are read with it")
	}
	// The beacon connects as the role it connects as in production, not
	// as the superuser holding the tape measure. A benchmark run as a
	// superuser would be measuring a deployment nobody has.
	//
	// The DSN is spelled out here rather than taken from internal/testdb
	// because that package is behind the integration tag, and importing
	// it would stop this file compiling in the nightly, which builds
	// this package with -tags loadtest alone. The convention it follows
	// is testdb's: each development role's password is its own name.
	beaconDSN := os.Getenv("CA_DSN_beacon_writer")
	if beaconDSN == "" {
		beaconDSN = "postgres://beacon_writer:beacon_writer@localhost:5432/analytics"
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	// t.Cleanup rather than defer, and registered before the wipe:
	// cleanups run last-registered-first, and a wipe against a closed
	// pool leaves this test's rows in a shared database.
	t.Cleanup(admin.Close)
	if err := admin.Ping(ctx); err != nil {
		t.Fatalf("ping as admin: %v", err)
	}

	// Start from no rows of our own, and leave none behind: this runs
	// against the shared development database.
	wipe := func() {
		if _, err := admin.Exec(ctx,
			"delete from beacon_events where site_id = $1", benchSite); err != nil {
			t.Fatalf("clearing this test's rows: %v", err)
		}
	}
	wipe()
	t.Cleanup(wipe)

	pgPID := postmasterPID(t, admin)
	t.Logf("PostgreSQL postmaster pid %d", pgPID)

	crucibleCount := counterFor(admin,
		"select count(*) from beacon_events where site_id = '"+benchSite+"'")

	// Umami, if this machine has one.
	//
	// The test owns its lifecycle rather than attaching to a server
	// somebody started by hand. Two reasons, and the first one is not
	// tidiness: a process started from a shell that has since exited is
	// a process that can vanish mid-measurement, and this one did. The
	// second is that starting both servers the same way - a child under
	// taskset - is what makes "the same rig" true rather than claimed.
	var (
		umamiDir     = os.Getenv("CA_EVENT_UMAMI_DIR")
		umamiDSN     = os.Getenv("CA_EVENT_UMAMI_DSN")
		umamiWebsite = os.Getenv("CA_EVENT_UMAMI_WEBSITE")
		umamiNode    = os.Getenv("CA_EVENT_UMAMI_NODE")
		umamiPool    *pgxpool.Pool
		umamiCount   func() (int64, error)
	)
	if umamiNode == "" {
		umamiNode = "node"
	}
	haveUmami := umamiDir != "" && umamiDSN != "" && umamiWebsite != ""
	if !haveUmami {
		t.Log("Umami NOT measured: CA_EVENT_UMAMI_{DIR,DSN,WEBSITE} not all set. " +
			"The comparison half of this file is not being produced.")
	} else {
		umamiPool, err = pgxpool.New(ctx, umamiDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(umamiPool.Close)
		if err := umamiPool.Ping(ctx); err != nil {
			t.Fatalf("ping Umami's database: %v", err)
		}
		if _, err := umamiPool.Exec(ctx, "delete from website_event"); err != nil {
			t.Fatalf("clearing Umami's events: %v", err)
		}
		umamiCount = counterFor(umamiPool, "select count(*) from website_event")
		t.Logf("%s from %s, website %s",
			umamiVersionAndMode(t, umamiDir), umamiDir, umamiWebsite)
	}

	everyCPU := cpuSet(0, total)
	t.Cleanup(func() { pinProcess(t, "PostgreSQL", pgPID, everyCPU) })

	for _, cores := range sizes {
		t.Run(fmt.Sprintf("sunucu-%d-cekirdek", cores), func(t *testing.T) {
			serverCPUs := cpuSet(0, cores)
			srv := startPinnedChild(t, serverCPUs, "^TestComparisonOfEventIngest$",
				[]string{
					"CA_EVENT_SERVE=" + serverCPUs,
					"CA_EVENT_BEACON_DSN=" + beaconDSN,
				})
			if srv.procs != cores {
				t.Fatalf("beacon was pinned to %d CPUs but reports GOMAXPROCS=%d",
					cores, srv.procs)
			}
			// The database is part of the server, not a neighbour of it.
			pinProcess(t, "PostgreSQL", pgPID, serverCPUs)
			umamiURL := ""
			if haveUmami {
				umamiURL = startPinnedUmami(t, serverCPUs, umamiNode, umamiDir, umamiDSN)
			}

			gens := generatorSets(cores, total)
			t.Cleanup(func() { pinSelf(t, everyCPU, total) })
			t.Logf("sunucu + PostgreSQL CPU %s, yuk ureteci %s",
				serverCPUs, gens[0].cpus)

			targets := []eventTarget{
				crucibleTarget("http://"+srv.addrs["beacon"], crucibleCount),
			}
			if haveUmami {
				targets = append(targets,
					umamiTarget(umamiURL, umamiWebsite, false, nil, umamiCount))
				tokens := warmUmamiTokens(t, umamiURL, umamiWebsite,
					concurrencyLadder[len(concurrencyLadder)-1])
				targets = append(targets,
					umamiTarget(umamiURL, umamiWebsite, true, tokens, umamiCount))
			}

			// Every target is warmed until warming stops changing the
			// answer, before anything is recorded.
			pinSelf(t, gens[0].cpus, gens[0].size)
			for _, target := range targets {
				warmUntilStable(t, target)
			}

			// The curve, on the metric being reported.
			peak := make([]int, len(targets))
			retained := make([]float64, len(targets))
			for i, target := range targets {
				best, atPeak, atTop := 0.0, 0.0, 0.0
				for _, n := range concurrencyLadder {
					r := driveEvents(t, target, probe(n))
					mustBeAnEventMeasurement(t, r)
					t.Logf("egri  %-26s %4d baglanti  %7.0f satir/s  "+
						"%7.0f kabul/s  p50 %9s  bekleme %s",
						target.label, n, r.rowsPerSecond(), r.acceptedPerSecond(),
						r.p50.Round(time.Microsecond), r.settle.Round(time.Millisecond))
					if r.rowsPerSecond() > best {
						best, peak[i], atPeak = r.rowsPerSecond(), n, r.rowsPerSecond()
					}
					atTop = r.rowsPerSecond()
				}
				retained[i] = atTop / atPeak
				t.Logf("%-26s tepe %d baglantida; %d baglantida tepenin %%%.0f'i",
					target.label, peak[i],
					concurrencyLadder[len(concurrencyLadder)-1], retained[i]*100)
			}

			// Capacity: best over generator sizes, reported as a floor.
			best := make([]eventResult, len(targets))
			bestGen := make([]string, len(targets))
			// Tracked as a plain number, not read back off best[i]: a
			// zero-value result has no duration, so its rate is NaN, and
			// every comparison against NaN is false. The first version
			// of this loop therefore never assigned anything and
			// reported NaN for every capacity in the run.
			bestRate := make([]float64, len(targets))
			for i := range bestRate {
				bestRate[i] = -1
			}
			for _, g := range gens {
				pinSelf(t, g.cpus, g.size)
				for i, target := range targets {
					for round := 0; round < 2; round++ {
						r := driveEvents(t, target, steady(peak[i]))
						mustBeAnEventMeasurement(t, r)
						if rate := r.rowsPerSecond(); rate > bestRate[i] {
							best[i], bestRate[i], bestGen[i] = r, rate, g.cpus
						}
					}
					t.Logf("uretec %-5s %-26s %4d baglanti  en iyi %7.0f satir/s",
						g.cpus, target.label, peak[i], bestRate[i])
				}
			}

			// Latency at one connection.
			pinSelf(t, gens[0].cpus, gens[0].size)
			lone := make([]eventResult, len(targets))
			for i, target := range targets {
				lone[i] = driveEvents(t, target, steady(1))
				mustBeAnEventMeasurement(t, lone[i])
			}

			for i, r := range best {
				t.Logf("KAPASITE %-26s en az %7.0f satir/s (%7.0f kabul/s, "+
					"%4d baglanti, uretec %s), %d baglantida tepenin %%%.0f'i",
					r.label, r.rowsPerSecond(), r.acceptedPerSecond(),
					r.clients, bestGen[i],
					concurrencyLadder[len(concurrencyLadder)-1], retained[i]*100)
				if gap := r.acceptedPerSecond() / r.rowsPerSecond(); gap > 1.1 {
					t.Logf("KAPASITE %-26s !! kabul ettigi, sakladiginin %.2f katı: "+
						"fazlası tamponda bekledi ya da düştü", r.label, gap)
				}
			}
			for i, r := range lone {
				t.Logf("GECIKME  %-26s tek baglanti  p50 %9s  p99 %9s  %5.0f satir/s",
					r.label, r.p50.Round(time.Microsecond),
					r.p99.Round(time.Microsecond), r.rowsPerSecond())
				_ = i
			}
		})
	}
}

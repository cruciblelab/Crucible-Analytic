//go:build loadtest

// What this product costs per request, in numbers.
//
// # Why this did not exist until it was asked for
//
// The owner asked whether it is performant and whether a small server
// holds up under high request rates. There was no answer.
// internal/loadtest proves the limiter is *correct* under dozens of
// concurrent connections, and PLAN.md's "gerçek eşzamanlı yük testi
// yapıldı" means exactly that and nothing more. No throughput figure had
// ever been measured, and the read side's numbers (the O group) say
// nothing about the path a visitor's request takes.
//
// So this measures the number that decides whether a small server
// survives: what the collector adds to a request that the customer's own
// server would have answered anyway. Every visitor request goes through
// it.
//
// # How the TLS confound is removed
//
// fullproxy terminates TLS, so a naive "proxy versus nothing" comparison
// would report Go's RSA handshake as this product's cost. Both sides of
// the comparison therefore speak TLS with the same certificate: the
// baseline is a TLS backend the client talks to directly, and every
// measured mode ends at a backend with the same handler. What is left in
// the difference is admission, the rate store and one extra hop.
//
// Both collector modes are measured, because they cost different things:
//
//   - passthrough (internal/proxy) forwards the TLS bytes untouched
//     after fingerprinting the ClientHello. The backend keeps its own
//     TLS.
//   - full (internal/fullproxy) terminates TLS itself and speaks
//     plaintext HTTP to the backend, so it sees requests rather than
//     connections.
//
// # The load generator must not share the server's cores - measured
//
// The first version of this set GOMAXPROCS in-process, so the sixty-four
// client goroutines, the proxy and the backend all ran inside the same
// budget. It reported a throughput ratio of 0.27-0.33, and that number
// was not the proxy's cost: it was the cost of putting three parties on
// one core instead of two. In a deployment the clients are the internet.
//
// This project's own rule, written down before this test existed: a load
// generator that starves the thing it measures is measuring its own
// load.
//
// So the server runs in a child process pinned with taskset to a fixed
// set of CPUs, and the load runs in this one on the rest. That is what a
// small server actually is - a machine with few cores, driven from
// outside.
//
// Two things about that pinning are verified rather than assumed. The
// child reports its own GOMAXPROCS and the parent fails if it is not the
// number of CPUs it was pinned to; the parent reads its own affinity
// back out of /proc. A pinning nobody checked is a pinning that may not
// have happened, and it would fail by quietly reporting a bigger machine
// than the one named in the output.
//
// # Why the biggest core count is not measured
//
// Every CPU given to the server is one the load generator does not have.
// On a machine with n CPUs the largest honest server size is n-2: the
// generator needs cores of its own, and it needs one to spare so that
// the "was the generator the limit" question can be answered rather than
// assumed. See headroom below.
//
// # Capacity is the top of a curve, not a number at one concurrency - measured
//
// The version after the pinning drove 64 connections at everything, and
// its headroom check then reported the one-core full proxy 18.5% FASTER
// on a smaller load generator - twice, outside the repeats' spread, so
// not noise. Taking a CPU away from the client cannot make the server
// quicker. What it does is offer less load, and one core past its knee
// returns less goodput the harder it is pushed:
//
//	fullproxy, 1 core:  8 conns 14053 req/s p50   471µs
//	                   32 conns 10523 req/s p50  3167µs
//	                  128 conns 10142 req/s p50 12660µs
//
// So 64 was not a capacity, it was a point on the far side of the knee,
// and the "faster with fewer client CPUs" reading was the curve seen
// sideways. Every mode is now swept up concurrencyLadder and its
// capacity read off its own peak, with the goodput retained at the top
// of the ladder reported next to it - that retention figure is the
// argument for the limiter being a setting rather than a constant.
//
// # Two operating points, because the two questions have different answers
//
// Reading capacity at each mode's own peak broke the latency comparison
// in a way worth keeping written down: the first version subtracted a
// p50 measured at 8 connections from one measured at 128 and printed
// "the proxy adds -1.174ms". A difference between two different offered
// loads is a difference in queueing, not in cost. Capacity is therefore
// measured at each mode's peak and added latency at one low concurrency
// shared by all of them.
//
// # Why this is not in the nightly
//
// The nightly runs this package with -race -count=3, and both of those
// are wrong for a throughput figure: the race detector changes the cost
// of the thing being measured, and three runs of a five-minute sweep is
// most of the nightly. A shared cloud runner would also be measuring
// itself. So this test skips unless CA_THROUGHPUT is set, and the
// numbers in NOTES.md name the machine they came from.
//
// # What is deliberately not measured
//
// Any comparison with another analytics product. Nobody has run one on
// this machine, and a number for this product beside a number somebody
// else published is not a comparison.
//
// HTTP/2. Both sides of a comparison have to speak the same protocol or
// the difference is the protocol's, and httptest's h2 wiring is not the
// same code path as fullproxy's. The numbers here are HTTP/1.1, which
// the driver verifies rather than assumes.
//
// Run it:
//
//	CA_THROUGHPUT=1 go test -tags loadtest ./internal/loadtest/ -run Throughput -v -timeout 25m
//
// It takes about six minutes and wants the machine otherwise quiet.
package loadtest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/fullproxy"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/proxy"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// loadFor is how long each measurement drives load; warmFor is what is
// thrown away first.
//
// A Go server's first requests pay for connection setup, map growth and
// the scheduler settling. Reporting those as steady-state throughput
// would understate the product on exactly the machine the question is
// about.
const (
	loadFor = 4 * time.Second
	warmFor = 2 * time.Second
)

// concurrencyLadder is the offered load each mode is swept over before
// anything is reported.
//
// Capacity is the top of a curve, not a number at whatever concurrency
// the test happened to use - see the package doc. The ladder has to
// reach past the peak or the peak cannot be recognised as one, which is
// why it ends well above what a single core can keep up with.
var concurrencyLadder = []int{1, 8, 16, 32, 64, 128}

// capacityRounds is how many times each mode is driven at each size of
// load generator; latencyRounds how many times the single-connection
// round trip is measured.
//
// More than two, for the reason written down elsewhere in this project
// after a worse mistake: two samples do not have a spread, they have a
// difference, and a difference cannot say whether a third sample would
// have sat between them or outside both. Capacity gets three per
// generator size only on the smallest machines - the generator sweep
// multiplies the cost - so it gets three where it matters and the
// spread is printed either way.
const (
	capacityRounds = 3
	latencyRounds  = 3
)

// wideSpread is where a figure stops being a number and starts being a
// range. A one-core full-proxy capacity has come in at 24.8% spread on
// this container, so the output has to say so rather than quote a
// middle.
const wideSpread = 0.15

type result struct {
	label    string
	clients  int
	requests int64
	dur      time.Duration
	p50, p99 time.Duration
	errors   int64
}

func (r result) perSecond() float64 { return float64(r.requests) / r.dur.Seconds() }

// summary is what several drives of the same thing amount to.
//
// The middle value is reported rather than the best or the mean: the
// best is the machine on its luckiest four seconds, and a mean of three
// hides which way the odd one went.
type summary struct {
	label             string
	low, median, high float64
	p50               time.Duration
}

// spread is how far the repeats disagreed, relative to the figure being
// reported. It is the tolerance the headroom question gets measured
// against, so that the answer does not depend on a number I chose.
func (s summary) spread() float64 {
	if s.median == 0 {
		return 0
	}
	return (s.high - s.low) / s.median
}

func summarize(label string, runs []result) summary {
	rates := make([]float64, 0, len(runs))
	lat := make([]time.Duration, 0, len(runs))
	for _, r := range runs {
		rates = append(rates, r.perSecond())
		lat = append(lat, r.p50)
	}
	sort.Float64s(rates)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return summary{
		label:  label,
		low:    rates[0],
		median: rates[len(rates)/2],
		high:   rates[len(rates)-1],
		p50:    lat[len(lat)/2],
	}
}

// plan is one drive: how many connections offer load, for how long, and
// how much of the beginning is discarded.
type plan struct {
	clients    int
	load, warm time.Duration
}

// probe is a short drive, for finding the shape of a curve; steady is a
// full-length one, for the figures that get reported.
func probe(clients int) plan {
	return plan{clients: clients, load: 2 * time.Second, warm: time.Second}
}

func steady(clients int) plan {
	return plan{clients: clients, load: loadFor, warm: warmFor}
}

// drive sends requests from p.clients concurrent connections and reports
// what got through after the warm-up.
func drive(t *testing.T, label, url string, p plan, tlsCfg *tls.Config) result {
	t.Helper()

	var (
		requests atomic.Int64
		errors   atomic.Int64
		mu       sync.Mutex
		samples  []time.Duration
	)
	stop := make(chan struct{})
	measuring := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < p.clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// One transport per goroutine: a shared client with a small
			// MaxIdleConnsPerHost would make this a measurement of
			// connection reuse rather than of the proxy.
			c := &http.Client{
				Transport: &http.Transport{
					TLSClientConfig:     tlsCfg,
					MaxIdleConns:        4,
					MaxIdleConnsPerHost: 4,
					DisableCompression:  true,
				},
				Timeout: 20 * time.Second,
			}
			defer c.CloseIdleConnections()
			local := make([]time.Duration, 0, 8192)
			for {
				select {
				case <-stop:
					mu.Lock()
					samples = append(samples, local...)
					mu.Unlock()
					return
				default:
				}
				began := time.Now()
				resp, err := c.Get(url)
				if err != nil {
					errors.Add(1)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				took := time.Since(began)
				select {
				case <-measuring:
					requests.Add(1)
					local = append(local, took)
				default:
				}
			}
		}()
	}

	time.Sleep(p.warm)
	close(measuring)
	began := time.Now()
	time.Sleep(p.load)
	dur := time.Since(began)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	out := result{label: label, clients: p.clients, requests: requests.Load(),
		dur: dur, errors: errors.Load()}
	if n := len(samples); n > 0 {
		out.p50 = samples[n/2]
		out.p99 = samples[(n*99)/100]
	}
	return out
}

// mustBeAMeasurement rejects a drive that cannot carry a number.
func mustBeAMeasurement(t *testing.T, r result) {
	t.Helper()
	if r.requests == 0 {
		t.Fatalf("%s answered nothing at %d connections, so nothing here is a measurement",
			r.label, r.clients)
	}
	if r.errors*100 > r.requests {
		t.Fatalf("%s at %d connections: %d failures against %d requests; a run "+
			"this broken is not a throughput measurement",
			r.label, r.clients, r.errors, r.requests)
	}
}

// checkReachableOverHTTP11 makes one request before any measurement.
//
// It answers two questions that a throughput number cannot: whether the
// address answers at all, and which protocol it answered with. A run
// where one mode negotiated HTTP/2 and another HTTP/1.1 would produce a
// difference belonging to the protocol and label it the proxy's.
func checkReachableOverHTTP11(t *testing.T, label, url string, tlsCfg *tls.Config) {
	t.Helper()
	c := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   10 * time.Second,
	}
	defer c.CloseIdleConnections()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("%s (%s): %v", label, url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s (%s): status %d", label, url, resp.StatusCode)
	}
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("%s (%s): answered over %s; every mode must speak the same "+
			"protocol or the difference between them is the protocol's",
			label, url, resp.Proto)
	}
}

func throughputCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(kind string, b []byte) []byte {
		var buf bytes.Buffer
		_ = pem.Encode(&buf, &pem.Block{Type: kind, Bytes: b})
		return buf.Bytes()
	}
	// Not t.TempDir: a child process outlives the subtest that started
	// it only by a moment, but the files have to exist for the whole of
	// the parent test, and the child is what reads them.
	dir, err := os.MkdirTemp("", "ca-tput-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, enc("CERTIFICATE", der), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, enc("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(priv)), 0o600); err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(leaf)
	return certFile, keyFile, pool
}

// serveMode is the child process: the whole server side of the
// measurement, on whichever CPUs taskset gave it.
//
// The test re-executes its own binary rather than building a separate
// one, so there is a single file to keep in step and no build step that
// could be skipped or go stale.
func serveMode(cpus string) {
	die := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "child: %s: %v\n", what, err)
		os.Exit(1)
	}
	certFile, keyFile := os.Getenv("CA_TPUT_CERT"), os.Getenv("CA_TPUT_KEY")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		die("load cert", err)
	}
	hello := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	// The backend a customer already runs, doing nothing.
	//
	// Nothing rather than something realistic, deliberately. A backend
	// that took 20ms would hide the proxy's cost inside its own, which
	// is comfortable and useless: the question is what this product
	// adds, and that is only visible when it is the only thing there.
	plain := httptest.NewServer(hello)
	tlsBackend := httptest.NewUnstartedServer(hello)
	tlsBackend.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		// http/1.1 only, on this side and on the client's, so ALPN
		// cannot make one mode faster than another by choosing a
		// different protocol for it.
		NextProtos: []string{"http/1.1"},
	}
	tlsBackend.StartTLS()

	// Logs discarded: the passthrough proxy writes a line per rejected
	// connection and the full one per request class, and a measurement
	// that also measures stderr is not a measurement of the product.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	newStore := func() ratestore.RateStore {
		return ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	}
	// Limits far above anything this machine can reach: the limiter's
	// correctness is internal/loadtest's other files' subject, and a
	// rejection here would turn throughput into a measurement of the
	// ceiling I typed.
	newLimiter := func() *limiter.Limiter {
		return limiter.New(limiter.Config{
			MaxConcurrentConnections: 1_000_000,
			MaxRequestsPerSecond:     1_000_000,
		})
	}
	listen := func() net.Listener {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			die("listen", err)
		}
		return ln
	}

	fullLn, passLn := listen(), listen()
	full := &fullproxy.Server{
		BackendAddr: plain.Listener.Addr().String(),
		CertFile:    certFile,
		KeyFile:     keyFile,
		Store:       newStore(),
		Limiter:     newLimiter(),
		DialTimeout: 5 * time.Second,
		Logger:      quiet,
	}
	pass := &proxy.Server{
		BackendAddr:      tlsBackend.Listener.Addr().String(),
		Store:            newStore(),
		Limiter:          newLimiter(),
		HandshakeTimeout: 5 * time.Second,
		DialTimeout:      5 * time.Second,
		Logger:           quiet,
	}
	go func() { _ = full.Serve(context.Background(), fullLn) }()
	go func() { _ = pass.Serve(context.Background(), passLn) }()

	// One line, so the parent needs no protocol beyond "wait for READY".
	// procs is in it because the parent has to check the pinning took:
	// Go sizes GOMAXPROCS from the affinity mask at startup, so this is
	// the child's own account of how big a machine it thinks it is on.
	fmt.Printf("READY procs=%d cpus=%s tlsdirect=%s fullproxy=%s passproxy=%s\n",
		runtime.GOMAXPROCS(0), cpus,
		tlsBackend.Listener.Addr().String(),
		fullLn.Addr().String(), passLn.Addr().String())
	select {}
}

// pinnedServer is a running child and the addresses it serves.
type pinnedServer struct {
	procs int
	addrs map[string]string
}

func startPinnedServer(t *testing.T, cpus, certFile, keyFile string) pinnedServer {
	t.Helper()
	return startPinnedChild(t, cpus, "^TestThroughputOfTheRequestPath$", []string{
		"CA_TPUT_SERVE=" + cpus,
		"CA_TPUT_CERT=" + certFile,
		"CA_TPUT_KEY=" + keyFile,
	})
}

// startPinnedChild re-executes this test binary under taskset, running
// only the named test, and waits for the READY line it prints.
//
// Shared by both measurements in this package so that "the server side
// on its own CPUs" means exactly one thing. A second copy of this would
// be a second chance for the two measurements to differ in the rig
// rather than in the product.
func startPinnedChild(t *testing.T, cpus, testName string, env []string) pinnedServer {
	t.Helper()
	cmd := exec.Command("taskset", "-c", cpus, os.Args[0],
		"-test.run", testName, "-test.timeout=0")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("taskset -c %s %s: %v", cpus, os.Args[0], err)
	}
	stopped := make(chan struct{})
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		<-stopped
	})

	type line struct {
		text string
		err  error
	}
	found := make(chan line, 1)
	go func() {
		defer close(stopped)
		sc := bufio.NewScanner(out)
		sent := false
		for sc.Scan() {
			// The child runs under the test framework, so its output
			// carries "=== RUN" as well. Only one line is a protocol.
			if !sent && strings.HasPrefix(sc.Text(), "READY ") {
				found <- line{text: sc.Text()}
				sent = true
			}
		}
		if !sent {
			found <- line{err: fmt.Errorf("child exited without READY")}
		}
	}()

	var got line
	select {
	case got = <-found:
	case <-time.After(60 * time.Second):
		t.Fatal("child never said READY")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}

	srv := pinnedServer{addrs: map[string]string{}}
	for _, field := range strings.Fields(strings.TrimPrefix(got.text, "READY ")) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			t.Fatalf("child said %q, which is not key=value", field)
		}
		if k == "procs" {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("child said procs=%q: %v", v, err)
			}
			srv.procs = n
			continue
		}
		srv.addrs[k] = v
	}
	return srv
}

// pinSelf restricts this process - the load generator - to cpus, and
// says so to the Go runtime, which read the affinity mask once at
// startup and would otherwise keep scheduling for the whole machine.
func pinSelf(t *testing.T, cpus string, n int) {
	t.Helper()
	out, err := exec.Command("taskset", "-a", "-p", "-c", cpus,
		strconv.Itoa(os.Getpid())).CombinedOutput()
	if err != nil {
		t.Fatalf("taskset -a -p -c %s: %v: %s", cpus, err, out)
	}
	runtime.GOMAXPROCS(n)
	if got := allowedCPUs(t); got != cpus {
		t.Fatalf("asked for CPUs %s, kernel says %s", cpus, got)
	}
}

// allowedCPUs reads this process's affinity back out of the kernel.
func allowedCPUs(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(ln, "Cpus_allowed_list:"); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("/proc/self/status has no Cpus_allowed_list")
	return ""
}

// cpuSet names CPUs [from,to) the way taskset spells them.
func cpuSet(from, to int) string {
	if to-from == 1 {
		return strconv.Itoa(from)
	}
	return fmt.Sprintf("%d-%d", from, to-1)
}

// serverSizes is how many CPUs the server may be given on a machine with
// total of them.
//
// The generator keeps at least two: one to drive with, and one more so
// that varying its size is an experiment rather than a hope.
func serverSizes(total int) []int {
	var out []int
	for n := 1; n <= total-2; n++ {
		out = append(out, n)
	}
	return out
}

// generatorSet is one size of load generator: how many CPUs it gets and
// which ones.
type generatorSet struct {
	size int
	cpus string
}

// generatorSets lists every size of load generator that fits beside a
// server of cores CPUs, largest first.
//
// Always the TOP CPUs, so that no size ever overlaps the server's, which
// occupy the bottom ones. Every size is measured because the rig turned
// out to change the answer: see the capacity comment in the test.
func generatorSets(cores, total int) []generatorSet {
	var out []generatorSet
	for size := total - cores; size >= 1; size-- {
		out = append(out, generatorSet{size: size, cpus: cpuSet(total-size, total)})
	}
	return out
}

// TestThroughputOfTheRequestPath is the measurement.
func TestThroughputOfTheRequestPath(t *testing.T) {
	if cpus := os.Getenv("CA_TPUT_SERVE"); cpus != "" {
		serveMode(cpus)
		return
	}
	if os.Getenv("CA_THROUGHPUT") == "" {
		t.Skip("set CA_THROUGHPUT=1; this takes minutes and wants the machine quiet")
	}

	total := runtime.NumCPU()
	sizes := serverSizes(total)
	if len(sizes) == 0 {
		t.Skipf("%d CPUs is not enough to put the load generator somewhere else", total)
	}
	certFile, keyFile, pool := throughputCert(t)
	clientTLS := &tls.Config{RootCAs: pool, NextProtos: []string{"http/1.1"}}
	everyCPU := cpuSet(0, total)

	for _, cores := range sizes {
		t.Run(fmt.Sprintf("sunucu-%d-cekirdek", cores), func(t *testing.T) {
			srv := startPinnedServer(t, cpuSet(0, cores), certFile, keyFile)
			if srv.procs != cores {
				t.Fatalf("server was pinned to %d CPUs but reports GOMAXPROCS=%d; "+
					"the numbers below would be a bigger machine's",
					cores, srv.procs)
			}
			driverCPUs := cpuSet(cores, total)
			t.Cleanup(func() { pinSelf(t, everyCPU, total) })
			pinSelf(t, driverCPUs, total-cores)
			t.Logf("sunucu CPU %s (GOMAXPROCS %d), yuk ureteci CPU %s",
				cpuSet(0, cores), srv.procs, driverCPUs)

			modes := []struct{ label, key string }{
				{"dogrudan arka uc (TLS)", "tlsdirect"},
				{"gecisli vekil (passthrough)", "passproxy"},
				{"tam vekil (fullproxy)", "fullproxy"},
			}
			for _, m := range modes {
				checkReachableOverHTTP11(t, m.label, "https://"+srv.addrs[m.key], clientTLS)
			}

			// First the curve, then the figures.
			//
			// Each mode is swept up the ladder so that its capacity is
			// read off the top of its own curve rather than at whatever
			// concurrency this test happened to pick. A mode compared at
			// someone else's peak would be reported as slower than it
			// is.
			peak := make([]int, len(modes))
			retained := make([]float64, len(modes))
			for i, m := range modes {
				url := "https://" + srv.addrs[m.key]
				best, atPeak := 0.0, 0.0
				var atLadderTop float64
				for _, n := range concurrencyLadder {
					r := drive(t, m.label, url, probe(n), clientTLS)
					mustBeAMeasurement(t, r)
					t.Logf("egri  %-28s %4d baglanti  %8.0f istek/s  p50 %9s  p99 %9s",
						m.label, n, r.perSecond(), r.p50.Round(time.Microsecond),
						r.p99.Round(time.Microsecond))
					if r.perSecond() > best {
						best, peak[i], atPeak = r.perSecond(), n, r.perSecond()
					}
					atLadderTop = r.perSecond()
				}
				// What overload costs, which is the whole reason the
				// limiter is a setting. A mode that keeps its goodput at
				// the top of the ladder reports 100%.
				retained[i] = atLadderTop / atPeak
				t.Logf("%-28s tepe %d baglantida; %d baglantida tepenin %%%.0f'i",
					m.label, peak[i], concurrencyLadder[len(concurrencyLadder)-1],
					retained[i]*100)
			}

			// Capacity: the best figure any size of load generator got
			// out of this server, and it is a lower bound.
			//
			// This is not tidiness, it is the only defensible reading of
			// what was measured. Asking the headroom question of every
			// mode instead of just the product's showed the generator
			// changing the answer by up to 32% at FIXED concurrency, in
			// both directions and differently per mode: three client
			// CPUs waking one server core cost that core more than two
			// did, and the baseline gained 4% going from one server core
			// to two, where a server-bound figure would have roughly
			// doubled. So no single rig configuration measures the
			// server; what every configuration agrees on is that the
			// server did at least the best of them.
			gens := generatorSets(cores, total)
			best := make([]summary, len(modes))
			bestGen := make([]string, len(modes))
			for _, g := range gens {
				pinSelf(t, g.cpus, g.size)
				runs := make([][]result, len(modes))
				for round := 0; round < capacityRounds; round++ {
					for i, m := range modes {
						r := drive(t, m.label, "https://"+srv.addrs[m.key],
							steady(peak[i]), clientTLS)
						mustBeAMeasurement(t, r)
						runs[i] = append(runs[i], r)
					}
				}
				for i, m := range modes {
					s := summarize(m.label, runs[i])
					t.Logf("uretec %-5s %-28s %4d baglanti  %8.0f istek/s "+
						"(%.0f - %.0f, yayilma %.1f%%)",
						g.cpus, m.label, peak[i], s.median, s.low, s.high,
						s.spread()*100)
					if s.median > best[i].median {
						best[i], bestGen[i] = s, g.cpus
					}
				}
			}

			// Cost per request, at one connection.
			//
			// One rather than the ladder's busiest rung, because at one
			// connection nothing is queueing anywhere: the number is the
			// round trip itself. An earlier version subtracted a p50
			// measured at 8 connections from one measured at 128 and
			// reported "the proxy adds -1.174ms" - a difference between
			// two different offered loads is a difference in queueing,
			// not in cost.
			pinSelf(t, gens[0].cpus, gens[0].size)
			var lone []summary
			for _, m := range modes {
				var runs []result
				for round := 0; round < latencyRounds; round++ {
					r := drive(t, m.label, "https://"+srv.addrs[m.key],
						steady(1), clientTLS)
					mustBeAMeasurement(t, r)
					runs = append(runs, r)
				}
				lone = append(lone, summarize(m.label, runs))
			}

			// The report.
			for i, s := range best {
				line := fmt.Sprintf("KAPASITE %-28s en az %6.0f istek/s "+
					"(%4d baglanti, uretec %s, %.0f - %.0f)",
					s.label, s.median, peak[i], bestGen[i], s.low, s.high)
				if bestGen[i] != gens[0].cpus {
					line += "  [en iyisini kucuk uretec verdi]"
				}
				t.Log(line)
				if s.spread() > wideSpread {
					t.Logf("KAPASITE %-28s !! tekrarlar %%%.0f ayrildi: bu bir ARALIK",
						s.label, s.spread()*100)
				}
			}
			for i, s := range best[1:] {
				t.Logf("KAPASITE %-28s taban kapasitesinin %.2f'si; %d baglantida "+
					"tepesinin %%%.0f'i",
					s.label, s.median/best[0].median, concurrencyLadder[len(concurrencyLadder)-1],
					retained[i+1]*100)
			}
			for i, s := range lone {
				line := fmt.Sprintf("GECIKME  %-28s tek baglanti  p50 %9s",
					s.label, s.p50.Round(time.Microsecond))
				if i > 0 {
					line += fmt.Sprintf("  ekledigi %s",
						(s.p50 - lone[0].p50).Round(time.Microsecond))
				}
				t.Log(line)
			}
		})
	}
}

//go:build e2e || docker

// What both end-to-end suites need.
//
// Two suites, because there are two deployments: e2e_test.go installs
// the release tarball on this machine, docker_test.go brings up the
// shipped compose file. They ask the same question - does a real request
// become a number on the dashboard - of two arrangements that have
// already broken differently, so the shared half is the parts that are
// genuinely the same: a TLS origin to proxy to, a request through the
// collector, a pageview, and reading the cards off a rendered page.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// flushWait is how long either suite waits for something the collector
// writes.
//
// Its interval is ten seconds - storage.flush_interval_seconds - and
// three times that is the difference between a slow flush and a broken
// one.
//
// Derived from a configured interval rather than from this machine,
// which is the whole of why it is safe on a slower one: a build agent
// would have to make a ten-second ticker take thirty before this
// produced a false red. See internal/invariants/thresholds_test.go for
// the rule and for the three red builds that wrote it.
//
// Here rather than in e2e_test.go, where it began. The container suite
// was written second, needed exactly this number, and did not have it.
const flushWait = 30 * time.Second

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

type originServer struct {
	addr string
	cert tls.Certificate
}

// startOrigin is the customer's own website: a TLS server the collector
// proxies to without decrypting.
func startOrigin(t *testing.T) *originServer {
	return startOriginOn(t, "127.0.0.1:0")
}

// startOriginOn is startOrigin with a chosen bind address.
//
// The container suite needs 0.0.0.0: its collector runs in a container
// and reaches the host by its gateway address, so an origin bound to the
// host's loopback is one the collector cannot connect to at all. The
// symptom is an EOF from the proxy with nothing in any log, because from
// the collector's side the backend simply refused.
func startOriginOn(t *testing.T, bind string) *originServer {
	t.Helper()

	cert := selfSigned(t)
	ln, err := tls.Listen("tcp", bind, &tls.Config{
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "origin-served-this")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return &originServer{addr: ln.Addr().String(), cert: cert}
}

// throughProxy makes a real HTTPS request through the collector.
func throughProxy(t *testing.T, collectorAddr string, origin *originServer) string {
	t.Helper()

	leaf, err := x509.ParseCertificate(origin.cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// Every request goes to the collector's address; the
			// certificate is the origin's. Verification stays on and
			// trusts the origin's own authority, so the handshake this
			// exercises is a real one.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, collectorAddr)
			},
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12},
		},
	}

	resp, err := client.Get("https://127.0.0.1/")
	if err != nil {
		t.Fatalf("the request through the collector failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// sendPageview POSTs one event, the way the snippet in a page does.
//
// Hand-built rather than driven through a browser: what is being proved
// here is that the beacon process accepts a real request over HTTP and
// the row reaches the database under the right site. Whether the
// snippet itself builds this body correctly is a different question,
// and internal/browsertest answers it against a real Chromium.
func sendPageview(t *testing.T, beaconAddr, site string) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"site":     site,
		"type":     "pageview",
		"url":      "/fiyatlandirma",
		"title":    "Fiyatlandırma",
		"language": "tr-TR",
		"screen_w": 1920,
		"screen_h": 1080,
	})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost,
		"http://"+beaconAddr+"/_ca/event", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// A real browser's, because the beacon classifies it: an empty
	// user-agent is the sort of thing a bot filter drops, and a test
	// whose row is filtered out later would report the wrong fault.
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("the beacon did not answer: %v", err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("the beacon answered %d: %s", resp.StatusCode, answer)
	}
}

// getPage returns where the client ended up, the status there, and the
// body. The landing path matters as much as the status on a site whose
// answer to "you are not signed in" is a 200 with a login form.
func getPage(t *testing.T, client *http.Client, url string) (landed string, status int, body string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Request.URL.Path, resp.StatusCode, string(raw)
}

// hasANumber reports whether the page carries a figure a reader would
// recognise as a measurement.
//
// The weakest assertion that still fails on the case this test exists
// for: a dashboard that renders every panel with a dash in it. It looks
// for a digit inside a value element rather than anywhere on the page,
// because a page always carries digits - dates, version numbers, a
// footer year.
func hasANumber(page string) bool {
	return regexp.MustCompile(`(?s)class="[^"]*deger[^"]*"[^>]*>[^<]*[0-9]`).MatchString(page)
}

// card is one figure on the dashboard, and whether it has one.
//
// Read off the rendered page rather than by importing the panel's own
// types, and crude on purpose: these suites drive the built binary, and
// a diagnosis that came from the same structs the page is built from
// would agree with the page about a mistake they share.
//
// A field rather than a suffix on a string. Both suites used to ask
// `strings.HasSuffix(line, "(empty)")` for themselves, which is two
// copies of a decision that has to be one - and two copies is how the
// suites came to disagree about whether an empty card was worth waiting
// for. A bool nobody can spell differently ends that.
type card struct {
	title  string
	value  string
	filled bool
}

func cards(page string) []card {
	var out []card
	for _, m := range cardRE.FindAllStringSubmatch(page, -1) {
		out = append(out, card{
			title:  strings.TrimSpace(m[1]),
			value:  strings.TrimSpace(m[3]),
			filled: m[2] != "kart-bos",
		})
	}
	return out
}

// cardLines renders the cards for a log line.
func cardLines(page string) []string {
	var out []string
	for _, c := range cards(page) {
		state := "value"
		if !c.filled {
			state = "empty"
		}
		out = append(out, fmt.Sprintf("%s = %s (%s)", c.title, c.value, state))
	}
	return out
}

func emptyCards(page string) int {
	n := 0
	for _, c := range cards(page) {
		if !c.filled {
			n++
		}
	}
	return n
}

// dashboard reads a site's dashboard, waiting for the cards to fill.
//
// The only way either suite fetches that page -
// internal/invariants/e2ecards_test.go keeps it the only one - so the
// wait below cannot be present on one deployment path and missing on the
// other, which is exactly what happened.
//
// # Why a poll, and not a read
//
// The dashboard is fed by two processes with different clocks. The
// beacon writes every two seconds or every five hundred rows, whichever
// comes first, so its four cards are filled almost as soon as the
// pageview is accepted. The collector summarises its rate store on a
// ticker - storage.flush_interval_seconds, ten by default - started when
// the process started, so its two cards appear at the next tick:
// anywhere from immediately to a full interval after the request, with
// nothing wrong anywhere.
//
// A single read therefore asks a question whose answer is a coin toss
// weighted by how fast this machine starts containers.
//
// It came due on the nightly of 2026-09-01. Four cards carried numbers
// and the two traffic cards read "Bu site için henüz hiç bağlantı kaydı
// yok. Trafik toplayıcı bu sitenin önünde çalışmıyor olabilir." - the
// stack was working, the collector had proxied the request and returned
// the origin's body, and the page was simply read before the tick.
//
// Measured afterwards on a machine where it passes, by moving the panel
// session ahead of the request so the polling could start beside it
// rather than five seconds after it:
//
//	+30ms    six cards empty
//	+1.38s   two cards empty   <- the beacon's four, at its 2s interval
//	+9.39s   none empty        <- the collector's two, at its 10s tick
//
// Both faces of the toss on one machine, half an hour apart: an earlier
// run of the same probe read the page once at +5.38s and found every
// card filled. Nothing differed but where the request fell in the
// ticker's cycle.
//
// # Why the tarball suite needs this too, even though it never failed
//
// It waits on the row itself before it opens the panel: it has a
// superuser connection and can ask the database whether
// traffic_snapshots has anything in it. The container path cannot - the
// shipped compose file publishes no database port, deliberately - so the
// rendered page is the only thing it can watch.
//
// That difference is the reason one suite waited and the other did not,
// and it is not a reason for the *check* to exist twice. It lives here,
// both call it, and a third deployment path gets it without having to
// rediscover it. That rediscovery is what this whole group is about:
// e2e_test.go's install() carries the same scar one function over, where
// the flag the gate had from the start was missing from the path written
// later.
func dashboard(t *testing.T, client *http.Client, panelAddr, siteID string) string {
	t.Helper()
	url := "http://" + panelAddr + "/site/" + siteID

	read := func() string {
		_, status, body := getPage(t, client, url)
		if status != http.StatusOK {
			t.Fatalf("the dashboard for %s answered %d:\n%s", siteID, status, body)
		}
		return body
	}

	deadline := time.Now().Add(flushWait)
	page := read()
	for emptyCards(page) > 0 {
		if time.Now().After(deadline) {
			// Returned rather than failed here. What an empty card is
			// depends on the suite - the container path has six and the
			// tarball path checks a figure as well - and a helper that
			// decided it would be making the caller's judgement with less
			// context than the caller has.
			return page
		}
		time.Sleep(500 * time.Millisecond)
		page = read()
	}
	return page
}

// lastLine is build.sh's final line with its "== " marker removed.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimPrefix(strings.TrimSpace(lines[len(lines)-1]), "== ")
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

var cardRE = regexp.MustCompile(`(?s)<h2 class="kart-baslik">(.*?)</h2>.*?<p class="(kart-deger|kart-bos)[^"]*">(.*?)</p>`)

// DevAccessSegment is what a redemption URL contains. Written out
// rather than imported: this test drives the built binary, and taking
// the constant from the source would let a path change pass here while
// breaking the package.
const DevAccessSegment = "/gelistirici/"

// SetupPathPrefix is where a redeemed developer link leads. Written out
// rather than imported, for the same reason as the segment above.
const SetupPathPrefix = "/kurulum/"

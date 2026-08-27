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

func cardLines(page string) []string {
	var out []string
	for _, m := range cardRE.FindAllStringSubmatch(page, -1) {
		state := "value"
		if m[2] == "kart-bos" {
			state = "empty"
		}
		out = append(out, fmt.Sprintf("%s = %s (%s)",
			strings.TrimSpace(m[1]), strings.TrimSpace(m[3]), state))
	}
	return out
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

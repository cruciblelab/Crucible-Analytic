package relupdate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/releasesign"
)

// The manifest, checked against a server that serves it.
//
// The properties worth asserting are all about what happens when the
// document is *not* what it should be, because the case where it is
// correct is the one every implementation gets right.

// manifestServer serves a manifest and its signature over TLS, and
// returns the source that reads them.
//
// httptest.NewTLSServer rather than a plain one: Source refuses a
// base URL that is not https, and a test that worked around that would
// be testing a Source this project does not ship.
func manifestServer(t *testing.T, body string, sign func([]byte) []byte) (Source, *httptest.Server) {
	t.Helper()
	key, err := releasesign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	sig := sign([]byte(body))

	mux := http.NewServeMux()
	mux.HandleFunc("/"+ManifestName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/"+ManifestSigName, func(w http.ResponseWriter, r *http.Request) {
		if sig == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(sig)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	_ = key
	return Source{BaseURL: srv.URL, Client: srv.Client()}, srv
}

// signedWith builds a manifest signed by a key, and the matching source.
func signedWith(t *testing.T, body string, domain string) (Source, releasesign.PublicKey) {
	t.Helper()
	key, err := releasesign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := key.SignIn(domain, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+ManifestName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/"+ManifestSigName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sig)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	return Source{BaseURL: srv.URL, PublicKey: key.Public(), Client: srv.Client()}, key.Public()
}

// TestAVerifiedManifestIsRead is the positive case, first, so the
// refusals below cannot be satisfied by a reader that refuses
// everything.
func TestAVerifiedManifestIsRead(t *testing.T) {
	body := "version: v0.21.0\nreleased: 2026-09-04T05:00:00Z\nnotes: https://example.invalid/x\n"
	src, _ := signedWith(t, body, releasesign.ManifestDomain)

	m, err := src.CheckLatest(context.Background())
	if err != nil {
		t.Fatalf("a correctly signed manifest was refused: %v", err)
	}
	if m.Version != "v0.21.0" {
		t.Errorf("version is %q", m.Version)
	}
	if !m.Released.Equal(time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("released is %v", m.Released)
	}
	if m.Notes != "https://example.invalid/x" {
		t.Errorf("notes is %q", m.Notes)
	}
}

// TestAManifestSignedForTheOtherDocumentIsRefused.
//
// One key signs both a release's SHA256SUMS and this. Without the domain
// separator an old signed SHA256SUMS could be served as a manifest, and
// whoever could do that would be choosing which version a panel offers
// its customer.
func TestAManifestSignedForTheOtherDocumentIsRefused(t *testing.T) {
	body := "version: v9.9.9\n"
	src, _ := signedWith(t, body, "crucible-analytic/release-sums/v1\n")

	if _, err := src.CheckLatest(context.Background()); err == nil {
		t.Fatal("a manifest signed in the SHA256SUMS domain was accepted")
	}
}

// TestAnUnsignedManifestIsRefusedRatherThanTrusted.
//
// The fallback that must not exist: if a missing signature meant "read
// it anyway", an attacker who could delete one file would have removed
// the requirement to sign.
func TestAnUnsignedManifestIsRefusedRatherThanTrusted(t *testing.T) {
	src, _ := manifestServer(t, "version: v9.9.9\n", func([]byte) []byte { return nil })
	key, err := releasesign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	src.PublicKey = key.Public()

	_, err = src.CheckLatest(context.Background())
	if err == nil {
		t.Fatal("a manifest with no signature was accepted")
	}
	if !strings.Contains(err.Error(), ManifestSigName) {
		t.Errorf("the refusal does not name the missing file: %v", err)
	}
}

// TestASourceWithNoKeyRefusesToLookAtAll.
//
// Not "fetches and cannot verify" but "does not fetch". A version string
// read from an unverifiable document is exactly the value this design
// refuses to put in front of a customer, and the earliest place to
// refuse it is before it exists.
func TestASourceWithNoKeyRefusesToLookAtAll(t *testing.T) {
	var reached bool
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { reached = true })
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	src := Source{BaseURL: srv.URL, Client: srv.Client()}
	if _, err := src.CheckLatest(context.Background()); err == nil {
		t.Fatal("a source with no public key returned a manifest")
	}
	if reached {
		t.Error("the server was contacted despite there being no key to check the answer " +
			"with. Fetching first and failing later means the document existed in this " +
			"process before anything vouched for it")
	}
}

// TestAManifestNamingSomethingThatIsNotAVersionIsRefused.
//
// The version travels on to become part of a URL and part of a queued
// request. A signed document is not a trusted document in the sense
// that matters here: signing proves who wrote it, never that what they
// wrote is well formed.
func TestAManifestNamingSomethingThatIsNotAVersionIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"empty":      "version:\n",
		"words":      "version: latest\n",
		"path":       "version: ../../etc/passwd\n",
		"no field":   "released: 2026-09-04T05:00:00Z\n",
		"url":        "version: https://example.invalid/x\n",
		"with slash": "version: v0.21.0/extra\n",
	} {
		t.Run(name, func(t *testing.T) {
			src, _ := signedWith(t, body, releasesign.ManifestDomain)
			if _, err := src.CheckLatest(context.Background()); err == nil {
				t.Errorf("%q was accepted as a manifest", body)
			}
		})
	}
}

// TestAnUnknownFieldDoesNotBreakAnOlderReader.
//
// The reader that has to keep working is the one already installed on
// somebody's server, not the one being written. A publisher who adds a
// field must not break every deployment that has not upgraded yet.
func TestAnUnknownFieldDoesNotBreakAnOlderReader(t *testing.T) {
	body := "version: v0.21.0\nsomething-added-later: whatever\n# a comment\n\n"
	src, _ := signedWith(t, body, releasesign.ManifestDomain)

	m, err := src.CheckLatest(context.Background())
	if err != nil {
		t.Fatalf("a manifest with an unknown field was refused: %v", err)
	}
	if m.Version != "v0.21.0" {
		t.Errorf("version is %q", m.Version)
	}
}

// TestANotesLinkThatIsNotHttpIsDropped.
//
// The manifest is signed, so this is not about distrusting the
// publisher: it is about a signing key that leaks producing a link on a
// customer's page that can do more than open one.
func TestANotesLinkThatIsNotHttpIsDropped(t *testing.T) {
	for _, notes := range []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
	} {
		body := "version: v0.21.0\nnotes: " + notes + "\n"
		src, _ := signedWith(t, body, releasesign.ManifestDomain)
		m, err := src.CheckLatest(context.Background())
		if err != nil {
			t.Fatalf("%q: %v", notes, err)
		}
		if m.Notes != "" {
			t.Errorf("%q survived into the manifest as %q", notes, m.Notes)
		}
	}
}

// TestASourceThatPublishesNoManifestSaysSoDistinctly.
//
// A publisher who has not started publishing one is not a fault. The
// page draws a different sentence for it, so the error has to be
// distinguishable rather than merged into "could not reach".
func TestASourceThatPublishesNoManifestSaysSoDistinctly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	key, err := releasesign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	src := Source{BaseURL: srv.URL, PublicKey: key.Public(), Client: srv.Client()}

	_, err = src.CheckLatest(context.Background())
	if err == nil {
		t.Fatal("a source with no manifest returned one")
	}
	if !errors.Is(err, ErrNoManifest) {
		t.Errorf("got %v, want ErrNoManifest - the page says something different for "+
			"\"not published\" than for \"unreachable\"", err)
	}
}

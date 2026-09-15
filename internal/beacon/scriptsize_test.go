package beacon

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The size README states for the snippet is the size the snippet is.
//
// # Why this test exists
//
// Because the number drifted and nobody noticed for months. README said
// the script was "2.1 KB over the wire (gzipped), the same size class as
// Umami and Plausible", and by the time P1 had added opt-out and P2 the
// embedded disclosure it was 5.6 KB gzipped - two and a half times
// Umami's 2.3 KB, in a paragraph whose whole point was that the size was
// comparable.
//
// That is the shape of every hand-kept number in a document: right the
// day it was written, and wrong afterwards with nothing failing. This
// project already has one of these for the setup wizard's check count
// (internal/docs/checkcount_test.go), and the reason is the same.
//
// # What is asserted, and what is not
//
// Both numbers in README's table, against the embedded file, plus the
// sentence beside them about who does the compressing. Not Umami's row -
// that is a measurement of another repository taken on a named version,
// and this suite cannot re-measure it. It is labelled "for scale" in the
// document for that reason.
//
// The sentence is in scope because it went stale inside a day. The
// paragraph used to say "The beacon does not compress it. There is no
// gzip anywhere in this serving path, deliberately" - which was true
// when it was written and false as soon as the beacon started
// compressing. A claim about behaviour ages exactly like a number
// does.
//
// The tolerance is one tenth of a KB either way, which is what the
// document's own precision is. A change that moves the script by more
// than 100 bytes has to move the sentence with it - and that is the
// point: the size is a promise to somebody putting this on their site,
// so growing it should cost a deliberate edit.
func TestTheSnippetSizeInTheReadmeIsTheSizeItIs(t *testing.T) {
	served := float64(len(scriptSource)) / 1024

	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(scriptSource); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := float64(buf.Len()) / 1024

	readme := readReadme(t)
	row := regexp.MustCompile(`\|\s*this snippet\s*\|\s*([0-9.]+) KB\s*\|\s*([0-9.]+) KB\s*\|`)
	m := row.FindStringSubmatch(readme)
	if m == nil {
		t.Fatalf("README has no \"this snippet\" size row.\n"+
			"The row is the thing this test checks; without it the test passes by "+
			"checking nothing. Measured now: %.1f KB served, %.1f KB gzipped.",
			served, compressed)
	}

	for _, tc := range []struct {
		what   string
		stated string
		got    float64
	}{
		{"as served", m[1], served},
		{"gzipped", m[2], compressed},
	} {
		stated, err := strconv.ParseFloat(tc.stated, 64)
		if err != nil {
			t.Fatalf("README's %s figure %q is not a number: %v", tc.what, tc.stated, err)
		}
		if diff := stated - tc.got; diff > 0.1 || diff < -0.1 {
			t.Errorf("README says the snippet is %.1f KB %s; it is %.1f KB.\n"+
				"Either the script changed and the sentence did not, or the sentence "+
				"was wrong. A size in a document is a promise to whoever is deciding "+
				"whether to put this on their pages.", stated, tc.what, tc.got)
		}
	}

	if compressed >= served {
		t.Errorf("gzip made the script bigger (%.1f KB vs %.1f KB), which means this "+
			"measurement is not measuring what it thinks", compressed, served)
	}

	// And the sentence: README says the beacon compresses this itself,
	// so the handler has to. The pair is the point - either half moving
	// alone is the defect, and the half that moves silently is the
	// document.
	saysBeacon := strings.Contains(readme, "beacon compresses it itself")
	r := httptest.NewRequest(http.MethodGet, DefaultPathPrefix+"/ca.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	(&Server{Sites: []string{"acme"}, Sink: &fakeSink{}, Visitors: newTestVisitorIDs(t)}).
		Handler().ServeHTTP(w, r)
	doesCompress := w.Header().Get("Content-Encoding") == "gzip"

	if saysBeacon != doesCompress {
		t.Errorf("README says the beacon compresses the snippet: %v. It does: %v.\n"+
			"Whichever of the two moved, the other has to move with it: the gzipped "+
			"column means 'what a visitor downloads' only while the beacon is the "+
			"thing producing it, and otherwise it means 'if your proxy is set up "+
			"for it', which is a different promise.", saysBeacon, doesCompress)
	}
}

// readReadme reads the document from the repository root.
//
// Walks up rather than assuming a depth: this package sits two levels
// down today and a test that hard-coded "../.." would break the day the
// tree moved, silently reading nothing.
func readReadme(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		candidate := filepath.Join(dir, "README.md")
		if body, err := os.ReadFile(candidate); err == nil {
			return string(body)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no README.md above the working directory")
		}
		dir = parent
	}
}

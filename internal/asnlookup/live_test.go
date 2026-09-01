//go:build network

package asnlookup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ipsources"
)

// Every dataset in the library, fetched from where it really lives.
//
// M1 turned one provider into five, and the four datasets it added are
// reachable only by a deployment that chooses them. Nothing else in this
// repository ever asks whether their URLs resolve or whether the bytes
// behind them still parse - the unit tests use fixtures this repository
// controls, and internal/panel only ever sees ids.
//
// That is the shape of the failure this project keeps finding, one level
// up: a customer picks server-country, the next refresh fetches a 404,
// the fallback quietly puts them back on the default, and the only
// evidence is one warning line on their own server. Nobody here would
// ever hear about it.
//
//	go test -tags network ./internal/asnlookup/ -run TestLive -v
//
// # Why the network tag and not integration
//
// internal/botdata/live_test.go argues this in full and the reasoning is
// the same: a database this repository starts is available whenever
// somebody starts it; a third party's web server is available when they
// decide it is. A test that goes red for reasons unrelated to the change
// cannot gate a merge without teaching people to ignore red. So it runs
// nightly and reports.

// liveSampleBytes is how much of each file is parsed.
//
// Parsing all of it would be tens of millions of rows for a question
// that a prefix answers: does this URL serve the shape this build
// expects. The whole body is still *read* - see fetchAndDigest - because
// the second question below needs a digest of all of it.
//
// The truncated final row is handled: both parsers stop at a malformed
// record and keep what they already read, which is exactly the behaviour
// a partial read needs.
//
// # What the prefix cannot answer, measured
//
// user-country-ipv4.csv and server-country-ipv4.csv are byte-identical
// for their first 330,696 bytes - the low address space where hosting
// country and user country simply agree. Measured 2026-09-01: the files
// are 8,796,182 and 8,425,916 bytes, 286,082 and 274,801 rows, and
// 11,461 lines differ, the first at line 11,569.
//
// So a prefix is evidence about *shape* and no evidence at all about two
// sources being different sources. That is why the digest is over the
// whole stream, and why it is a separate assertion.
const liveSampleBytes = 256 << 10

// liveMinRows is the floor a real dataset clears easily and an error
// page does not.
//
// An HTML 404 body parses to zero rows, so a bare "did it error" check
// would pass on one. This is the check that catches a URL that moved.
const liveMinRows = 100

func TestLiveEverySourceInTheLibraryStillExists(t *testing.T) {
	if os.Getenv("CA_OFFLINE") != "" {
		t.Skip("CA_OFFLINE is set")
	}

	client := &http.Client{Timeout: 5 * time.Minute}

	// digest of the whole file -> the source and family that served it,
	// for the "these are not the same file" check below.
	digests := map[string]string{}

	for _, src := range ipsources.All() {
		for _, file := range []struct {
			url    string
			family string
		}{
			{src.IPv4URL, "IPv4"},
			{src.IPv6URL, "IPv6"},
		} {
			name := src.ID + "/" + file.family
			t.Run(name, func(t *testing.T) {
				body, digest, total := fetchAndDigest(t, client, file.url)

				switch src.Kind {
				case ipsources.KindCountry:
					checkCountryShape(t, src, file.family, body)
				case ipsources.KindASN:
					checkASNShape(t, src, file.family, body)
				default:
					t.Fatalf("%s has kind %d, which this test does not know how to "+
						"check - a new kind needs a check here or it ships unmeasured",
						src.ID, src.Kind)
				}

				// Two ids pointing at the same bytes would be a choice that
				// is not a choice: the panel would offer an alternative, the
				// Why text would explain when to prefer it, and picking it
				// would change nothing. Nothing else in this repository can
				// notice that - the ids differ, the URLs differ, and only
				// the files are the same.
				if other, seen := digests[digest]; seen {
					t.Errorf("%s serves byte-identical content to %s (%d bytes, sha256 %s).\n"+
						"The panel offers these as different datasets and explains when to "+
						"prefer one, so a deployment would change its setting and get the "+
						"same data back", name, other, total, digest[:16])
				}
				digests[digest] = name
				t.Logf("%s: %d bytes, sha256 %s", name, total, digest[:16])
			})
		}
	}
}

// fetchAndDigest reads a URL once, returning the first liveSampleBytes
// for parsing, a digest over the whole body, and its length.
//
// One pass rather than two requests: a second GET could see a different
// file - these are rebuilt daily - and a digest that does not belong to
// the bytes that were parsed would be worse than no digest.
func fetchAndDigest(t *testing.T, client *http.Client, url string) (sample, digest string, total int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("fetching %s: %v\n\nIf this source has moved, that is the finding: a "+
			"deployment that chose it would fall back to the default with only one "+
			"warning line on its own server to say so.", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned HTTP %d, so a deployment that chose this dataset fetches "+
			"nothing at every refresh", url, resp.StatusCode)
	}

	sum := sha256.New()
	var head bytes.Buffer
	// Tee the whole body into the hash; the first liveSampleBytes of it
	// also land in head.
	total, err = io.Copy(sum, io.TeeReader(resp.Body, &limitedWriter{w: &head, remaining: liveSampleBytes}))
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return head.String(), hex.EncodeToString(sum.Sum(nil)), total
}

// limitedWriter passes through the first remaining bytes and silently
// drops the rest, so one io.Copy can both hash everything and keep a
// prefix.
//
// Pointer receiver, and the reason is a bug this had on the first
// writing: with a value receiver the counter reset on every call, and
// since io.Copy writes in 32 KiB chunks - each one comfortably under the
// limit - nothing was ever dropped and the "prefix" was the whole file.
// It would have passed, slowly, and parsed several million rows per
// nightly.
type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining > 0 {
		keep := p
		if len(keep) > l.remaining {
			keep = keep[:l.remaining]
		}
		if _, err := l.w.Write(keep); err != nil {
			return 0, err
		}
		l.remaining -= len(keep)
	}
	// The full length is always reported: this is the tee side of the
	// copy, and a short write there would abort the read the hash needs
	// to finish.
	return len(p), nil
}

// checkCountryShape parses a sample with the country parser and asserts
// the values look like country codes.
//
// The row count alone is not enough. An ASN file fed to the country
// parser produces zero rows (four fields, not three) - but a *different*
// three-column file would produce plenty of rows carrying nonsense, and
// the deployment would then report visitors from country "13335".
func checkCountryShape(t *testing.T, src ipsources.Source, family, body string) {
	t.Helper()
	entries, err := parseCountryCSV(strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s did not parse: %v", src.ID, family, err)
	}
	if len(entries) < liveMinRows {
		t.Fatalf("%s %s parsed %d rows out of %d bytes, which is what an error page or "+
			"a changed format looks like - not a country dataset",
			src.ID, family, len(entries), len(body))
	}

	for _, e := range entries {
		code := e.value
		if len(code) != 2 || strings.ToUpper(code) != code {
			t.Fatalf("%s %s carries %q where an ISO 3166-1 alpha-2 code belongs. This "+
				"parses without error and reaches the panel as a country name",
				src.ID, family, code)
		}
		if !addrFamilyMatches(e.start, family) {
			t.Fatalf("%s %s contains %s, which is the other address family - the two "+
				"files would be loaded into each other's range table",
				src.ID, family, e.start)
		}
	}
	t.Logf("%s %s: %d rows in the first %d bytes", src.ID, family, len(entries), len(body))
}

// checkASNShape is the same for the four-column datasets.
func checkASNShape(t *testing.T, src ipsources.Source, family, body string) {
	t.Helper()
	entries, err := parseASNCSV(strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s did not parse: %v", src.ID, family, err)
	}
	if len(entries) < liveMinRows {
		t.Fatalf("%s %s parsed %d rows out of %d bytes, which is what an error page or "+
			"a changed format looks like - not an ASN dataset",
			src.ID, family, len(entries), len(body))
	}

	named := 0
	for _, e := range entries {
		// AS 0 is the resolver's "not found" value, and a dataset that
		// really contained it would make every unresolvable address look
		// like a member of a real network.
		if e.value.asn <= 0 {
			t.Fatalf("%s %s carries AS %d; 0 and negative numbers are what this "+
				"resolver returns for 'not found'", src.ID, family, e.value.asn)
		}
		if e.value.org != "" {
			named++
		}
		if !addrFamilyMatches(e.start, family) {
			t.Fatalf("%s %s contains %s, which is the other address family",
				src.ID, family, e.start)
		}
	}
	// The org name is what the panel actually shows. A file that parsed
	// but had lost its fourth column would still pass every check above.
	if named == 0 {
		t.Fatalf("%s %s parsed %d rows and not one carries an organisation name, so "+
			"the panel would show bare AS numbers", src.ID, family, len(entries))
	}
	t.Logf("%s %s: %d rows in the first %d bytes, %d named", src.ID, family, len(entries), len(body), named)
}

func addrFamilyMatches(addr netip.Addr, family string) bool {
	if family == "IPv4" {
		return addr.Is4()
	}
	return addr.Is6() && !addr.Is4In6()
}

// TestLiveTheDefaultsAreAmongThemIsNotAssumed.
//
// The loop above walks the library, so a library that lost its defaults
// would still produce a green run over whatever was left. Naming them
// here is the guard - and they are the two files every installation that
// has chosen nothing downloads.
func TestLiveTheDefaultsAreAmongThemIsNotAssumed(t *testing.T) {
	for _, id := range []string{ipsources.DefaultCountry, ipsources.DefaultASN} {
		if _, ok := ipsources.ByID(id); !ok {
			t.Errorf("the library has no %q, which is a default - "+
				"every installation that has chosen nothing fetches it", id)
		}
	}
	if t.Failed() {
		return
	}
	t.Log(fmt.Sprintf("defaults present: %s, %s", ipsources.DefaultCountry, ipsources.DefaultASN))
}

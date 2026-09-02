package asnlookup

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCountryOnlyNeverReadsTheASNDataset.
//
// # The claim, and why "empty" would not be it
//
// Country-only mode exists to save memory on a small VDS, and the memory
// it saves is mostly not the memory the range tables occupy. Both
// parsers read a whole file and build a slice of every range in it
// before anything is swapped in, so the peak during a refresh is larger
// than the steady state that follows. A mode that loaded the ASN files
// and then discarded them would leave that peak exactly where it was and
// still pass a test that only asked whether the tables came out empty.
//
// So the assertion is that the files are never opened.
//
// # How that is proved without trusting a counter
//
// The two ASN filenames are created as *directories*. Nothing here can
// read one: os.Open succeeds and the first Read returns EISDIR, and
// unlike a permission bit that is true for root as well - which matters,
// because most machines run this suite as root and a check that quietly
// stops checking there is worse than none.
//
// So if refresh so much as reaches for the ASN dataset, storeASN logs
// "asn ipv4 refresh failed" and this test sees it. The absence of that
// line is the evidence.
//
// The country half is asserted too, in the same run. A mode that skipped
// everything would satisfy the ASN half perfectly.
func TestCountryOnlyNeverReadsTheASNDataset(t *testing.T) {
	dir := t.TempDir()

	// Real country data, so the country half has something to find.
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write(countryIPv4Filename, "1.0.0.0,1.0.0.255,AU\n")
	write(countryIPv6Filename, "2001:200::,2001:200:ffff:ffff:ffff:ffff:ffff:ffff,JP\n")

	// Booby traps. Reading either is an error on every machine.
	for _, name := range []string{asnIPv4Filename, asnIPv6Filename} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatalf("creating the %s trap: %v", name, err)
		}
	}

	var logged bytes.Buffer
	r := &Resolver{
		localCSVPath: dir,
		CountryOnly:  true,
		// No pool in this test, so the persistence step has to be off;
		// it is off in the beacon for its own reasons anyway.
		SkipRangePersistence: true,
		cache:                newTTLCache(16, 0),
		Logger:               slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	r.refresh(context.Background())

	// ": asn " with the spaces, not "asn". Every line this package logs
	// begins "asnlookup:", so the bare substring matches the *country*
	// success line and fails a working mode - which is exactly what it
	// did the first time this was written. The messages that matter read
	// "asnlookup: asn ipv4 refresh failed" and "asnlookup: asn refresh
	// complete"; the country ones read "asnlookup: country ...".
	if got := logged.String(); strings.Contains(got, ": asn ") {
		t.Errorf("country-only mode touched the ASN dataset. The traps in %s can only "+
			"be reached by opening them, so this line means refresh fetched, read or "+
			"parsed a file it was told to leave alone:\n%s", dir, got)
	}
	if r.asnTable4.Load() != nil || r.asnTable6.Load() != nil {
		t.Error("an ASN range table exists after a country-only refresh")
	}

	// And the half that must still work.
	if r.countryTable4.Load() == nil || r.countryTable6.Load() == nil {
		t.Fatalf("country-only mode loaded no country table either, so the ASN half "+
			"above proves nothing. What was logged:\n%s", logged.String())
	}
	for _, tc := range []struct{ ip, want string }{
		{"1.0.0.42", "AU"},
		{"2001:200::1", "JP"},
	} {
		got := r.Resolve(netip.MustParseAddr(tc.ip))
		if !got.Found || got.Country != tc.want {
			t.Errorf("Resolve(%s) country = %q found=%v, want %q found=true",
				tc.ip, got.Country, got.Found, tc.want)
		}
		if got.ASN != 0 || got.ASNName != "" {
			t.Errorf("Resolve(%s) reports ASN %d %q, and there is no ASN dataset loaded",
				tc.ip, got.ASN, got.ASNName)
		}
	}
}

// TestTheDefaultStillReadsBothDatasets.
//
// The other side of the switch, and the one that catches a fix applied
// too widely: a refresh that stopped loading ASN data for everybody
// would pass every assertion above.
//
// Same fixture shape, with real ASN files this time - so the traps are
// the only difference between the two tests, and what they measure is
// the flag rather than the fixture.
func TestTheDefaultStillReadsBothDatasets(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write(countryIPv4Filename, "1.0.0.0,1.0.0.255,AU\n")
	write(countryIPv6Filename, "2001:200::,2001:200:ffff:ffff:ffff:ffff:ffff:ffff,JP\n")
	write(asnIPv4Filename, "1.0.0.0,1.0.0.255,13335,\"Cloudflare, Inc.\"\n")
	write(asnIPv6Filename, "2001:200::,2001:200:ffff:ffff:ffff:ffff:ffff:ffff,2500,\"WIDE\"\n")

	r := &Resolver{
		localCSVPath:         dir,
		SkipRangePersistence: true,
		cache:                newTTLCache(16, 0),
		Logger:               slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
	r.refresh(context.Background())

	got := r.Resolve(netip.MustParseAddr("1.0.0.42"))
	if got.ASN != 13335 {
		t.Errorf("Resolve without country_only found ASN %d, want 13335.\n"+
			"The ASN dataset is loaded by default and this is what country_only turns off",
			got.ASN)
	}
	if !got.Found || got.Country != "AU" {
		t.Errorf("Resolve country = %q (found=%v), want AU", got.Country, got.Found)
	}
}

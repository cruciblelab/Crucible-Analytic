package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A fingerprint is never aggregated with max().
//
// Every query in this package groups snapshots by address, and max() is
// the right tool for the columns that are constant per address - country
// and ASN come from the same lookup on every flush. A TLS fingerprint is
// not one of those: it belongs to the client, and an address is not a
// client. So max(ja4) answers "which fingerprint sorts highest", which
// has nothing to do with which one the row's other columns describe.
//
// Four queries had it, written at four different times, each borrowing
// the spelling from the one next to it. Three of the four also carried
// bool_or(is_known_bot_ja4) on the same row, so the flag and the
// fingerprint printed beside it could come from different snapshots -
// and on a live deployment they did.
//
// This is a structural check because the fifth query will be written the
// same way. representativeJA4 is the expression to use; it is one
// definition, so a fix applies everywhere at once.
func TestNoQueryAggregatesAFingerprintWithMax(t *testing.T) {
	files, err := filepath.Glob("store*.go")
	if err != nil {
		t.Fatal(err)
	}
	// The guard would be worthless if the glob stopped matching: a scan
	// over no files reports no problems.
	if len(files) < 4 {
		t.Fatalf("found only %d store files (%v); this check reads the package's own "+
			"source and would pass over an empty list", len(files), files)
	}

	maxJA4 := regexp.MustCompile(`\bmax\s*\(\s*ja4\s*\)`)
	var scanned int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		scanned++
		text := stripCrossoverComments(string(source))
		for _, found := range maxJA4.FindAllString(text, -1) {
			t.Errorf("%s aggregates a fingerprint with %s.\n"+
				"Use representativeJA4 instead: max() picks whichever fingerprint sorts "+
				"highest, which need not be the one is_known_bot_ja4 was set by, and an "+
				"address can carry several - a /24 in masked mode is up to 256 machines.",
				name, found)
		}
	}
	if scanned == 0 {
		t.Fatal("no non-test store files were read")
	}
}

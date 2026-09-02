//go:build network

package asnlookup

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// TestCountryOnlyIsMeasurablySmaller.
//
// # The half of A3's criterion a fixture cannot answer
//
// The unit test proves the ASN files are never opened. What it cannot
// show is that this is worth doing, because it runs against two-line
// fixtures where every arrangement costs nothing. The claim being made
// is about the real datasets - roughly 135 MB against 65-70 MB - and the
// only honest way to check it is to load the real datasets.
//
// So: network tag, nightly only, alongside the other test that asks
// whether these files still exist and still parse.
//
// # Why the comparison is relative and has no fixed number in it
//
// A megabyte figure would be a fact about today's datasets, which grow
// every month, and about this machine's allocator. Either could move
// without anything being wrong, and the test would go red on a Tuesday.
//
// What cannot move without something being wrong is the ordering: a
// resolver that loaded two of four tables must hold less than one that
// loaded four. Both figures come from the same process, minutes apart,
// so the comparison scales with whatever the data has grown to.
//
// The margin is a property of the data rather than of the hardware. The
// ASN files carry an organisation name per range where the country files
// carry two letters, so the ASN half is the larger one by a wide margin;
// requiring country-only to come in under 70% of full is well inside
// that and still fails loudly if the skip silently stops happening.
func TestCountryOnlyIsMeasurablySmaller(t *testing.T) {
	measure := func(countryOnly bool) uint64 {
		t.Helper()
		r := &Resolver{
			httpClient:           &http.Client{Timeout: 5 * time.Minute},
			SkipRangePersistence: true,
			CountryOnly:          countryOnly,
			cache:                newTTLCache(16, 0),
			Logger:               slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		}
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		r.refresh(context.Background())

		// Held, not peak. The peak during a parse is larger and is the
		// better argument for this mode, but it is not something
		// MemStats can attribute to one resolver while another lives in
		// the same process.
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)

		if r.countryTable4.Load() == nil {
			t.Fatal("no country table loaded, so this measured a failed download " +
				"rather than a mode")
		}
		if countryOnly != (r.asnTable4.Load() == nil) {
			t.Fatalf("country_only = %v but an ASN table %s; the measurement below "+
				"would be comparing two of the same thing",
				countryOnly, map[bool]string{true: "is absent", false: "exists"}[r.asnTable4.Load() == nil])
		}
		held := after.HeapAlloc - min(after.HeapAlloc, before.HeapAlloc)
		runtime.KeepAlive(r)
		return held
	}

	full := measure(false)
	only := measure(true)

	t.Logf("held after a refresh: full %d bytes (%.1f MB), country-only %d bytes (%.1f MB)",
		full, float64(full)/(1<<20), only, float64(only)/(1<<20))

	if full == 0 {
		t.Fatal("a full refresh appeared to hold nothing, so the measurement is not " +
			"measuring; HeapAlloc is being read at the wrong moment")
	}
	if only >= full {
		t.Errorf("country-only held %d bytes and a full refresh held %d.\n"+
			"Loading two range tables instead of four cannot cost more; either the "+
			"skip has stopped happening or this is no longer measuring the resolver",
			only, full)
	}
	if ratio := float64(only) / float64(full); ratio > 0.7 {
		t.Errorf("country-only held %.0f%% of what a full refresh held.\n"+
			"The ASN files carry an organisation name per range against the country "+
			"files' two letters, so the saving should be far larger than this - a "+
			"figure this close suggests the ASN data is being loaded anyway",
			ratio*100)
	}
}

//go:build network

package asnlookup

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"sync/atomic"
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
// # What the two numbers are worth
//
// Measured five times: held came out 136.1 MB and 59.1 MB on every
// single run, to a tenth of a megabyte. The peak did not - 192-216 MB
// full, 102-111 MB country-only - because it depends on when the
// collector decides to run, and a 2ms sampler catches a different
// moment each time.
//
// So held is a figure worth quoting and peak is a ratio worth quoting:
// roughly one and a half to two times held. Anything sizing a container
// limit needs the peak, and needs headroom on top of it, because this
// module is not the only thing in the process.
//
// The margin is a property of the data rather than of the hardware. The
// ASN files carry an organisation name per range where the country files
// carry two letters, so the ASN half is the larger one by a wide margin;
// requiring country-only to come in under 70% of full is well inside
// that and still fails loudly if the skip silently stops happening.
func TestCountryOnlyIsMeasurablySmaller(t *testing.T) {
	// held and peak. The peak is the number that decides whether a
	// container survives: a cgroup limit kills on the high-water mark,
	// not on what is left afterwards, and the parse allocates a slice of
	// every range in the file before any table is swapped in.
	//
	// Sampled from a second goroutine rather than derived from
	// TotalAlloc, which counts every byte ever allocated and would report
	// a figure no limit ever sees.
	measure := func(countryOnly bool) (held, peak uint64) {
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

		var stop atomic.Bool
		var high atomic.Uint64
		watching := make(chan struct{})
		go func() {
			defer close(watching)
			var m runtime.MemStats
			for !stop.Load() {
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > high.Load() {
					high.Store(m.HeapAlloc)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()

		r.refresh(context.Background())
		stop.Store(true)
		<-watching

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
		held = after.HeapAlloc - min(after.HeapAlloc, before.HeapAlloc)
		runtime.KeepAlive(r)
		return held, high.Load()
	}

	full, fullPeak := measure(false)
	only, onlyPeak := measure(true)

	t.Logf("full:         held %.1f MB, peak %.1f MB", float64(full)/(1<<20), float64(fullPeak)/(1<<20))
	t.Logf("country-only: held %.1f MB, peak %.1f MB", float64(only)/(1<<20), float64(onlyPeak)/(1<<20))

	// The peak is what a cgroup limit kills on, so it gets its own
	// assertion rather than being logged and forgotten. Relative, like
	// the one below: which is larger cannot change without something
	// being wrong, while the megabytes move every time the datasets grow.
	if onlyPeak >= fullPeak {
		t.Errorf("country-only peaked at %d bytes and a full refresh at %d.\n"+
			"Parsing two files instead of four cannot cost more at the high-water "+
			"mark, and the high-water mark is what a container limit enforces",
			onlyPeak, fullPeak)
	}

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

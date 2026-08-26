//go:build network

package botdata

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One real fetch against the real source.
//
// The unit tests prove the parser against a fixture this repository
// controls. What they cannot prove is that the fixture still resembles
// what the source actually serves - and since this project no longer
// ships a copy of that data, a parser that has silently stopped matching
// the source means every deployment quietly loses the signal.
//
//	go test -tags network ./internal/botdata/ -run TestLiveFetch -v
//
// # Why its own tag rather than "integration"
//
// It was under the integration tag, alongside the tests that need a real
// TimescaleDB. Those two dependencies are not alike. A database this
// repository starts is available whenever somebody chooses to start it;
// a third party's web server is available when they decide it is, and on
// 2026-08-26 it was not - three runs, all timed out on the TLS
// handshake, with plain curl failing the same way.
//
// That difference decides where a test belongs in CI. This one cannot
// gate a merge: it goes red for reasons having nothing to do with the
// change, and the predictable result is that people learn to ignore red.
// It also must not be deleted, because noticing that the source moved is
// its entire job - a deployment would otherwise lose the known-bot
// signal with no error anywhere. So it runs on a schedule and reports.
//
// The tag names the dependency instead of the ceremony, which also means
// the next network test is filed correctly by whoever writes it.
func TestLiveFetch(t *testing.T) {
	if os.Getenv("CA_OFFLINE") != "" {
		t.Skip("CA_OFFLINE is set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()

	set, err := Fetch(ctx, nil, DefaultSourceURL)
	if err != nil {
		t.Fatalf("fetching %s: %v\n\nIf the source has moved or changed shape, that is the finding: "+
			"deployments would be losing the known-bot signal with no error anywhere.", DefaultSourceURL, err)
	}

	if set.Len() == 0 {
		t.Fatal("the live source produced no usable fingerprints")
	}
	t.Logf("live source: %d fingerprints kept, %d filtered out", set.Len(), set.Dropped)

	if _, ok := set.Labels[""]; ok {
		t.Error("an empty fingerprint survived; all non-TLS traffic would be called a bot")
	}
	for ja4, label := range set.Labels {
		if strings.TrimSpace(ja4) == "" {
			t.Errorf("a blank fingerprint survived with label %q", label)
		}
		// A JA4 is a fixed-shape string; anything wildly off is a sign
		// the source changed field meanings rather than shape.
		if len(ja4) < 20 || !strings.Contains(ja4, "_") {
			t.Errorf("%q does not look like a JA4 fingerprint (label %q)", ja4, label)
		}
	}
	if time.Since(set.FetchedAt) > time.Hour {
		t.Errorf("FetchedAt is %v, which is not now", set.FetchedAt)
	}

	// And it survives a round trip through the file the deployment keeps.
	path := filepath.Join(t.TempDir(), "bot.json")
	if err := Save(path, set); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Len() != set.Len() {
		t.Errorf("saved %d fingerprints, loaded %d", set.Len(), loaded.Len())
	}
}

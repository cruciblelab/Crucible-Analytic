package panel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/botdata"
)

// TestBotDataCheckDistinguishesNeverFetchedFromMissing is the whole
// point of the check. This project ships no copy of that dataset, so
// "never fetched" is ordinary - and a deployment that has quietly lost
// the known-bot signal is the failure this is here to prevent.
func TestBotDataCheckDistinguishesNeverFetchedFromMissing(t *testing.T) {
	dir := t.TempDir()

	// Not told where to look: skip, not fail. "We did not look" and "we
	// looked and it was missing" are different facts.
	if got := checkBotData("", nil); got.Status != CheckSkip {
		t.Errorf("an unconfigured path gave %q, want skip", got.Status)
	}

	// Configured but never fetched: a warning that says what is off.
	missing := filepath.Join(dir, "nope.json")
	never := checkBotData(missing, nil)
	if never.Status != CheckWarn {
		t.Errorf("a never-fetched set gave %q, want warn", never.Status)
	}
	if !strings.Contains(never.Detail, "dağıtmıyor") {
		t.Errorf("the detail does not explain why it is absent: %q", never.Detail)
	}
	if never.Fix == "" {
		t.Error("the check does not say how to fix it")
	}
	// Never required: blocking an installation over a third party's data
	// would be the wrong trade.
	if never.Severity != SeverityRecommended {
		t.Errorf("severity is %q; this must not block an installation", never.Severity)
	}

	// Fetched today: pass.
	fresh := filepath.Join(dir, "fresh.json")
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	if err := botdata.Save(fresh, botdata.Set{
		Labels: map[string]string{"ja4-a": "curl"}, Source: "https://example.test",
		FetchedAt: now.Add(-2 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if got := checkBotData(fresh, func() time.Time { return now }); got.Status != CheckPass {
		t.Errorf("a fresh set gave %q (%s)", got.Status, got.Detail)
	}

	// Fetched long ago: warn, but never fail - last month's fingerprints
	// are far better than none.
	stale := filepath.Join(dir, "stale.json")
	if err := botdata.Save(stale, botdata.Set{
		Labels: map[string]string{"ja4-a": "curl"}, FetchedAt: now.Add(-90 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	old := checkBotData(stale, func() time.Time { return now })
	if old.Status != CheckWarn {
		t.Errorf("a stale set gave %q, want warn", old.Status)
	}
	if !strings.Contains(old.Detail, "90") {
		t.Errorf("the detail does not say how stale: %q", old.Detail)
	}

	// A file that exists and cannot be read is a real failure.
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{ truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := checkBotData(broken, nil); got.Status != CheckFail {
		t.Errorf("an unreadable file gave %q, want fail", got.Status)
	}
}

package preflight

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
	"github.com/cruciblelab/crucible-analytic/internal/botdata"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
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

// TestDeveloperPasswordCheckNeedsTheKeyList pins the consequence of
// moving GuardedKeys out to the caller.
//
// The check names the frozen settings, which is the only reason it is
// worth printing at all - "no developer password" without the list is a
// fact nobody can act on. Once the list arrives through Config, a caller
// that forgets to supply it would previously have produced a confident
// "0 ayar şifre soruyor". That is worse than the check not running, so
// an empty list is a skip.
func TestDeveloperPasswordCheckNeedsTheKeyList(t *testing.T) {
	// Hashed here rather than pasted in as a constant: a literal hash in
	// a test file is a credential-shaped string somebody eventually
	// copies somewhere real.
	hash, err := argon2id.Hash("bu bir test parolasi, kimse kullanmasin")
	if err != nil {
		t.Fatalf("argon2id.Hash: %v", err)
	}
	configured, err := devgate.New(
		devgate.Config{PasswordHash: hash},
		devgate.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("devgate.New: %v", err)
	}
	unconfigured, err := devgate.New(devgate.Config{},
		devgate.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("devgate.New: %v", err)
	}

	keys := []string{"privacy.ip_storage", "analytics.retention_days"}

	// No gate at all: the wizard was not told, which is not the same as
	// there being no password.
	if got := checkDeveloperPassword(nil, keys); got.Status != CheckSkip {
		t.Errorf("a missing gate gave %q, want skip", got.Status)
	}

	// A gate but no key list: skip rather than a reassuring count of
	// nothing.
	if got := checkDeveloperPassword(configured, nil); got.Status != CheckSkip {
		t.Errorf("an empty key list gave %q, want skip - a check that says '0 ayar' has "+
			"answered a question nobody asked", got.Status)
	}

	// No password: warn, and name every frozen setting so the reader can
	// act on it.
	warn := checkDeveloperPassword(unconfigured, keys)
	if warn.Status != CheckWarn {
		t.Fatalf("an unconfigured gate gave %q, want warn", warn.Status)
	}
	for _, key := range keys {
		if !strings.Contains(warn.Detail, key) {
			t.Errorf("the warning does not name %q, so nobody can tell what is frozen: %q", key, warn.Detail)
		}
	}
	if warn.Fix == "" {
		t.Error("the warning offers no command to set a password")
	}
	if warn.Severity != SeverityRecommended {
		t.Errorf("severity is %q; freezing those settings at their privacy-preserving defaults "+
			"is a defensible choice and must not block handover", warn.Severity)
	}

	if got := checkDeveloperPassword(configured, keys); got.Status != CheckPass {
		t.Errorf("a configured gate gave %q (%s)", got.Status, got.Detail)
	}
}

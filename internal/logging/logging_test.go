package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestTree builds a tree under a temp directory with a clock the test
// controls, so day rolling and retention can be exercised without
// waiting.
func newTestTree(t *testing.T, now *time.Time) *Tree {
	t.Helper()
	tree, err := NewTree(TreeConfig{
		Root:          t.TempDir(),
		Service:       "testsvc",
		RetentionDays: 3,
		Now:           func() time.Time { return *now },
	})
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return tree
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %q is not valid JSON: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}

func TestTree_WritesOneFilePerCategory(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	logger := slog.New(NewHandler(tree, HandlerConfig{}))

	logger.Info("started", In(CategoryApp))
	logger.Info("login refused", In(CategoryAuth))
	logger.Info("event kept", In(CategoryIngest))

	day := tree.DayDir("2026-05-06")
	for _, category := range []Category{CategoryApp, CategoryAuth, CategoryIngest} {
		path := filepath.Join(day, string(category)+".log")
		if lines := readLines(t, path); len(lines) != 1 {
			t.Errorf("%s has %d lines, want 1", path, len(lines))
		}
	}

	// A category nobody wrote to must not exist: an empty file suggests
	// "we looked and found nothing", which is the distinction this
	// project refuses to blur elsewhere too.
	if _, err := os.Stat(filepath.Join(day, "audit.log")); !os.IsNotExist(err) {
		t.Errorf("audit.log exists without anything having been written to it")
	}
}

func TestHandler_RecordWithoutACategoryLandsInApp(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	slog.New(NewHandler(tree, HandlerConfig{})).Info("no category here")

	lines := readLines(t, filepath.Join(tree.DayDir("2026-05-06"), "app.log"))
	if len(lines) != 1 || lines[0]["msg"] != "no category here" {
		t.Errorf("app.log = %+v, want the uncategorised record", lines)
	}
}

// "Did anything go wrong today" has to be one file, not a search across
// nine.
func TestHandler_MirrorsWarningsIntoError(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	logger := slog.New(NewHandler(tree, HandlerConfig{}))

	logger.Info("fine", In(CategoryAuth))
	logger.Warn("suspicious", In(CategoryAuth))
	logger.Error("broken", In(CategoryIngest))

	day := tree.DayDir("2026-05-06")
	if lines := readLines(t, filepath.Join(day, "auth.log")); len(lines) != 2 {
		t.Errorf("auth.log has %d lines, want 2 (the info and the warning)", len(lines))
	}
	errors := readLines(t, filepath.Join(day, "error.log"))
	if len(errors) != 2 {
		t.Fatalf("error.log has %d lines, want 2 (the warning and the error)", len(errors))
	}
	for _, line := range errors {
		if line["msg"] == "fine" {
			t.Error("an INFO record was mirrored into error.log")
		}
	}
}

// A record already filed as an error must not be copied to itself.
func TestHandler_DoesNotMirrorErrorCategoryOntoItself(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	slog.New(NewHandler(tree, HandlerConfig{})).Error("once", In(CategoryError))

	if lines := readLines(t, filepath.Join(tree.DayDir("2026-05-06"), "error.log")); len(lines) != 1 {
		t.Errorf("error.log has %d lines, want 1", len(lines))
	}
}

func TestTree_RollsIntoTheNextDayDirectory(t *testing.T) {
	now := time.Date(2026, 5, 6, 23, 59, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	logger := slog.New(NewHandler(tree, HandlerConfig{}))

	logger.Info("before midnight", In(CategoryApp))
	now = now.Add(2 * time.Minute) // 2026-05-07 00:01
	logger.Info("after midnight", In(CategoryApp))

	before := readLines(t, filepath.Join(tree.DayDir("2026-05-06"), "app.log"))
	after := readLines(t, filepath.Join(tree.DayDir("2026-05-07"), "app.log"))
	if len(before) != 1 || before[0]["msg"] != "before midnight" {
		t.Errorf("2026-05-06/app.log = %+v", before)
	}
	if len(after) != 1 || after[0]["msg"] != "after midnight" {
		t.Errorf("2026-05-07/app.log = %+v", after)
	}
}

func TestTree_PruneDropsOldDaysAndKeepsRecentOnes(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now) // RetentionDays: 3

	// Days created by hand, as an older run would have left them.
	for _, day := range []string{"2026-05-01", "2026-05-05", "2026-05-09"} {
		if err := os.MkdirAll(tree.DayDir(day), dirPerm); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	removed, err := tree.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d day directories, want 2", removed)
	}
	for _, gone := range []string{"2026-05-01", "2026-05-05"} {
		if _, err := os.Stat(tree.DayDir(gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived pruning", gone)
		}
	}
	for _, kept := range []string{"2026-05-09", "2026-05-10"} {
		if _, err := os.Stat(tree.DayDir(kept)); err != nil {
			t.Errorf("%s was pruned but should have been kept: %v", kept, err)
		}
	}
}

// Pruning must never remove the directory currently being written to,
// whatever an odd clock says.
func TestTree_PruneNeverRemovesTheActiveDay(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	tree, err := NewTree(TreeConfig{
		Root: t.TempDir(), Service: "svc", RetentionDays: 1,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })

	slog.New(NewHandler(tree, HandlerConfig{})).Info("today", In(CategoryApp))
	// Jump the clock far forward: today's directory is now "old".
	now = now.AddDate(0, 0, 30)

	if _, err := tree.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(tree.DayDir("2026-05-10")); err != nil {
		t.Errorf("the active day directory was pruned: %v", err)
	}
}

func TestTree_PruneIgnoresDirectoriesItDidNotCreate(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)

	foreign := filepath.Join(tree.serviceDir(), "not-a-date")
	if err := os.MkdirAll(foreign, dirPerm); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := tree.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("pruning removed a directory it did not create: %v", err)
	}
}

// Log injection: a value containing a newline would otherwise end the
// record and let the rest parse as a second one - an attacker forging
// entries in the file the operator reads to find out what they did.
func TestHandler_UntrustedValueCannotForgeASecondRecord(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	logger := slog.New(NewHandler(tree, HandlerConfig{}))

	hostile := "normal\n{\"level\":\"INFO\",\"msg\":\"forged entry\"}"
	logger.Info("request", In(CategoryAccess), slog.String("user_agent", hostile))

	lines := readLines(t, filepath.Join(tree.DayDir("2026-05-06"), "access.log"))
	if len(lines) != 1 {
		t.Fatalf("one call produced %d records; a value forged an entry", len(lines))
	}
	if ua, _ := lines[0]["user_agent"].(string); strings.Contains(ua, "\n") {
		t.Error("a newline survived into a logged value")
	}
}

func TestHandler_RedactsAnythingThatLooksLikeACredential(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	logger := slog.New(NewHandler(tree, HandlerConfig{}))

	logger.Info("login", In(CategoryAuth),
		slog.String("password", "hunter2"),
		slog.String("session_token", "abc123"),
		slog.String("api_key", "sk-live-xyz"),
		slog.String("email", "ahmet@example.com"))

	lines := readLines(t, filepath.Join(tree.DayDir("2026-05-06"), "auth.log"))
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	for _, key := range []string{"password", "session_token", "api_key"} {
		if got := lines[0][key]; got != Redacted {
			t.Errorf("%s = %v, want %q", key, got, Redacted)
		}
	}
	// Redaction must not be so broad that it eats ordinary fields.
	if lines[0]["email"] != "ahmet@example.com" {
		t.Errorf("email = %v, want it kept", lines[0]["email"])
	}

	raw, err := os.ReadFile(filepath.Join(tree.DayDir("2026-05-06"), "auth.log"))
	if err != nil {
		t.Fatalf("reading auth.log: %v", err)
	}
	for _, secret := range []string{"hunter2", "abc123", "sk-live-xyz"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("the secret %q reached the file", secret)
		}
	}
}

// A lazily-computed value must be resolved before the key is judged, or
// a LogValuer could smuggle a secret past redaction.
type lazySecret struct{}

func (lazySecret) LogValue() slog.Value { return slog.StringValue("late-secret") }

func TestHandler_RedactsLazilyComputedSecrets(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	slog.New(NewHandler(tree, HandlerConfig{})).Info("x", In(CategoryAuth),
		slog.Any("refresh_token", lazySecret{}))

	raw, err := os.ReadFile(filepath.Join(tree.DayDir("2026-05-06"), "auth.log"))
	if err != nil {
		t.Fatalf("reading auth.log: %v", err)
	}
	if bytes.Contains(raw, []byte("late-secret")) {
		t.Error("a LogValuer smuggled a secret past redaction")
	}
}

func TestTree_RejectsUnknownCategoryRatherThanCreatingAFile(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)

	if err := tree.Write(Category("../../etc/passwd"), []byte("{}")); err == nil {
		t.Error("Write accepted a category that is a path traversal")
	}
	if err := tree.Write(Category("made-up"), []byte("{}")); err == nil {
		t.Error("Write accepted an unknown category")
	}
}

func TestNewTree_RejectsAnUnsafeServiceName(t *testing.T) {
	for _, name := range []string{"", "..", "a/b", "with space", strings.Repeat("x", 65)} {
		if _, err := NewTree(TreeConfig{Root: t.TempDir(), Service: name}); err == nil {
			t.Errorf("NewTree accepted service name %q", name)
		}
	}
}

func TestTree_FilesRejectsADayThatIsNotADate(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	// The day reaches this from a panel request, so it is a claim like
	// any other.
	if _, err := tree.Files("../../.."); err == nil {
		t.Error("Files accepted a traversal as a day")
	}
}

func TestTree_RotatesAnOversizedCategoryFile(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree, err := NewTree(TreeConfig{
		Root: t.TempDir(), Service: "svc", MaxFileBytes: 256,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })

	logger := slog.New(NewHandler(tree, HandlerConfig{}))
	for i := 0; i < 20; i++ {
		logger.Info("a reasonably long message to push the file over the cap", In(CategoryApp))
	}

	files, err := tree.Files("2026-05-06")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) < 2 {
		t.Errorf("files = %v, want app.log plus at least one rotated file", files)
	}
}

func TestTree_PermissionsKeepLogsPrivate(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	slog.New(NewHandler(tree, HandlerConfig{})).Info("x", In(CategoryApp))

	// Log lines carry IP addresses and user agents, so they are personal
	// data under the same reading as the analytics tables.
	info, err := os.Stat(filepath.Join(tree.DayDir("2026-05-06"), "app.log"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("app.log mode = %o, want %o", perm, filePerm)
	}
	dir, err := os.Stat(tree.DayDir("2026-05-06"))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != dirPerm {
		t.Errorf("day directory mode = %o, want %o", perm, dirPerm)
	}
}

// Every service logs from many goroutines; two rotating the same file at
// once would interleave a line.
func TestHandler_ConcurrentWritesProduceWholeLines(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	logger := slog.New(NewHandler(tree, HandlerConfig{}))

	var wg sync.WaitGroup
	const writers, each = 8, 50
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				logger.Info("concurrent", In(CategoryApp), slog.Int("writer", w), slog.Int("seq", i))
			}
		}(w)
	}
	wg.Wait()

	// readLines fails the test if any line is not valid JSON, which is
	// what a torn write would produce.
	lines := readLines(t, filepath.Join(tree.DayDir("2026-05-06"), "app.log"))
	if len(lines) != writers*each {
		t.Errorf("got %d records, want %d", len(lines), writers*each)
	}
}

// A failed write must not take the service down with it.
func TestHandler_DiskFailureFallsBackInsteadOfFailing(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)

	var fallback bytes.Buffer
	handler := NewHandler(tree, HandlerConfig{Fallback: &fallback})

	// Make the day directory unwritable by replacing it with a file.
	day := tree.DayDir("2026-05-06")
	if err := os.RemoveAll(day); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.WriteFile(day, []byte("not a directory"), filePerm); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	slog.New(handler).Info("this cannot be written", In(CategoryApp))

	if fallback.Len() == 0 {
		t.Error("a write failure produced no fallback output")
	}
	if !strings.Contains(fallback.String(), "this cannot be written") {
		t.Errorf("fallback = %q, want it to carry the message", fallback.String())
	}
}

func TestTrust_CarriesBothTheClaimAndTheVerdict(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	logger := slog.New(NewHandler(tree, HandlerConfig{}))

	logger.Warn("site claim refused",
		Trust(CategorySecurity, "somebody-elses-site", VerdictRejected, "not in the configured allowlist")...)

	lines := readLines(t, filepath.Join(tree.DayDir("2026-05-06"), "security.log"))
	if len(lines) != 1 {
		t.Fatalf("security.log has %d lines, want 1", len(lines))
	}
	// Both halves, or the record cannot answer "what did they claim and
	// what did we conclude".
	if lines[0][KeyClaim] != "somebody-elses-site" {
		t.Errorf("%s = %v", KeyClaim, lines[0][KeyClaim])
	}
	if lines[0][KeyVerdict] != VerdictRejected {
		t.Errorf("%s = %v", KeyVerdict, lines[0][KeyVerdict])
	}
	if lines[0][KeyReason] == "" {
		t.Error("the verdict carries no reason")
	}
	// And it must have been mirrored, being a warning.
	if lines := readLines(t, filepath.Join(tree.DayDir("2026-05-06"), "error.log")); len(lines) != 1 {
		t.Errorf("error.log has %d lines, want the mirrored warning", len(lines))
	}
}

func TestAttempt_LogsSuccessesAsWellAsFailures(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)
	logger := slog.New(NewHandler(tree, HandlerConfig{}))

	logger.Info("login", Attempt("ahmet@example.com", VerdictAccepted, "password and second factor", "203.0.113.7")...)
	logger.Warn("login", Attempt("ahmet@example.com", VerdictRejected, "wrong password", "203.0.113.7")...)

	lines := readLines(t, filepath.Join(tree.DayDir("2026-05-06"), "auth.log"))
	if len(lines) != 2 {
		t.Fatalf("auth.log has %d lines, want 2. A file recording only failures cannot show "+
			"that a success came from an address that had failed forty times an hour earlier.", len(lines))
	}
	if lines[0][KeyVerdict] != VerdictAccepted || lines[1][KeyVerdict] != VerdictRejected {
		t.Errorf("verdicts = %v / %v", lines[0][KeyVerdict], lines[1][KeyVerdict])
	}
	if lines[0][KeyPeer] != "203.0.113.7" {
		t.Errorf("%s = %v, want the peer address", KeyPeer, lines[0][KeyPeer])
	}
}

func TestSanitizeValue_BoundsAndCleans(t *testing.T) {
	if got := SanitizeValue("a\x00b\nc"); strings.ContainsAny(got, "\x00\n") {
		t.Errorf("SanitizeValue left a control character: %q", got)
	}
	long := strings.Repeat("x", maxValueLen+500)
	if got := SanitizeValue(long); len([]rune(got)) > maxValueLen+1 {
		t.Errorf("SanitizeValue returned %d runes, want at most %d", len([]rune(got)), maxValueLen+1)
	}
	if got := SanitizeValue(""); got != "" {
		t.Errorf("SanitizeValue(\"\") = %q", got)
	}
}

func TestIsSecretKey(t *testing.T) {
	for _, key := range []string{"password", "Password", "session_token", "X-API-Key", "totp_secret", "authorization"} {
		if !IsSecretKey(key) {
			t.Errorf("IsSecretKey(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"email", "site_id", "path", "user_agent", "ip", "country"} {
		if IsSecretKey(key) {
			t.Errorf("IsSecretKey(%q) = true; redaction is too broad", key)
		}
	}
}

// Package logging writes this project's logs to a structured directory
// tree instead of a single stream.
//
// The shape is deliberate. One flat log file per service answers "what
// happened" and answers "what happened to authentication last Tuesday"
// only by grepping through everything else, which is exactly the moment
// somebody needs an answer quickly. So the tree separates by the three
// axes that matter when reading rather than when writing:
//
//	<root>/<service>/<YYYY-MM-DD>/<category>.log
//
// Service, because four processes share a machine. Day, because
// retention then becomes deleting a directory rather than rewriting a
// file, and because "what happened on the 14th" is a directory listing.
// Category, because auth attempts, ingest rejections and ordinary
// operation are read by different people looking for different things.
//
// Records are JSON lines. A human can read them, and the panel's log
// viewer (see PLAN.md B1) can parse them without a second format.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Category names one log stream inside a day's directory.
//
// A closed set of constants rather than a free string: the value becomes
// a filename, and a caller-supplied filename is a path-traversal
// primitive. Adding one is a code change that goes through review, which
// is the same rule the panel's repair operations follow.
type Category string

const (
	// CategoryApp is ordinary operation: startup, shutdown, flushes.
	CategoryApp Category = "app"
	// CategoryError mirrors every WARN-and-above record from any other
	// category, so "did anything go wrong today" is one file rather than
	// a search across all of them.
	CategoryError Category = "error"
	// CategorySecurity records trust decisions - what a client claimed
	// and what the server concluded. See Security.
	CategorySecurity Category = "security"
	// CategoryAuth records every authentication attempt, successful or
	// not. Separate from security because a locked-out customer and an
	// attack in progress are read at different times.
	CategoryAuth Category = "auth"
	// CategoryAccess records served requests.
	CategoryAccess Category = "access"
	// CategoryIngest records accepted events.
	CategoryIngest Category = "ingest"
	// CategoryRejected records refused input and why. Its own file
	// because "the customer says data is missing" is answered here and
	// nowhere else.
	CategoryRejected Category = "rejected"
	// CategoryAudit records who changed what.
	CategoryAudit Category = "audit"
	// CategoryQuery records read queries and their timing.
	CategoryQuery Category = "query"
)

// categories is every valid category. Anything not in this list is
// rejected rather than turned into a file.
var categories = []Category{
	CategoryApp, CategoryError, CategorySecurity, CategoryAuth,
	CategoryAccess, CategoryIngest, CategoryRejected, CategoryAudit,
	CategoryQuery,
}

// Valid reports whether c is a known category.
func (c Category) Valid() bool {
	for _, known := range categories {
		if c == known {
			return true
		}
	}
	return false
}

// Defaults for TreeConfig.
const (
	// DefaultMaxFileBytes rotates a single category file once it reaches
	// this size, so one noisy day cannot produce a file too large to
	// open.
	DefaultMaxFileBytes = 64 << 20 // 64 MiB
	// DefaultRetentionDays is how many day-directories are kept. Short,
	// because logs are the fastest-growing thing this project writes and
	// a full disk stops the collector - see PLAN.md's retention section.
	DefaultRetentionDays = 14
	// dirPerm and filePerm keep logs readable only by the user running
	// the service. Log lines carry IP addresses and user agents, so they
	// are personal data under the same reading as the analytics tables.
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// TreeConfig configures a Tree. Zero values take the defaults above.
type TreeConfig struct {
	// Root is the directory everything lives under.
	Root string
	// Service names the subdirectory, e.g. "collector".
	Service string
	// MaxFileBytes rotates a category file once it exceeds this.
	MaxFileBytes int64
	// RetentionDays is how many day-directories to keep. Zero or
	// negative disables deletion, which is a choice a deployment can
	// make and not one it should make by accident.
	RetentionDays int
	// Now supplies the clock, for tests.
	Now func() time.Time
}

// Tree owns the on-disk directory and the open files inside it.
//
// Safe for concurrent use: every service logs from many goroutines, and
// two of them rotating the same file at once would interleave a line.
type Tree struct {
	cfg TreeConfig

	mu   sync.Mutex
	open map[Category]*os.File
	// day is the directory currently open, so a process running past
	// midnight rolls into the next one rather than writing tomorrow's
	// records into yesterday's folder.
	day string
}

// NewTree prepares the directory for today and returns the tree.
func NewTree(cfg TreeConfig) (*Tree, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("logging: root is required")
	}
	if !validServiceName(cfg.Service) {
		return nil, fmt.Errorf("logging: invalid service name %q (want 1-64 characters, letters/digits/underscore/dash)", cfg.Service)
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = DefaultMaxFileBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	t := &Tree{cfg: cfg, open: make(map[Category]*os.File, len(categories))}
	if err := os.MkdirAll(t.serviceDir(), dirPerm); err != nil {
		return nil, fmt.Errorf("logging: create %s: %w", t.serviceDir(), err)
	}
	if err := t.rollTo(t.today()); err != nil {
		return nil, err
	}
	return t, nil
}

// validServiceName bounds the one path segment a caller supplies.
// Narrow on purpose: it becomes a directory name, and ".." would be a
// traversal.
func validServiceName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (t *Tree) today() string { return t.cfg.Now().UTC().Format("2006-01-02") }

func (t *Tree) serviceDir() string { return filepath.Join(t.cfg.Root, t.cfg.Service) }

// DayDir is the directory holding a given day's category files.
func (t *Tree) DayDir(day string) string { return filepath.Join(t.serviceDir(), day) }

// rollTo closes the current day's files and opens the next day's
// directory. The caller holds no lock; callers inside the lock use
// rollToLocked.
func (t *Tree) rollTo(day string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rollToLocked(day)
}

func (t *Tree) rollToLocked(day string) error {
	for category, f := range t.open {
		_ = f.Close()
		delete(t.open, category)
	}
	if err := os.MkdirAll(t.DayDir(day), dirPerm); err != nil {
		return fmt.Errorf("logging: create %s: %w", t.DayDir(day), err)
	}
	t.day = day
	return nil
}

// Write appends one already-encoded line to a category's file.
//
// The line must be a complete record without a trailing newline; Write
// adds it. Rolling the day and rotating an oversized file both happen
// here, so a caller never has to think about either.
func (t *Tree) Write(category Category, line []byte) error {
	if !category.Valid() {
		return fmt.Errorf("logging: unknown category %q", category)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if today := t.today(); today != t.day {
		if err := t.rollToLocked(today); err != nil {
			return err
		}
	}

	f, err := t.fileLocked(category)
	if err != nil {
		return err
	}

	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("logging: write %s: %w", category, err)
	}
	return nil
}

// fileLocked returns the open handle for a category, opening or
// rotating it as needed. Caller holds t.mu.
func (t *Tree) fileLocked(category Category) (*os.File, error) {
	f, ok := t.open[category]
	if ok {
		info, err := f.Stat()
		if err == nil && info.Size() < t.cfg.MaxFileBytes {
			return f, nil
		}
		if err == nil {
			// Oversized: close, move aside, reopen empty.
			_ = f.Close()
			delete(t.open, category)
			if err := t.rotateLocked(category); err != nil {
				return nil, err
			}
		}
	}

	path := filepath.Join(t.DayDir(t.day), string(category)+".log")
	opened, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("logging: open %s: %w", path, err)
	}
	t.open[category] = opened
	return opened, nil
}

// rotateLocked renames a full category file out of the way, choosing
// the lowest free sequence number so ordering is readable.
func (t *Tree) rotateLocked(category Category) error {
	base := filepath.Join(t.DayDir(t.day), string(category))
	for n := 1; n < 10_000; n++ {
		candidate := fmt.Sprintf("%s.%d.log", base, n)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return os.Rename(base+".log", candidate)
		}
	}
	return fmt.Errorf("logging: too many rotated files for %s", category)
}

// Prune deletes day-directories older than RetentionDays.
//
// Deliberately a directory removal rather than a line-by-line trim: a
// day is a unit, dropping one is cheap, and there is no partially
// pruned state to reason about. Returns how many were removed.
func (t *Tree) Prune() (int, error) {
	if t.cfg.RetentionDays <= 0 {
		return 0, nil
	}

	entries, err := os.ReadDir(t.serviceDir())
	if err != nil {
		return 0, fmt.Errorf("logging: read %s: %w", t.serviceDir(), err)
	}

	cutoff := t.cfg.Now().UTC().AddDate(0, 0, -t.cfg.RetentionDays)
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		day, err := time.Parse("2006-01-02", entry.Name())
		if err != nil {
			// Not one of ours. Leaving it alone is the safe reading of a
			// directory this process did not create.
			continue
		}
		if !day.Before(cutoff) {
			continue
		}
		// Never delete the day currently being written to, whatever the
		// clock says.
		if entry.Name() == t.day {
			continue
		}
		if err := os.RemoveAll(t.DayDir(entry.Name())); err != nil {
			return removed, fmt.Errorf("logging: prune %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

// Days lists the day-directories present, oldest first. For the panel's
// log browser.
func (t *Tree) Days() ([]string, error) {
	entries, err := os.ReadDir(t.serviceDir())
	if err != nil {
		return nil, fmt.Errorf("logging: read %s: %w", t.serviceDir(), err)
	}
	days := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := time.Parse("2006-01-02", entry.Name()); err != nil {
			continue
		}
		days = append(days, entry.Name())
	}
	sort.Strings(days)
	return days, nil
}

// Files lists the category files present for a day, for the same
// browser. Rotated files (`auth.1.log`) are included.
func (t *Tree) Files(day string) ([]string, error) {
	if _, err := time.Parse("2006-01-02", day); err != nil {
		// The day reaches this from a panel request, so it is a claim
		// like any other: parsed and rejected here rather than joined
		// into a path and hoped about.
		return nil, fmt.Errorf("logging: invalid day %q", day)
	}
	entries, err := os.ReadDir(t.DayDir(day))
	if err != nil {
		return nil, fmt.Errorf("logging: read %s: %w", t.DayDir(day), err)
	}
	files := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		files = append(files, entry.Name())
	}
	sort.Strings(files)
	return files, nil
}

// Close releases every open file.
func (t *Tree) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var firstErr error
	for category, f := range t.open {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(t.open, category)
	}
	return firstErr
}

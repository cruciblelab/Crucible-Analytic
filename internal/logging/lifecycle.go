package logging

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A log's life has three stages, not one, and the stages exist because
// not all logs are equally worth keeping.
//
// access and ingest are enormous and interesting for about a week.
// security, auth and audit are small and are exactly what somebody asks
// for a year later, when the question is "who got in, and when". A
// single retention figure would either throw the second group away or
// keep the first group forever, and the first group is the one that
// fills a disk - which stops the collector, which is an analytics
// feature taking down the traffic path.
//
// So: plain text while it is being read, compressed once it is history,
// deleted when its own category says so.

// importantCategories are kept far longer than the rest. They are the
// small ones, and they are the ones a question arrives about long after
// the fact.
var importantCategories = map[Category]bool{
	CategorySecurity: true,
	CategoryAuth:     true,
	CategoryAudit:    true,
}

// Important reports whether a category is kept on the long schedule.
func (c Category) Important() bool { return importantCategories[c] }

// Lifecycle is the retention policy a Tree is maintained against.
type Lifecycle struct {
	// ArchiveAfterDays compresses a day's files once they are older than
	// this. A gzipped day is roughly a tenth of the size and still
	// readable, so keeping a year of security logs costs about what five
	// weeks of them cost uncompressed.
	//
	// Zero or negative disables compression.
	ArchiveAfterDays int
	// RetentionDays is how long an ordinary category is kept.
	RetentionDays int
	// ImportantRetentionDays is how long security, auth and audit are
	// kept.
	ImportantRetentionDays int
}

// DefaultLifecycle is what a deployment gets before anyone changes
// anything. The numbers match the defaults in internal/panel's setting
// registry, and the panel is where they are meant to be changed.
func DefaultLifecycle() Lifecycle {
	return Lifecycle{
		ArchiveAfterDays:       7,
		RetentionDays:          14,
		ImportantRetentionDays: 365,
	}
}

// retentionFor returns how long a category is kept.
func (l Lifecycle) retentionFor(c Category) int {
	if c.Important() {
		return l.ImportantRetentionDays
	}
	return l.RetentionDays
}

// MaintenanceReport says what a maintenance pass actually did, so the
// panel can show it and the operation journal can record it.
type MaintenanceReport struct {
	// Archived is how many files were compressed.
	Archived int
	// Deleted is how many files were removed.
	Deleted int
	// RemovedDays is how many day directories were removed once empty.
	RemovedDays int
	// BytesReclaimed is the difference compression and deletion made.
	BytesReclaimed int64
}

// Maintain runs one pass of the lifecycle: compress what is old enough
// to archive, delete what is past its category's retention, and remove
// day directories left empty.
//
// Ordinary maintenance rather than a cron job with its own opinions: it
// is called at startup and by the panel's repair operation, and it is
// safe to run at any time because the day currently being written to is
// never touched.
func (t *Tree) Maintain(policy Lifecycle) (MaintenanceReport, error) {
	var report MaintenanceReport

	days, err := t.Days()
	if err != nil {
		return report, err
	}

	now := t.cfg.Now().UTC()
	today := t.today()

	for _, day := range days {
		// Never the day being written to, whatever the clock says. Its
		// files are open, and compressing an open file would truncate
		// the handle the process is still appending through.
		if day == today {
			continue
		}
		parsed, err := time.Parse("2006-01-02", day)
		if err != nil {
			continue
		}
		age := int(now.Sub(parsed).Hours() / 24)

		entries, err := os.ReadDir(t.DayDir(day))
		if err != nil {
			return report, fmt.Errorf("logging: read %s: %w", t.DayDir(day), err)
		}

		remaining := 0
		for _, entry := range entries {
			if entry.IsDir() {
				remaining++
				continue
			}
			category, ok := categoryOfFile(entry.Name())
			if !ok {
				// Not a file this package created. Leaving it alone is
				// the safe reading of something another process put here.
				remaining++
				continue
			}

			path := filepath.Join(t.DayDir(day), entry.Name())

			if retention := policy.retentionFor(category); retention > 0 && age >= retention {
				size := fileSize(path)
				if err := os.Remove(path); err != nil {
					return report, fmt.Errorf("logging: delete %s: %w", path, err)
				}
				report.Deleted++
				report.BytesReclaimed += size
				continue
			}

			if policy.ArchiveAfterDays > 0 && age >= policy.ArchiveAfterDays && !strings.HasSuffix(entry.Name(), ".gz") {
				saved, err := compressInPlace(path)
				if err != nil {
					return report, err
				}
				report.Archived++
				report.BytesReclaimed += saved
			}
			remaining++
		}

		// A day whose every file has aged out leaves an empty directory
		// behind; removing it keeps Days() meaning "days with logs".
		if remaining == 0 {
			if err := os.Remove(t.DayDir(day)); err == nil {
				report.RemovedDays++
			}
		}
	}
	return report, nil
}

// categoryOfFile recovers the category from a filename, accepting the
// rotated ("auth.3.log") and archived ("auth.log.gz") forms as well as
// the plain one.
//
// The filename is not trusted input - this package wrote it - but the
// directory it was read from is on disk, where anything could put a
// file. An unrecognised name reports false and is left alone rather than
// being guessed at.
func categoryOfFile(name string) (Category, bool) {
	base := strings.TrimSuffix(name, ".gz")
	if !strings.HasSuffix(base, ".log") {
		return "", false
	}
	base = strings.TrimSuffix(base, ".log")
	// Strip a rotation sequence: "auth.3" -> "auth".
	if i := strings.LastIndex(base, "."); i >= 0 {
		if _, err := time.Parse("2006", base[i+1:]); err != nil {
			// Not a year; treat it as a rotation number if it is digits.
			if isDigits(base[i+1:]) {
				base = base[:i]
			}
		}
	}
	category := Category(base)
	if !category.Valid() {
		return "", false
	}
	return category, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// compressInPlace gzips a file and removes the original, returning how
// many bytes were saved.
//
// Written to a temporary file and renamed rather than compressed over
// the original: a crash halfway through the first shape leaves a
// truncated log, and a log that is silently half missing is worse than
// one that is simply large.
func compressInPlace(path string) (int64, error) {
	before := fileSize(path)

	source, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("logging: open %s: %w", path, err)
	}
	defer source.Close()

	tmp := path + ".gz.tmp"
	target, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
	if err != nil {
		return 0, fmt.Errorf("logging: create %s: %w", tmp, err)
	}

	writer := gzip.NewWriter(target)
	if _, err := io.Copy(writer, source); err != nil {
		writer.Close()
		target.Close()
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("logging: compress %s: %w", path, err)
	}
	if err := writer.Close(); err != nil {
		target.Close()
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("logging: finish compressing %s: %w", path, err)
	}
	if err := target.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("logging: close %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path+".gz"); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("logging: rename %s: %w", tmp, err)
	}
	if err := os.Remove(path); err != nil {
		return 0, fmt.Errorf("logging: remove %s after compressing: %w", path, err)
	}

	after := fileSize(path + ".gz")
	return before - after, nil
}

// Usage reports how much disk this service's logs occupy, split by
// whether it is still plain text. For the health page: "how large are
// the logs" is asked before "delete some of them".
type Usage struct {
	PlainBytes      int64
	CompressedBytes int64
	Days            int
	Files           int
}

// Total is the whole footprint.
func (u Usage) Total() int64 { return u.PlainBytes + u.CompressedBytes }

// Usage walks the tree and measures it.
func (t *Tree) Usage() (Usage, error) {
	var usage Usage
	days, err := t.Days()
	if err != nil {
		return usage, err
	}
	usage.Days = len(days)

	for _, day := range days {
		entries, err := os.ReadDir(t.DayDir(day))
		if err != nil {
			return usage, fmt.Errorf("logging: read %s: %w", t.DayDir(day), err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			usage.Files++
			if strings.HasSuffix(entry.Name(), ".gz") {
				usage.CompressedBytes += info.Size()
			} else {
				usage.PlainBytes += info.Size()
			}
		}
	}
	return usage, nil
}

package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/diskspace"
)

// ManifestName is the entry every backup carries first.
const ManifestName = "manifest.json"

// Manifest is what the file says about itself.
//
// The last entry in the archive rather than the first: it carries row
// counts, and those are only known once the rows have gone by. See
// Write.
type Manifest struct {
	// TakenAt is when the copy started, from the database's clock.
	//
	// The database's, for the reason Doorbell.Since gives: every other
	// timestamp in this product comes from there, and one that did not
	// would be comparable with none of them.
	TakenAt time.Time `json:"alindi"`

	// Sets is what was asked for, and Tables is what that resolved to.
	// Both, because the sets are the customer's words and the tables are
	// the file's contents, and a set that gains a table later must not
	// make an old file look incomplete.
	Sets   []string `json:"kumeler"`
	Tables []Table  `json:"tablolar"`

	// BinaryVersion and SchemaVersion are what produced it.
	//
	// The schema version is the one a restore has to agree with. The
	// binary version is what an operator recognises, and is the only
	// thing in here that answers "which release was this machine
	// running".
	BinaryVersion string `json:"surum"`
	SchemaVersion int    `json:"sema_surumu"`
}

// Table is one table's entry.
type Table struct {
	Name string `json:"ad"`
	// Columns in the order the COPY data has them.
	//
	// Named rather than implied by SELECT *, so a restore can write
	// `COPY t (a, b, c) FROM STDIN` and land the right values in the
	// right places even after a column is added to the table. Without
	// this, a file taken before an ALTER TABLE restores every value one
	// column to the left, silently, into a table whose types happen to
	// accept them.
	Columns []string `json:"sutunlar"`
	// Rows is what was copied, for a restore to check against.
	Rows int64 `json:"satir"`
	// Bytes is the uncompressed COPY data.
	Bytes int64 `json:"bayt"`
}

// Result is what Write produced.
type Result struct {
	Path     string
	Bytes    int64
	SHA256   string
	Manifest Manifest
	// Device is the kernel's number for the filesystem the file landed
	// on, and zero when it could not be read. See panel_backups.device
	// for why the panel needs it and why it is a number.
	Device int64
}

// Writer produces a backup file.
type Writer struct {
	Pool *pgxpool.Pool
	// Dir is where the file goes. Created 0700 if missing.
	Dir string
	// BinaryVersion is stamped into the manifest.
	BinaryVersion string
	// SchemaVersion likewise.
	SchemaVersion int
}

// Write copies the named tables into a new file and returns what it
// made.
//
// # Written to a temporary name and renamed
//
// A backup interrupted half way - the process killed, the disk full,
// the machine rebooted - leaves a file that is a valid gzip stream, has
// a plausible size, and is missing its last table. Nothing about it
// looks wrong. Under the final name it would sit in the catalogue as a
// backup somebody could rely on.
//
// So the bytes go to `.<name>.tmp` and the file only takes its real name
// after the last table is written, the stream is flushed, and the data
// is on the disk. A crash leaves a dot file that no listing counts and
// the next run removes.
func (w Writer) Write(ctx context.Context, name string, sets []string) (Result, error) {
	tables, err := TablesFor(sets)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(w.Dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("backup: preparing %s: %w", w.Dir, err)
	}
	// And tightened even when it was already there.
	//
	// MkdirAll's mode applies only to directories it creates. A backup
	// directory the operator made by hand, or one left by an earlier
	// install, keeps whatever mode it had - 0755 on most machines - and
	// the 0600 on the files then protects their contents while leaving
	// every backup's name, date and size readable by anyone with an
	// account.
	//
	// Found by a test asserting the directory's mode, not by reading
	// this function: MkdirAll looks like it sets the mode, and it does,
	// on exactly the runs where the directory did not exist yet.
	//
	// Tightened rather than refused, because the alternative is a
	// customer pressing a button and being told to go and chmod
	// something. Nothing but backups belongs in here.
	if err := os.Chmod(w.Dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("backup: restricting %s: %w", w.Dir, err)
	}

	final := filepath.Join(w.Dir, name)
	tmp := filepath.Join(w.Dir, "."+name+".tmp")

	// 0600: the four services run as `crucible` and this file holds
	// every row they are not allowed to read, plus the panel's password
	// hashes and TOTP secrets. Only the account that wrote it may open
	// it, and that is the upgrader's.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("backup: creating %s: %w", tmp, err)
	}
	// Removed on every path that does not rename it. A failed backup
	// that left forty megabytes behind each time somebody retried would
	// fill the disk of the machine least able to spare it.
	defer func() {
		if _, statErr := os.Stat(tmp); statErr == nil {
			_ = os.Remove(tmp)
		}
	}()

	takenAt, err := databaseNow(ctx, w.Pool)
	if err != nil {
		_ = f.Close()
		return Result{}, err
	}

	// Hashed as it is written rather than by reading the file back: the
	// checksum is of what went to the disk, and a second pass could hash
	// a file something else had changed in between.
	sum := sha256.New()
	counted := &countingWriter{w: io.MultiWriter(f, sum)}
	gz := gzip.NewWriter(counted)
	tw := tar.NewWriter(gz)

	manifest := Manifest{
		TakenAt:       takenAt,
		Sets:          Normalise(sets),
		BinaryVersion: w.BinaryVersion,
		SchemaVersion: w.SchemaVersion,
	}

	// The tables first, the manifest last.
	//
	// The manifest carries each table's row count, and a row count is
	// only known after the rows have gone by. tar cannot rewrite an
	// entry, so the choice is: write the manifest first without the
	// counts, or write it last and have a reader scan for it.
	//
	// Last. The counts are what a restore checks itself against, and a
	// manifest that omitted them would leave "did everything arrive"
	// unanswerable - which is the question the whole file exists to be
	// able to answer.
	for _, table := range tables {
		entry, err := w.copyTable(ctx, tw, table)
		if err != nil {
			_ = tw.Close()
			_ = gz.Close()
			_ = f.Close()
			return Result{}, err
		}
		manifest.Tables = append(manifest.Tables, entry)
	}

	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = f.Close()
		return Result{}, fmt.Errorf("backup: writing the manifest: %w", err)
	}
	if err := writeEntry(tw, ManifestName, body); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = f.Close()
		return Result{}, err
	}

	if err := tw.Close(); err != nil {
		_ = f.Close()
		return Result{}, fmt.Errorf("backup: closing the archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return Result{}, fmt.Errorf("backup: closing the compressor: %w", err)
	}
	// Sync before rename, not after.
	//
	// rename is atomic in the directory entry, and says nothing about
	// whether the bytes reached the disk. A machine that loses power
	// between the two comes back with a file under its final name and no
	// contents - which is exactly the shape the temporary name exists to
	// prevent, arrived at from the other side.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return Result{}, fmt.Errorf("backup: flushing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return Result{}, fmt.Errorf("backup: closing %s: %w", tmp, err)
	}
	// Never onto a file that is already there.
	//
	// rename replaces an existing name silently and atomically, which is
	// the property that makes it right for everything above and wrong
	// for exactly this. A backup landing on the name of an older backup
	// destroys it, reports success, and leaves the catalogue with two
	// rows - two dates, two sizes, two checksums - describing one file.
	// The customer sees two backups and has one.
	//
	// Runner.fileName carries milliseconds so this should not be
	// reachable. "Should not be reachable" is the reason to check rather
	// than the reason to skip it: the name is a timestamp, and a clock
	// that steps backwards produces one that has been used before.
	//
	// The gap between this and the rename is a race only another writer
	// could enter, and the queue's one-in-flight index means there is no
	// other writer. It is not the case being defended against.
	if _, err := os.Stat(final); err == nil {
		return Result{}, fmt.Errorf("backup: %s already exists; refusing to write over "+
			"a backup that is already there", final)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("backup: checking %s: %w", final, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return Result{}, fmt.Errorf("backup: naming %s: %w", final, err)
	}
	if err := syncDir(w.Dir); err != nil {
		return Result{}, err
	}

	out := Result{
		Path:     final,
		Bytes:    counted.n,
		SHA256:   hex.EncodeToString(sum.Sum(nil)),
		Manifest: manifest,
	}

	// Which filesystem this landed on, so the panel can draw it into the
	// right bar. See panel_backups.device.
	//
	// Read after the file exists rather than before, because the answer
	// is about the file: a directory created moments ago on a
	// filesystem that has since been remounted would give the wrong one.
	//
	// A failure here is not a failure of the backup. The file is written,
	// synced and named; the device is a label for a picture. So it is
	// left at zero, which the panel reads as "not on any bar I am
	// drawing" rather than as an error.
	if space, err := diskspace.Read(final); err == nil {
		out.Device = int64(space.Device)
	}
	return out, nil
}

// copyTable streams one table into the archive.
//
// Through COPY rather than pg_dump, and the difference is not stylistic:
// see the package comment. COPY is an ordinary query, so a hypertable
// answers it with the rows in its chunks; pg_dump's --table filter does
// not follow chunks and produces an empty file.
func (w Writer) copyTable(ctx context.Context, tw *tar.Writer, table string) (Table, error) {
	cols, err := columnsOf(ctx, w.Pool, table)
	if err != nil {
		return Table{}, err
	}
	if len(cols) == 0 {
		return Table{}, fmt.Errorf("backup: %s has no columns, so it is not a table this "+
			"database has", table)
	}

	// The whole table into memory before the archive entry, because tar
	// wants a size in the header before the body.
	//
	// The alternative is a temporary file per table, and it is what this
	// will need when a customer's traffic table is measured in
	// gigabytes. Recorded rather than hidden: today the largest table
	// this has been run against produced 1.5 MB of COPY text, and the
	// estimate the caller checks against free space is computed from the
	// same numbers - so the first deployment where this matters is one
	// where the estimate already refused.
	var buf sizedBuffer
	// AcquireFunc rather than the pool's own Query: CopyTo is on the
	// underlying connection, and it has to be the same one for the whole
	// stream.
	if err := w.Pool.AcquireFunc(ctx, func(c *pgxpool.Conn) error {
		sql := fmt.Sprintf("COPY (SELECT %s FROM %s) TO STDOUT",
			quoteAll(cols), quoteIdent(table))
		tag, err := c.Conn().PgConn().CopyTo(ctx, &buf, sql)
		if err != nil {
			return err
		}
		buf.rows = tag.RowsAffected()
		return nil
	}); err != nil {
		return Table{}, fmt.Errorf("backup: copying %s: %w", table, err)
	}

	if err := writeEntry(tw, "data/"+table+".copy", buf.b); err != nil {
		return Table{}, err
	}
	return Table{
		Name:    table,
		Columns: cols,
		Rows:    buf.rows,
		Bytes:   int64(len(buf.b)),
	}, nil
}

// columnsOf reads a table's columns in their declared order.
func columnsOf(ctx context.Context, pool *pgxpool.Pool, table string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
		ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("backup: reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("backup: reading the columns of %s: %w", table, err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// databaseNow reads the clock the rest of this product is timed by.
func databaseNow(ctx context.Context, pool *pgxpool.Pool) (time.Time, error) {
	var at time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("backup: reading the database's clock: %w", err)
	}
	return at, nil
}

func writeEntry(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o600,
		Size: int64(len(body)),
		// A fixed time rather than now: two backups of the same data
		// should differ only where the data differs, which is what makes
		// "did anything change" answerable by comparing checksums.
		ModTime: time.Unix(0, 0).UTC(),
		Format:  tar.FormatPAX,
	}); err != nil {
		return fmt.Errorf("backup: writing the header for %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("backup: writing %s: %w", name, err)
	}
	return nil
}

// syncDir makes the rename itself durable.
//
// Without it the file's contents survive a power cut and its name does
// not, which leaves the bytes on the disk under a dot file nothing
// looks at.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("backup: opening %s to flush it: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("backup: flushing %s: %w", dir, err)
	}
	return nil
}

// countingWriter counts what went through it, which is the file's size
// without asking the filesystem afterwards.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// sizedBuffer collects one table's COPY output.
type sizedBuffer struct {
	b    []byte
	rows int64
}

func (s *sizedBuffer) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

// quoteIdent quotes an identifier for use in SQL.
//
// Every name that reaches this comes from the constants in sets.go, so
// there is nothing here for a caller to inject. Quoted anyway: the day
// somebody adds a table list read from anywhere else, the quoting is
// already where it needs to be rather than being remembered.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteAll(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, quoteIdent(n))
	}
	return strings.Join(out, ", ")
}

// SortedTableNames is every table any set names, for the invariant that
// checks nothing was left unplaced.
func SortedTableNames() []string {
	var out []string
	for _, s := range Sets {
		out = append(out, s.Tables...)
	}
	sort.Strings(out)
	return out
}

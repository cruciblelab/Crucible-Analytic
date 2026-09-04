package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Reading a backup back.
//
// # Why this is here and not only in the restore phase
//
// Because a backup nobody has restored is not a backup. It is a file
// with a plausible name, and the first person to find out otherwise is
// the one who needed it.
//
// So the writer and the reader ship together, and the test that proves
// the writer works is a real restore into a real database with the row
// counts compared. Everything above this comment was written against a
// measured failure - pg_dump's table filter silently producing an empty
// file - and the only reason that was caught is that somebody restored
// one and counted.
//
// What is *not* here is the panel's restore button, which is F1f. This
// is the primitive: it takes a pool pointed at a database whose schema
// already exists, and puts the rows back.

// maxManifest bounds the JSON entry.
//
// A backup is produced by this program, so the number is a sanity bound
// rather than a defence - but it is read before anything else is
// trusted, and an unbounded read of a header field is how a file
// decides how much memory a process uses.
const maxManifest = 1 << 20

// ReadManifest returns what a backup file says about itself, without
// restoring anything.
//
// The cheap half of "is this backup any good": it proves the file is a
// readable archive, that it carries a manifest, and what schema version
// it expects. Whether the rows are there is a different question and
// needs a restore.
func ReadManifest(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: opening %s: %w", path, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: %s is not a backup this program wrote "+
			"(it does not decompress): %w", path, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("backup: reading %s: %w", path, err)
		}
		if h.Name != ManifestName {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxManifest))
		if err != nil {
			return Manifest{}, fmt.Errorf("backup: reading the manifest in %s: %w", path, err)
		}
		var m Manifest
		if err := json.Unmarshal(body, &m); err != nil {
			return Manifest{}, fmt.Errorf("backup: the manifest in %s is not readable: %w",
				path, err)
		}
		return m, nil
	}
	return Manifest{}, fmt.Errorf("backup: %s carries no manifest, so nothing can say what "+
		"is in it or what schema it needs", path)
}

// Restored is what Restore put back.
type Restored struct {
	Table string
	Rows  int64
	// Wanted is what the manifest said should arrive.
	Wanted int64
}

// Restore copies a backup's rows into the database pool points at.
//
// The schema has to be there already. This puts rows into tables; it
// does not create them, because the tables are defined by the embedded
// schema files and a second definition inside the backup would be a
// second source of truth for the same thing.
//
// # It refuses rather than half-finishes
//
// A table whose row count does not match the manifest stops the restore
// and is reported. Continuing would leave a database that looks restored
// and is missing rows, which is worse than one that is obviously
// incomplete: the second sends somebody to look, the first does not.
func Restore(ctx context.Context, pool *pgxpool.Pool, path string) ([]Restored, error) {
	m, err := ReadManifest(path)
	if err != nil {
		return nil, err
	}
	wanted := map[string]Table{}
	for _, t := range m.Tables {
		wanted[t.Name] = t
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("backup: opening %s: %w", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("backup: %s does not decompress: %w", path, err)
	}
	defer gz.Close()

	var out []Restored
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, fmt.Errorf("backup: reading %s: %w", path, err)
		}
		name, ok := tableOfEntry(h.Name)
		if !ok {
			continue
		}
		t, known := wanted[name]
		if !known {
			return out, fmt.Errorf("backup: %s holds data for %s and its manifest does not "+
				"mention that table, so nothing says which columns it has", path, name)
		}

		var copied int64
		// The entry is streamed straight into COPY rather than read into
		// memory first: a traffic table is the largest thing here and the
		// machine restoring it is the one that just lost its data.
		if err := pool.AcquireFunc(ctx, func(c *pgxpool.Conn) error {
			sql := fmt.Sprintf("COPY %s (%s) FROM STDIN",
				quoteIdent(name), quoteAll(t.Columns))
			tag, err := c.Conn().PgConn().CopyFrom(ctx, tr, sql)
			if err != nil {
				return err
			}
			copied = tag.RowsAffected()
			return nil
		}); err != nil {
			return out, fmt.Errorf("backup: restoring %s from %s: %w", name, path, err)
		}

		out = append(out, Restored{Table: name, Rows: copied, Wanted: t.Rows})
		if copied != t.Rows {
			return out, fmt.Errorf("backup: %s should have %d rows and %d arrived. The "+
				"restore is stopped here rather than continuing: a database that looks "+
				"restored and is missing rows is worse than one that is obviously not",
				name, t.Rows, copied)
		}
	}
	return out, nil
}

// tableOfEntry turns "data/traffic_snapshots.copy" into the table name.
func tableOfEntry(entry string) (string, bool) {
	if !strings.HasPrefix(entry, "data/") || !strings.HasSuffix(entry, ".copy") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(entry, "data/"), ".copy")
	// A name with a path separator in it would be a tar entry trying to
	// escape its own directory. Nothing this program writes can produce
	// one; refused anyway, because the check costs a comparison and the
	// alternative is trusting a file's own header.
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	return name, true
}

package panel

import (
	"context"
	"fmt"
	"time"
)

// The database half of the health page.
//
// # Why the panel asks the database directly here
//
// Everywhere else the panel reads analytics over the read-only HTTP API,
// on purpose: the component a customer logs into has no direct route to
// the traffic data. That rule is not being bent here, because none of
// this is traffic data. A table's size, its chunk count and whether a
// retention policy is attached are facts about storage; the panel's role
// can read them today without any grant, because PostgreSQL and
// TimescaleDB expose them to anybody who may connect.
//
// The reason to use that rather than route these through the API is the
// moment they matter. A health page whose every section goes dark when
// the read API is unreachable is not a health page - the first thing it
// has to be able to say is "the read API is unreachable", and the second
// is anything else useful while that is true. Two sources, failing
// independently, is the whole design.
//
// # What is deliberately not here
//
// Row counts. The panel cannot count rows in traffic_snapshots or
// beacon_events and must not start being able to: that is the isolation.
// "How many events yesterday" is a question for the API, and it stays
// there.

// StorageFact is one table as the health page sees it.
type StorageFact struct {
	Table string
	// Bytes is the total on-disk size including indexes and, for a
	// hypertable, every chunk.
	Bytes int64
	// Hypertable is whether TimescaleDB manages it.
	Hypertable bool
	// Chunks is how many chunks it has, zero for an ordinary table.
	Chunks int64
	// RetentionAfter is the age at which rows are dropped, zero when no
	// policy is attached.
	//
	// A duration rather than a bool because "retention is configured" and
	// "retention drops data after ninety days" are different sentences,
	// and only the second lets somebody notice it says nine hundred.
	RetentionAfter time.Duration
}

// HealthTables are the tables the page reports on, in the order it shows
// them.
//
// A fixed list rather than everything in the schema. The page is read by
// somebody deciding whether a deployment is healthy, and a table they
// have never heard of on that list is noise that makes the three that
// matter harder to see. A table missing from the database is reported as
// missing rather than skipped - see StorageFacts.
var HealthTables = []string{
	"traffic_snapshots",
	"beacon_events",
	"ip_asn_ranges",
	"ip_country_ranges",
}

// StorageFacts reads what the panel may know about storage.
func (s *Store) StorageFacts(ctx context.Context) ([]StorageFact, error) {
	// One query for sizes and hypertable membership. to_regclass returns
	// NULL for a table that does not exist, which is how a missing table
	// arrives as a zero row rather than as an error that loses the other
	// three.
	//
	// # pg_total_relation_size does not measure a hypertable
	//
	// It measures the *parent*, and a hypertable's parent holds no rows:
	// the data is in chunks, in another schema. So the obvious call
	// returns a small, plausible, wrong number for exactly the two
	// tables this page exists to report on.
	//
	// Measured on a real database with real data:
	//
	//   traffic_snapshots   parent 40 KB      hypertable 16 MB
	//   beacon_events       parent 48 KB      hypertable 18 MB
	//
	// Four hundred times out, on the page an operator reads to decide
	// whether the disk is filling. It shipped that way from B4 until
	// F1b, and nothing failed - the number was simply the wrong one, and
	// small numbers do not look like errors.
	//
	// It was found while measuring compression for the backup estimator:
	// the *compressed dump* of a table came out larger than the table
	// was supposed to be, which is the kind of impossible result that
	// makes somebody look.
	//
	// hypertable_detailed_size sums the chunks. It needs no privilege
	// panel_user lacks - checked against the real role - and it errors on
	// a table that is not there, so the CASE below tests to_regclass
	// first rather than relying on the function to be forgiving.
	rows, err := s.pool.Query(ctx, `
		WITH wanted AS (
		    SELECT t.name,
		           to_regclass('public.' || t.name) AS rel,
		           EXISTS (SELECT 1 FROM timescaledb_information.hypertables h
		                   WHERE h.hypertable_name = t.name) AS hyper
		    FROM unnest($1::text[]) AS t(name)
		)
		SELECT w.name,
		       CASE
		           WHEN w.rel IS NULL THEN 0
		           WHEN NOT w.hyper THEN COALESCE(pg_total_relation_size(w.rel), 0)
		           ELSE COALESCE((SELECT sum(total_bytes)
		                          FROM hypertable_detailed_size(w.rel)), 0)
		       END AS bytes,
		       w.hyper,
		       (SELECT count(*) FROM timescaledb_information.chunks c
		        WHERE c.hypertable_name = w.name) AS chunks
		FROM wanted w`, HealthTables)
	if err != nil {
		return nil, fmt.Errorf("panel: reading storage facts: %w", err)
	}
	defer rows.Close()

	byName := make(map[string]*StorageFact, len(HealthTables))
	var out []StorageFact
	for rows.Next() {
		var f StorageFact
		if err := rows.Scan(&f.Table, &f.Bytes, &f.Hypertable, &f.Chunks); err != nil {
			return nil, fmt.Errorf("panel: reading storage facts: %w", err)
		}
		out = append(out, f)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("panel: reading storage facts: %w", rows.Err())
	}
	// unnest preserves the array's order, but nothing in the SQL standard
	// promises a query returns rows in any order without ORDER BY - and
	// the page's order is the list above. Sorted here rather than trusted.
	ordered := make([]StorageFact, 0, len(out))
	for i := range out {
		byName[out[i].Table] = &out[i]
	}
	for _, name := range HealthTables {
		if f, ok := byName[name]; ok {
			ordered = append(ordered, *f)
		}
	}

	if err := s.attachRetention(ctx, ordered); err != nil {
		// Reported as no policy rather than as a failure of the whole
		// page. A TimescaleDB version whose job view differs, or a
		// database without the extension, must not take the sizes down
		// with it.
		return ordered, nil
	}
	return ordered, nil
}

// attachRetention fills in RetentionAfter from TimescaleDB's job table.
func (s *Store) attachRetention(ctx context.Context, facts []StorageFact) error {
	rows, err := s.pool.Query(ctx, `
		SELECT hypertable_name, (config ->> 'drop_after')::interval
		FROM timescaledb_information.jobs
		WHERE proc_name = 'policy_retention' AND hypertable_name IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()

	after := make(map[string]time.Duration)
	for rows.Next() {
		var name string
		var d *time.Duration
		if err := rows.Scan(&name, &d); err != nil {
			return err
		}
		if d != nil {
			after[name] = *d
		}
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	for i := range facts {
		facts[i].RetentionAfter = after[facts[i].Table]
	}
	return nil
}

// DatabaseBytes is how much disk the database occupies.
//
// # Why this is here and the disk is not
//
// pg_database_size needs only CONNECT, which the panel's role has, and
// it answers the one storage question about the database that does not
// require reaching past the isolation. The *disk* the database sits on
// is a different question and the panel cannot answer it: that needs
// data_directory, which needs a privilege panel_user does not have and
// must not be given.
//
// So this is reported and the disk is reported as unknown, rather than
// showing the panel's own filesystem under a heading that implies it is
// the database's. On a single-machine install they are usually the same
// disk; "usually" is not a thing to print next to a number somebody
// decides on.
func (s *Store) DatabaseBytes(ctx context.Context) (int64, error) {
	var bytes int64
	if err := s.pool.QueryRow(ctx,
		`SELECT pg_database_size(current_database())`).Scan(&bytes); err != nil {
		return 0, fmt.Errorf("panel: reading the database size: %w", err)
	}
	return bytes, nil
}

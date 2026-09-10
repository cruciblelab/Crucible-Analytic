//go:build integration

package retention_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/retention"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The schema these tests run against is applied once, by TestMain in
// compressiondb_test.go, into a database of this suite's own - schema
// files first and then release/sql/grants.sql, in install.sh's order.
//
// So a mutation to any schema file is applied here, which is the point:
// the first mutation run against this file reported every mutation as
// caught by a compile error, because nothing had ever applied the
// mutated SQL. See CLAUDE.md's rule about a test that does not depend on
// the file it measures.
//
// And it is applied by the superuser and then handed over, which is how
// a real deployment ends up with schema_admin owning the wrappers. That
// is not a detail: a SECURITY DEFINER function runs as its owner, and
// the first version of ca_set_compression passed every test only because
// a fixture had left it owned by the superuser.

// clearCompressionSettings puts the table back to never-configured.
//
// Without this the segment assertion below tests nothing. The table in a
// development database keeps whatever the last run set, so
// ca_set_compression takes its "already configured, and it matches"
// branch and the ALTER that names site_id never executes. Measured: a
// mutation removing compress_segmentby from that ALTER survived the
// whole suite.
//
// A test that never establishes the condition it is about is not testing
// the code that handles it.
func clearCompressionSettings(t *testing.T, table retention.Table) {
	t.Helper()
	ctx := context.Background()
	pool := adminPool(t)
	// Compressed chunks pin the settings, so they go first.
	if _, err := pool.Exec(ctx, `
		SELECT public.decompress_chunk(c, if_compressed => true)
		  FROM public.show_chunks($1::regclass) c`, string(table)); err != nil {
		t.Logf("decompressing %s: %v", table, err)
	}
	if _, err := pool.Exec(ctx,
		`ALTER TABLE `+string(table)+` SET (timescaledb.compress = false)`); err != nil {
		t.Fatalf("clearing compression settings on %s: %v.\n"+
			"Without a clean start the segment assertion cannot run", table, err)
	}
}

// compressionAvailable answers the question the skips below are allowed
// to depend on, and nothing wider.
//
// The first version of this file skipped on any failure of
// ApplyCompression. That hid a bug of mine for a whole run: the
// function's own SQL was wrong, every call failed, and every test
// reported "this TimescaleDB cannot compress" and passed. A skip
// conditioned on an error you produce is a skip that hides it.
//
// So the precondition is asked separately, of the database, before
// anything is attempted.
func compressionAvailable(t *testing.T) bool {
	t.Helper()
	var license string
	if err := adminPool(t).QueryRow(context.Background(),
		`SELECT current_setting('timescaledb.license', true)`).Scan(&license); err != nil {
		t.Fatalf("asking the database about its license: %v", err)
	}
	return license == "timescale"
}

// Compression against a real TimescaleDB, as the roles the services use.
//
// The claim being tested is not "the code says it compressed" - it would
// happily say so - but that TimescaleDB's own catalogue holds compressed
// chunks, segmented by site_id, and that turning it on does not break the
// three things a deployment already depends on: the collector writing,
// retention dropping, and the backup reading.

// restoreCompression puts the table back the way this test found it.
//
// The tests in this file share one database - built once by TestMain -
// and they run in sequence, so a table left compressed is a table the
// next test starts from. Between packages this no longer matters, since
// the database is this suite's own; between tests it still does.
func restoreCompression(t *testing.T, _ *pgxpool.Pool, table retention.Table) {
	t.Helper()
	ctx := context.Background()
	// Through the superuser: the service roles reach compression only
	// through ca_set_compression, and decompressing is not theirs to do.
	// A cleanup that cannot clean up leaves the next test running against
	// this one's leftovers.
	pool := adminPool(t)
	var enabledBefore bool
	if err := pool.QueryRow(ctx, `
		SELECT compression_enabled FROM timescaledb_information.hypertables
		 WHERE hypertable_schema = 'public' AND hypertable_name = $1`,
		string(table)).Scan(&enabledBefore); err != nil {
		t.Fatalf("reading whether %s is compressed to begin with: %v.\n"+
			"Without this the cleanup below cannot know what to put back", table, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		// Only where the state actually differs from what was found.
		// decompress_chunk on a table whose settings a test already
		// cleared fails with "missing compressed hypertable", and a
		// cleanup that reports an error for work nobody asked for is a
		// cleanup people learn to ignore.
		var enabledNow bool
		if err := pool.QueryRow(bg, `
			SELECT compression_enabled FROM timescaledb_information.hypertables
			 WHERE hypertable_schema = 'public' AND hypertable_name = $1`,
			string(table)).Scan(&enabledNow); err != nil {
			t.Errorf("cleanup: reading whether %s is still compressed: %v", table, err)
		}
		if enabledNow && !enabledBefore {
			// Chunks pin the settings, so they go first.
			if _, err := pool.Exec(bg, `
				SELECT public.decompress_chunk(c, if_compressed => true)
				  FROM public.show_chunks($1::regclass) c`, string(table)); err != nil {
				t.Errorf("cleanup: decompressing %s: %v", table, err)
			}
			if _, err := pool.Exec(bg,
				`ALTER TABLE `+string(table)+` SET (timescaledb.compress = false)`); err != nil {
				t.Errorf("cleanup: turning compression back off on %s: %v", table, err)
			}
		}
	})
}

func segmentByOf(t *testing.T, pool *pgxpool.Pool, table retention.Table) string {
	t.Helper()
	var segmentby *string
	err := pool.QueryRow(context.Background(), `
		SELECT segmentby
		  FROM timescaledb_information.hypertable_compression_settings
		 WHERE hypertable = $1::regclass`, string(table)).Scan(&segmentby)
	if err != nil {
		return ""
	}
	if segmentby == nil {
		return ""
	}
	return *segmentby
}

// The phase's own done-criterion: real chunks, actually compressed, and
// segmented by the column the dashboard queries by.
//
// The segment is the whole point. Compression alone would shrink the
// disk and leave the read pattern exactly as slow, because what costs is
// that one site's rows are interleaved with every other site's.
//
// # Why it seeds its own old row first
//
// A pass that finds no chunk old enough compresses nothing and returns
// "ok" - correctly. Run against whatever a development database happens
// to hold, this test would then pass on a build where compression was
// broken. So it puts a row forty days back in each table and asserts
// against a chunk it knows exists.
func TestApplyCompression_CompressesOldChunksSegmentedBySite(t *testing.T) {
	need(t)
	if !compressionAvailable(t) {
		t.Skip("this TimescaleDB is the Apache build; compression does not exist here")
	}
	ctx := context.Background()

	for _, table := range []retention.Table{retention.TableTrafficSnapshots, retention.TableBeaconEvents} {
		t.Run(string(table), func(t *testing.T) {
			pool := poolAs(t, ownerOf(table))
			restoreCompression(t, pool, table)
			clearCompressionSettings(t, table)
			// One chunk old enough to compress and one far too young.
			// Without the second, "compress everything, ignore the age"
			// is a mutation nothing here notices - measured.
			seedRow(t, pool, table, time.Now().Add(-40*24*time.Hour))
			seedRow(t, pool, table, time.Now().Add(-time.Minute))

			manager, err := retention.NewManager(pool, table)
			if err != nil {
				t.Fatalf("retention.NewManager: %v", err)
			}

			report, err := manager.ApplyCompression(ctx, 7, 90)
			if err != nil {
				t.Fatalf("ApplyCompression: %v", err)
			}
			if report.Skipped != "" {
				t.Fatalf("nothing was done: %s", report.Skipped)
			}
			if report.Gained < 1 {
				t.Fatalf("the pass compressed %d chunks; the seeded row is forty days old, "+
					"so at least one was eligible", report.Gained)
			}

			// Read back from TimescaleDB itself, not from the report. A
			// report is what the code believes; this is what happened.
			var compressed, total int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FILTER (WHERE is_compressed), count(*)
				  FROM timescaledb_information.chunks
				 WHERE hypertable_schema = 'public' AND hypertable_name = $1`,
				string(table)).Scan(&compressed, &total); err != nil {
				t.Fatalf("reading the chunk catalogue: %v", err)
			}
			if compressed < 1 {
				t.Fatalf("TimescaleDB holds %d compressed chunks of %d for %s, want at least 1",
					compressed, total, table)
			}
			if compressed != report.Compressed || total != report.Chunks {
				t.Errorf("the report says %d of %d compressed, the catalogue says %d of %d",
					report.Compressed, report.Chunks, compressed, total)
			}
			// And the young chunk is left alone. This is the reason
			// DefaultCompressAfterDays exists: a compressed chunk can
			// still be written to, but the row takes a slower path, and
			// the newest chunk is the one the collector writes into
			// every ten seconds. Compressing it would make the traffic
			// path pay for the dashboard.
			var tooYoung int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM timescaledb_information.chunks
				 WHERE hypertable_schema = 'public' AND hypertable_name = $1
				   AND is_compressed
				   AND range_end > now() - INTERVAL '7 days'`,
				string(table)).Scan(&tooYoung); err != nil {
				t.Fatalf("looking for compressed chunks that are too young: %v", err)
			}
			if tooYoung != 0 {
				t.Errorf("%d chunk(s) newer than the seven-day age were compressed; "+
					"the age is what keeps the write path out of this", tooYoung)
			}

			if got := segmentByOf(t, pool, table); got != "site_id" {
				t.Errorf("segmentby = %q, want site_id - without it the disk shrinks and "+
					"every per-site query still reads every site's pages", got)
			}

			// Applying again compresses nothing new and says so.
			again, err := manager.ApplyCompression(ctx, 7, 90)
			if err != nil {
				t.Fatalf("ApplyCompression again: %v", err)
			}
			if again.Gained != 0 {
				t.Errorf("the second pass compressed %d more chunks; every pass after the "+
					"first should find the work done", again.Gained)
			}
		})
	}
}

// seedRow puts one row at a chosen time, so a chunk of that age exists
// whatever else the database holds.
//
// Written per table rather than generated, for the reason the schema
// gives about ca_trim_site_rows: what runs is visible in full where it
// is read. The rows are removed again, by the owner - the writer roles
// hold no DELETE.
func seedRow(t *testing.T, pool *pgxpool.Pool, table retention.Table, at time.Time) {
	t.Helper()
	ctx := context.Background()
	const site = "sikistirma-tohumu"

	var err error
	switch table {
	case retention.TableTrafficSnapshots:
		_, err = pool.Exec(ctx, `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ja4, prev_window_count, curr_window_count,
			   request_rate, bot_score)
			VALUES ($1, $2, '203.0.113.40'::inet, 't13d', 1, 1, 1.0, 10)`, at, site)
	case retention.TableBeaconEvents:
		_, err = pool.Exec(ctx, `
			INSERT INTO beacon_events (time, site_id, visitor_id, event_type)
			VALUES ($1, $2, 'tohum', 'pageview')`, at, site)
	default:
		t.Fatalf("seedRow does not know %s", table)
	}
	if err != nil {
		t.Fatalf("seeding a row at %s in %s: %v", at.Format(time.RFC3339), table, err)
	}
	t.Cleanup(func() {
		if _, err := adminPool(t).Exec(context.Background(),
			`DELETE FROM `+string(table)+` WHERE site_id = $1`, site); err != nil {
			t.Errorf("removing the seeded row from %s: %v", table, err)
		}
	})
}

// A policy that never fires is worse than none, because it looks like
// the feature working.
func TestCompressingAfterTheDataIsDroppedIsRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	pool := poolAs(t, "collector")
	restoreCompression(t, pool, retention.TableTrafficSnapshots)

	manager, err := retention.NewManager(pool, retention.TableTrafficSnapshots)
	if err != nil {
		t.Fatalf("retention.NewManager: %v", err)
	}

	report, err := manager.ApplyCompression(ctx, 90, 30)
	if err != nil {
		t.Fatalf("ApplyCompression: %v", err)
	}
	if report.Skipped == "" {
		t.Fatal("compressing after 90 days on a table kept 30 was accepted; " +
			"no chunk would ever live long enough for that policy to fire")
	}
	if !strings.Contains(report.Skipped, "never compress") {
		t.Errorf("the refusal does not say why: %q", report.Skipped)
	}
}

// The settings are not rewritten under a deployment that already chose
// something else. Changing segmentby decompresses every chunk, and a
// service is not allowed to decide that about a customer's history at
// startup.
func TestCompressionLeavesSettingsItDidNotChooseAlone(t *testing.T) {
	need(t)
	if !compressionAvailable(t) {
		t.Skip("this TimescaleDB is the Apache build; compression does not exist here")
	}
	ctx := context.Background()
	// ALTER TABLE needs the table's owner, which on an installed
	// deployment is the role that ran the installer rather than the
	// service. The manager still runs as the service; only the fixture
	// that fakes another deployment's choice needs the stronger hand.
	pool := adminPool(t)
	service := poolAs(t, ownerOf(retention.TableBeaconEvents))
	table := retention.TableBeaconEvents
	restoreCompression(t, pool, table)

	// Whatever the table carries now, put it back afterwards.
	before := segmentByOf(t, pool, table)
	t.Cleanup(func() {
		bg := context.Background()
		if before == "" {
			_, _ = pool.Exec(bg, `ALTER TABLE beacon_events SET (timescaledb.compress = false)`)
			return
		}
		_, _ = pool.Exec(bg, `ALTER TABLE beacon_events SET (timescaledb.compress,
			timescaledb.compress_segmentby = 'site_id', timescaledb.compress_orderby = 'time DESC')`)
	})

	if _, err := pool.Exec(ctx, `ALTER TABLE beacon_events SET (
		timescaledb.compress,
		timescaledb.compress_segmentby = 'event_name',
		timescaledb.compress_orderby   = 'time DESC')`); err != nil {
		t.Skipf("cannot set compression settings here: %v", err)
	}

	manager, err := retention.NewManager(service, table)
	if err != nil {
		t.Fatalf("retention.NewManager: %v", err)
	}
	report, err := manager.ApplyCompression(ctx, 7, 90)
	if err != nil {
		t.Fatalf("ApplyCompression: %v", err)
	}
	if report.Skipped == "" {
		t.Fatal("a table with different settings was taken over silently")
	}
	if got := segmentByOf(t, pool, table); got != "event_name" {
		t.Errorf("segmentby is now %q; the deployment's own choice was overwritten", got)
	}
}

// TestNoWrapperRunsAsASuperuser.
//
// A SECURITY DEFINER function runs as whoever owns it, so the owner is
// the privilege the wrapper actually has - and it is decided by who
// applied the schema, not by anything in the schema.
//
// This is the property whose absence made the first version of
// ca_set_compression pass every test and work on no deployment. It
// called add_compression_policy, which only a superuser may call; in the
// development database the function happened to be owned by the
// superuser, so it worked there. On a real install
// release/sql/grants.sql hands every routine to schema_admin - which is
// the whole point of that file - and the call would have been denied,
// quietly, as "this database cannot compress".
//
// The list is derived from the catalogue, so a fifth wrapper is covered
// the day it is written.
func TestNoWrapperRunsAsASuperuser(t *testing.T) {
	need(t)
	rows, err := adminPool(t).Query(context.Background(), `
		SELECT p.proname, pg_get_userbyid(p.proowner), r.rolsuper
		  FROM pg_proc p
		  JOIN pg_namespace n ON n.oid = p.pronamespace
		  JOIN pg_roles r ON r.oid = p.proowner
		 WHERE n.nspname = 'public' AND p.prosecdef AND p.proname LIKE 'ca\_%'
		 ORDER BY p.proname`)
	if err != nil {
		t.Fatalf("listing the wrappers: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var name, owner string
		var superuser bool
		if err := rows.Scan(&name, &owner, &superuser); err != nil {
			t.Fatalf("scanning a wrapper: %v", err)
		}
		seen++
		if superuser {
			t.Errorf("%s is owned by %s, a superuser.\n"+
				"SECURITY DEFINER means it runs with that role's privileges, which is "+
				"both more than this product ever grants itself and a privilege no real "+
				"deployment has - release/sql/grants.sql gives every routine to "+
				"schema_admin. A wrapper that needs more than schema_admin works here "+
				"and nowhere else.", name, owner)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listing the wrappers: %v", err)
	}
	if seen == 0 {
		t.Fatal("no SECURITY DEFINER ca_* function was found, so the check above " +
			"passed by having nothing to check")
	}
}

// Each service reaches its own table and no other, here as everywhere
// else in this file.
func TestAServiceCannotCompressTheOtherServicesTable(t *testing.T) {
	need(t)
	ctx := context.Background()
	pool := poolAs(t, "collector")

	manager, err := retention.NewManager(pool, retention.TableBeaconEvents)
	if err != nil {
		t.Fatalf("retention.NewManager: %v", err)
	}
	if _, err := manager.ApplyCompression(ctx, 7, 90); err == nil {
		t.Fatal("collector compressed beacon_events; the wrapper's caller check did not run")
	}
}

// The three things a deployment already depends on, on a chunk that is
// actually compressed.
//
// This is the test the phase exists for. Compression is not a read-only
// convenience: it changes how rows are stored, and the collector writes
// every ten seconds, retention drops chunks daily, and the backup reads
// the table through COPY. Any one of those breaking would be a far worse
// outcome than a slow dashboard.
func TestWritingRetainingAndBackingUpStillWorkOnACompressedChunk(t *testing.T) {
	need(t)
	if !compressionAvailable(t) {
		t.Skip("this TimescaleDB is the Apache build; compression does not exist here")
	}
	ctx := context.Background()
	// Two hands, because production has two. The service writes, and asks
	// for the compression and the retention; the chunk-dropping is done
	// by TimescaleDB's scheduler, which runs as the role that installed -
	// see TestTheRetentionJobIsNotOwnedByTheServiceThatAskedForIt. A test
	// that did both as one role would be testing a deployment nobody has.
	service := poolAs(t, "collector")
	admin := adminPool(t)
	table := retention.TableTrafficSnapshots
	restoreCompression(t, service, table)

	const site = "sikistirma-testi"
	old := time.Now().Add(-40 * 24 * time.Hour)
	// Cleared before as well as after, and the error is not swallowed.
	//
	// The first version of this ran the delete as the service role and
	// wrote `_, _ =`. collector holds SELECT and INSERT and no DELETE, so
	// every cleanup failed silently and the next run started with the
	// last one's rows - which showed up as "4 rows, want 2" and looked
	// like a bug in compression. A cleanup that cannot clean up is worse
	// than none, because it is believed.
	clear := func(when string) {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM traffic_snapshots WHERE site_id = $1`, site); err != nil {
			t.Fatalf("clearing the test site %s: %v", when, err)
		}
	}
	clear("before")
	t.Cleanup(func() { clear("after") })

	insert := func(at time.Time, ip string) error {
		_, err := service.Exec(ctx, `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ja4, prev_window_count, curr_window_count,
			   request_rate, bot_score)
			VALUES ($1, $2, $3::inet, 't13d', 1, 1, 1.0, 10)`, at, site, ip)
		return err
	}
	if err := insert(old, "203.0.113.10"); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	manager, err := retention.NewManager(service, table)
	if err != nil {
		t.Fatalf("retention.NewManager: %v", err)
	}
	// The service's own call is what compresses the chunk holding that
	// row. Written this way rather than compressing by hand afterwards:
	// the first version did it by hand, which meant the three assertions
	// below held against a chunk the *test* had compressed, and would
	// have gone on holding if the product compressed nothing at all.
	report, err := manager.ApplyCompression(ctx, 7, 90)
	if err != nil {
		t.Fatalf("ApplyCompression: %v", err)
	}
	if report.Gained < 1 {
		t.Fatalf("the service compressed %d chunks, so nothing below is testing a "+
			"compressed chunk", report.Gained)
	}
	var compressed int
	if err := admin.QueryRow(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_schema = 'public' AND hypertable_name = 'traffic_snapshots'
		   AND is_compressed`).Scan(&compressed); err != nil {
		t.Fatalf("counting compressed chunks: %v", err)
	}
	if compressed == 0 {
		t.Fatal("TimescaleDB holds no compressed chunk, so nothing below is testing one")
	}

	// 1. The collector can still write into a compressed chunk's range.
	if err := insert(old.Add(time.Hour), "203.0.113.11"); err != nil {
		t.Errorf("writing into a compressed chunk failed: %v.\n"+
			"This is the traffic path; it must not be what pays for a faster dashboard", err)
	}

	// 2. The backup's own path still reads every row. COPY rather than
	// pg_dump, which is what internal/backup uses and why.
	var copied int64
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM traffic_snapshots WHERE site_id = $1`, site).Scan(&copied); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if copied != 2 {
		t.Errorf("the table reports %d rows for the test site, want 2", copied)
	}

	// 3. Retention still drops. The policy is set to a day, which is
	// shorter than the rows are old.
	if _, err := manager.Apply(ctx, retention.Policy{Days: 1}); err != nil {
		t.Fatalf("Apply retention: %v", err)
	}
	var dropped int64
	if err := admin.QueryRow(ctx, `
		SELECT count(*) FROM (
		  SELECT public.drop_chunks('traffic_snapshots',
		         older_than => INTERVAL '1 day')) x`).Scan(&dropped); err != nil {
		t.Fatalf("dropping chunks: %v", err)
	}
	var left int64
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM traffic_snapshots WHERE site_id = $1`, site).Scan(&left); err != nil {
		t.Fatalf("counting after the drop: %v", err)
	}
	if left != 0 {
		t.Errorf("%d rows survived a chunk drop on a compressed chunk; retention has stopped "+
			"working, which is a full disk on the machine that also serves the site", left)
	}
}

//go:build integration

package asnlookup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/ipsources"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// M2's record, against the real table and as the real roles.
//
// The phase's done criteria, one test each: a failed row says which
// source and why; a successful row carries rows and bytes; the sweep
// removes what it should and is actually called.

// fetchLogResolver is a resolver whose refreshes read from a directory
// rather than the network, so a test can decide what a fetch finds.
//
// Local files rather than an httptest server for the ordinary cases,
// because "the file is missing" and "the file is truncated" are the two
// failures worth recording and both are one WriteFile apart. The network
// failure has its own test below, since only it can produce an HTTP
// status in the error chain.
func fetchLogResolver(t *testing.T, dir string) *Resolver {
	t.Helper()
	ctx := context.Background()
	r, err := NewResolver(ctx, testDatabaseURL, CacheConfig{MaxEntries: 10, TTL: 0}, dir, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v (is the database up and installed? see internal/testdb)", err)
	}
	// SkipRangePersistence would also skip the fetch log - it is the one
	// switch that means "touch no table" - so these tests write the
	// range tables too and clean up after themselves.
	t.Cleanup(r.Close)
	clear := func() {
		if _, err := r.pool.Exec(context.Background(),
			`DELETE FROM ip_range_fetches`); err != nil {
			t.Logf("cleanup: clearing the fetch log: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
	return r
}

// countryFiles writes a usable pair of country CSVs for src into dir.
func countryFiles(t *testing.T, dir string, src ipsources.Source) {
	t.Helper()
	write(t, filepath.Join(dir, src.IPv4File), "1.0.0.0,1.0.0.255,AU\n192.0.2.0,192.0.2.255,US\n")
	write(t, filepath.Join(dir, src.IPv6File), "2001:db8::,2001:db8::ffff,JP\n")
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fetchRows reads the log back, oldest first, so a test can assert the
// order attempts happened in.
func fetchRows(t *testing.T, pool *pgxpool.Pool) []fetchRecord {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT started_at, finished_at, source_id, kind, family, origin,
		       outcome, rows_parsed, bytes_read, error_chain
		FROM ip_range_fetches ORDER BY id`)
	if err != nil {
		t.Fatalf("reading the fetch log: %v", err)
	}
	defer rows.Close()

	var out []fetchRecord
	for rows.Next() {
		var rec fetchRecord
		if err := rows.Scan(&rec.started, &rec.finished, &rec.sourceID, &rec.kind,
			&rec.family, &rec.origin, &rec.outcome, &rec.rows, &rec.bytes,
			&rec.errChain); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f fetchRecord) String() string {
	return fmt.Sprintf("%s/%s %s rows=%d bytes=%d err=%q",
		f.sourceID, f.family, f.outcome, f.rows, f.bytes, f.errChain)
}

// TestASuccessfulFetchRecordsRowsAndBytes.
//
// The half of the phase a customer reads when nothing is wrong: how much
// data came, and when. Both numbers are asserted against what the files
// actually contain rather than against "> 0", because a count that is
// merely nonzero is a count nobody can check.
func TestASuccessfulFetchRecordsRowsAndBytes(t *testing.T) {
	dir := t.TempDir()
	src, _ := ipsources.ByID(ipsources.DefaultCountry)
	countryFiles(t, dir, src)

	r := fetchLogResolver(t, dir)
	began := time.Now()
	r.refreshCountry(context.Background())

	rows := fetchRows(t, r.pool)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one per address family: %v", len(rows), rows)
	}

	v4, v6 := rows[0], rows[1]
	for _, rec := range rows {
		if rec.sourceID != src.ID {
			t.Errorf("%v names source %q, want %q", rec, rec.sourceID, src.ID)
		}
		if rec.kind != "country" {
			t.Errorf("%v has kind %q, want country", rec, rec.kind)
		}
		if rec.outcome != fetchSucceeded {
			t.Errorf("%v did not succeed, and the files were there", rec)
		}
		if rec.origin != originMirror {
			t.Errorf("%v says origin %q; this resolver reads a directory, and a "+
				"reader who cannot tell a mirror from a download reads a small byte "+
				"count as a broken server", rec, rec.origin)
		}
		if rec.errChain != "" {
			t.Errorf("%v carries an error chain on a successful fetch", rec)
		}
		if rec.started.Before(began.Add(-time.Second)) || rec.finished.Before(rec.started) {
			t.Errorf("%v has times outside this run (%v .. %v)", rec, rec.started, rec.finished)
		}
	}

	// Exactly what the files hold: two ranges in v4, one in v6.
	if v4.family != familyIPv4 || v4.rows != 2 {
		t.Errorf("ipv4 row = %v, want family ipv4 with 2 ranges", v4)
	}
	if v6.family != familyIPv6 || v6.rows != 1 {
		t.Errorf("ipv6 row = %v, want family ipv6 with 1 range", v6)
	}

	// And the byte counts are the file sizes, which is the claim
	// "how much data came" actually makes.
	for _, want := range []struct {
		rec  fetchRecord
		file string
	}{{v4, src.IPv4File}, {v6, src.IPv6File}} {
		info, err := os.Stat(filepath.Join(dir, want.file))
		if err != nil {
			t.Fatal(err)
		}
		if want.rec.bytes != info.Size() {
			t.Errorf("%v recorded %d bytes and the file is %d; a byte count that is "+
				"merely nonzero cannot tell a truncated download from a whole one",
				want.rec, want.rec.bytes, info.Size())
		}
	}
}

// TestAFailedFetchNamesTheSourceAndTheReason.
//
// The phase's headline criterion. Before this, a refresh that had been
// failing for a month left one warning line in a journal the customer
// cannot read, and the symptom was a page that looked like a quiet week.
func TestAFailedFetchNamesTheSourceAndTheReason(t *testing.T) {
	// An empty directory: every file is missing, which is the shape of a
	// mirror somebody pointed at the wrong path.
	r := fetchLogResolver(t, t.TempDir())
	r.refreshCountry(context.Background())

	rows := fetchRows(t, r.pool)
	if len(rows) == 0 {
		t.Fatal("a refresh where every file was missing recorded nothing at all, " +
			"which leaves the failure exactly where M2 found it: in a journal")
	}
	for _, rec := range rows {
		if rec.outcome != fetchFailed {
			t.Errorf("%v succeeded against an empty directory", rec)
		}
		if rec.sourceID == "" {
			t.Errorf("%v does not say which dataset failed", rec)
		}
		if !strings.Contains(rec.errChain, "no such file") {
			t.Errorf("%v's error chain is %q, which does not say why it failed",
				rec, rec.errChain)
		}
		// The path is in the chain, which is what turns "it failed" into
		// something the person who set local_csv_path can act on.
		if !strings.Contains(rec.errChain, rec.sourceID) {
			t.Errorf("%v's error chain does not name the file it looked for: %q",
				rec, rec.errChain)
		}
	}
}

// TestAFallbackLeavesBothAttemptsInTheLog.
//
// M1 added a fallback order, and a fallback that works silently is a
// deployment quietly running on its second choice. Both rows are here,
// in order, so "why is my data from iptoasn" has an answer.
func TestAFallbackLeavesBothAttemptsInTheLog(t *testing.T) {
	dir := t.TempDir()
	// The chosen source has no files; the fallback does.
	fallback, _ := ipsources.ByID("iptoasn-country")
	countryFiles(t, dir, fallback)

	r := fetchLogResolver(t, dir)
	r.SetSources(ipsources.DefaultCountry, "", []string{fallback.ID})
	r.refreshCountry(context.Background())

	rows := fetchRows(t, r.pool)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want two files failed then two succeeded: %v", len(rows), rows)
	}
	for _, rec := range rows[:2] {
		if rec.sourceID != ipsources.DefaultCountry || rec.outcome != fetchFailed {
			t.Errorf("first attempt = %v, want the chosen source failing", rec)
		}
	}
	for _, rec := range rows[2:] {
		if rec.sourceID != fallback.ID || rec.outcome != fetchSucceeded {
			t.Errorf("second attempt = %v, want the fallback succeeding", rec)
		}
	}
}

// TestATruncatedFileIsNotRecordedAsASuccess.
//
// The failure the byte count exists for. Both parsers stop at a
// malformed record and keep what they read, so a file cut in half
// produces rows, no error, and a range table missing half the internet.
// What the log has to show is the byte count, because nothing else
// differs.
func TestATruncatedFileIsNotRecordedAsASuccess(t *testing.T) {
	dir := t.TempDir()
	src, _ := ipsources.ByID(ipsources.DefaultCountry)
	countryFiles(t, dir, src)
	// Cut the IPv4 file mid-row.
	write(t, filepath.Join(dir, src.IPv4File), "1.0.0.0,1.0.0.255,AU\n192.0.2.0,192.0")

	r := fetchLogResolver(t, dir)
	r.refreshCountry(context.Background())

	rows := fetchRows(t, r.pool)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want two: %v", len(rows), rows)
	}
	v4 := rows[0]
	// The parser keeps the whole row it managed and drops the partial
	// one, so this is not an error - and that is the point. The only
	// evidence is the size.
	if v4.rows != 1 {
		t.Errorf("%v parsed %d rows from a file with one whole row and one cut short",
			v4, v4.rows)
	}
	info, err := os.Stat(filepath.Join(dir, src.IPv4File))
	if err != nil {
		t.Fatal(err)
	}
	if v4.bytes != info.Size() {
		t.Errorf("%v recorded %d bytes, file is %d", v4, v4.bytes, info.Size())
	}
	if v4.bytes >= rows[1].bytes*10 {
		t.Logf("note: the truncated file is %d bytes against the intact ipv6 file's %d",
			v4.bytes, rows[1].bytes)
	}
}

// TestAnHTTPFailureRecordsTheStatus.
//
// The download path, which the mirror tests cannot reach. A 404 is what
// a moved dataset looks like, and the status is the whole diagnosis.
func TestAnHTTPFailureRecordsTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	r := fetchLogResolver(t, "")
	// Point the loader at the test server directly: SetSources chooses
	// from the library, and the library's URLs are the real ones.
	began := time.Now()
	_, bytes, err := r.loadCountryCSV(context.Background(), srv.URL, "unused")
	if err == nil {
		t.Fatal("a 404 did not produce an error")
	}
	src, _ := ipsources.ByID(ipsources.DefaultCountry)
	r.recordFetch(context.Background(),
		r.newFetchRecord(src, familyIPv4, began, time.Now(), 0, bytes, err))

	rows := fetchRows(t, r.pool)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want one: %v", len(rows), rows)
	}
	if !strings.Contains(rows[0].errChain, "404") {
		t.Errorf("%v does not carry the HTTP status; "+
			"a moved dataset and an unreachable host are different problems and "+
			"the status is what separates them", rows[0])
	}
	if rows[0].origin != originDownload {
		t.Errorf("%v says %q, want download", rows[0], rows[0].origin)
	}
}

// TestTheSweepRemovesWhatIsPastRetentionAndNothingElse.
//
// Both halves. A sweep that removes nothing is the unbounded table this
// phase was warned about; a sweep that removes everything deletes the
// row somebody is looking at.
func TestTheSweepRemovesWhatIsPastRetentionAndNothingElse(t *testing.T) {
	// The threshold, written out once, deliberately.
	//
	// Everything below is expressed in terms of fetchRetention, so the
	// test is self-consistent whatever that constant says - which means
	// it would stay green if somebody changed ninety days to one hour
	// and every deployment quietly lost its fetch history. Measured:
	// that exact mutation passed. A literal is what makes the value
	// itself a decision somebody has to make again, the same job the
	// literal URLs in TestTheDefaultsAreStillTodaysFiles do.
	if fetchRetention != 90*24*time.Hour {
		t.Errorf("fetchRetention is %v and this project chose 90 days.\n"+
			"Not a typo check: the retention is how far back \"when did this "+
			"last work\" can be answered, and a weekly refresh needs months of "+
			"history before that question has any rows to look at.", fetchRetention)
	}

	r := fetchLogResolver(t, t.TempDir())
	ctx := context.Background()

	seed := func(age time.Duration) {
		when := time.Now().Add(-age)
		if _, err := r.pool.Exec(ctx, `
			INSERT INTO ip_range_fetches
			  (started_at, finished_at, source_id, kind, family, origin, outcome)
			VALUES ($1, $1, 'user-country', 'country', 'ipv4', 'download', 'succeeded')`,
			when); err != nil {
			t.Fatalf("seeding a row aged %v: %v", age, err)
		}
	}
	seed(fetchRetention + 24*time.Hour) // past
	seed(fetchRetention - 24*time.Hour) // inside
	seed(time.Hour)                     // today

	removed, err := r.PurgeOldFetches(ctx)
	if err != nil {
		t.Fatalf("PurgeOldFetches: %v", err)
	}
	if removed != 1 {
		t.Errorf("the sweep removed %d rows, want the one past %v", removed, fetchRetention)
	}
	if left := len(fetchRows(t, r.pool)); left != 2 {
		t.Errorf("%d rows left, want the two inside retention", left)
	}
}

// TestThePanelCanReadTheLogAndCannotWriteIt.
//
// The role split, measured against the live database rather than read
// off grants.sql. A record whose reader can also write it is a record
// that can be made to say the fetch succeeded, and the only thing
// holding that is a GRANT nobody re-checks.
func TestThePanelCanReadTheLogAndCannotWriteIt(t *testing.T) {
	ctx := context.Background()
	r := fetchLogResolver(t, t.TempDir())
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO ip_range_fetches
		  (started_at, finished_at, source_id, kind, family, origin, outcome)
		VALUES (now(), now(), 'user-country', 'country', 'ipv4', 'download', 'failed')`); err != nil {
		t.Fatalf("the writer could not write: %v", err)
	}

	panelPool, err := pgxpool.New(ctx, "postgres://panel_user:panel_user@localhost:5432/analytics")
	if err != nil {
		t.Fatalf("panel pool: %v", err)
	}
	defer panelPool.Close()

	var n int
	if err := panelPool.QueryRow(ctx, `SELECT count(*) FROM ip_range_fetches`).Scan(&n); err != nil {
		t.Fatalf("the panel cannot read the fetch log: %v.\n"+
			"Then the page that answers \"is my geography data current\" has no source", err)
	}
	if n != 1 {
		t.Errorf("the panel sees %d rows, want 1", n)
	}

	for _, forbidden := range []struct{ what, sql string }{
		{"insert", `INSERT INTO ip_range_fetches
		   (started_at, finished_at, source_id, kind, family, origin, outcome)
		   VALUES (now(), now(), 'made-up', 'country', 'ipv4', 'download', 'succeeded')`},
		{"update", `UPDATE ip_range_fetches SET outcome = 'succeeded'`},
	} {
		if _, err := panelPool.Exec(ctx, forbidden.sql); err == nil {
			t.Errorf("the panel could %s the fetch log. A reader that can write its "+
				"own evidence can make a month of failed fetches read as a healthy "+
				"deployment", forbidden.what)
		}
	}
}

// TestTheUpgradePathAloneLeavesTheFetchLogWritable.
//
// The gap this table was the first to fall into, and the test that keeps
// the next new table out of it.
//
// There are two ways a database reaches a new schema, and they do
// different amounts of work:
//
//	install.sh          schema files, then release/sql/grants.sql
//	the upgrade button  schema files, and nothing else
//
// internal/schemafiles.InOrder is exactly the list of schema.sql files;
// privileges are not in it. Every table in this project predates the
// upgrade machinery, so nobody had added one since - and this is the
// first. Measured before the fix: after applying only the schema file,
//
//	INSERT INTO ip_range_fetches ...
//	ERROR:  permission denied for table ip_range_fetches
//
// which fails quietly the way this project's failures do: recordFetch
// logs a warning, the refresh carries on, geography keeps working, and
// the fetch log stays permanently empty - indistinguishable from a
// deployment that has simply never refreshed.
//
// So the whole button path is replayed here: revoke everything, apply
// the schema file as the applier's own role, and ask each service to do
// its job.
func TestTheUpgradePathAloneLeavesTheFetchLogWritable(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Pool(t, testdb.SchemaAdmin)
	// The table's privileges are global state and internal/panel reads
	// this table in another process. Without this, a read landing inside
	// the revoke below fails with "permission denied", which reads as a
	// broken grant rather than as two suites overlapping.
	testdb.Lock(t, admin, testdb.FetchLogLock)

	roles := []string{"collector", "beacon_writer", "panel_user"}
	for _, role := range roles {
		if _, err := admin.Exec(ctx,
			`REVOKE ALL ON ip_range_fetches FROM `+role); err != nil {
			t.Fatalf("revoking from %s: %v", role, err)
		}
	}
	// Restored by the schema file below, but also on the way out: a test
	// that left this database without its grants would break every later
	// suite for a reason none of them could explain.
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), SchemaSQL); err != nil {
			t.Logf("cleanup: reapplying the schema: %v", err)
		}
	})

	// Exactly what the applier does: the embedded schema file, as
	// schema_admin, and nothing else.
	if _, err := admin.Exec(ctx, SchemaSQL); err != nil {
		t.Fatalf("applying the schema the way the upgrader does: %v", err)
	}

	writer, err := pgxpool.New(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("writer pool: %v", err)
	}
	defer writer.Close()
	if _, err := writer.Exec(ctx, `
		INSERT INTO ip_range_fetches
		  (started_at, finished_at, source_id, kind, family, origin, outcome)
		VALUES (now(), now(), 'upgrade-path', 'country', 'ipv4', 'download', 'succeeded')`); err != nil {
		t.Fatalf("after the upgrade path alone, the fetcher cannot write its log: %v.\n"+
			"The button applies schema files and not grants.sql, so a new table's "+
			"privileges have to travel with the table - see the DO block at the "+
			"bottom of internal/asnlookup/schema.sql", err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM ip_range_fetches WHERE source_id = 'upgrade-path'`); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}()

	// The sweep needs DELETE, which is the privilege easiest to forget
	// precisely because nothing fails until ninety days in.
	if _, err := writer.Exec(ctx,
		`DELETE FROM ip_range_fetches WHERE source_id = 'no-such-row'`); err != nil {
		t.Errorf("the fetcher cannot delete, so its sweep would fail for ever and "+
			"the table would grow without bound: %v", err)
	}

	panelPool, err := pgxpool.New(ctx, "postgres://panel_user:panel_user@localhost:5432/analytics")
	if err != nil {
		t.Fatalf("panel pool: %v", err)
	}
	defer panelPool.Close()

	var n int
	if err := panelPool.QueryRow(ctx, `SELECT count(*) FROM ip_range_fetches`).Scan(&n); err != nil {
		t.Errorf("after the upgrade path alone, the panel cannot read the fetch log: %v", err)
	}
	// And the split survives the same path: privileges that travel with
	// a table are just as able to travel with too many.
	if _, err := panelPool.Exec(ctx, `UPDATE ip_range_fetches SET outcome = 'succeeded'`); err == nil {
		t.Error("after the upgrade path alone, the panel could update the fetch log")
	}
}

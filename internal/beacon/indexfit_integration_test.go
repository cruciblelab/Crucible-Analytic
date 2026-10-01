//go:build integration

package beacon

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The largest row BuildRow can make, written beside two ordinary ones,
// into the indexes this database actually has.
//
// # The defect this holds
//
// PostgreSQL refuses an index entry larger than 2704 bytes, and refuses
// the whole statement with it. The writer's statement is the batch's
// COPY, so one oversized entry loses every other visitor's events in the
// batch - the failure sanitizeText exists to prevent, through a limit it
// was not counting. Three campaign values of 256 four-byte runes each
// made a 3104-byte entry in idx_beacon_events_campaign: measured with the
// real beacon (NOTES, "Beacon yığını"), fifty-one events answered 204 and
// none was written. Every value was inside its rune cap.
//
// The test meant to cover this,
// TestWriter_RealTimescaleDB_StoresLongButValidText, wrote one campaign
// value of a repeated two-byte character, and a repeated character is
// the input that hides the limit: PostgreSQL compresses a long index
// value before measuring it.
// So the values here are varied four-byte runes (fourByteRunes), and the
// site is as long as the configuration allows.
//
// # Why the index list is asked rather than written down
//
// Which columns are in an index is the schema's fact, and the next index
// is one line of it. So every B-tree on beacon_events is read from the
// catalog, and every text column in a key must be one of two things: a
// column the maximal row actually filled - then the write above is the
// measurement - or one named in serverBounded with what bounds it. A new
// index on a column a visitor fills is measured here the day it is
// added; one on a column nothing here fills fails until somebody says
// what bounds it.
func TestWriter_RealTimescaleDB_AMaximalRowFitsEveryIndex(t *testing.T) {
	ctx := context.Background()
	// The longest identifier siteIDPattern accepts.
	site := "beacon-maxrow-" + strings.Repeat("x", 64-len("beacon-maxrow-"))
	if !siteIDPattern.MatchString(site) {
		t.Fatalf("%q is not a site id this configuration accepts; the case is not the maximal one", site)
	}
	writer := newTestWriter(t, site, WriterConfig{})
	reader := testdb.Pool(t, testdb.Reader)

	indexes := btreeKeys(t, reader)
	requireDeclaredIndexes(t, indexes)

	// Every field a visitor sends, past its cap, in runes that compress
	// to nothing. BuildRow is what cuts them - the function the handler
	// calls - so the row is the largest the product makes, not the
	// largest this test can type.
	query := []string{}
	for i, p := range standardParams {
		query = append(query, p+"="+fourByteRunes(10+i, 2*maxCampaignValueLen))
	}
	maximal := BuildRow(Event{
		Site:     site,
		Type:     TypeEvent,
		Name:     fourByteRunes(1, 2*maxNameLen),
		URL:      "/" + fourByteRunes(2, 2*maxPathLen) + "?" + strings.Join(query, "&"),
		Title:    fourByteRunes(3, 2*maxTitleLen),
		Referrer: "https://" + strings.Repeat("h", 300) + ".example/" + fourByteRunes(4, 2*maxPathLen),
		Language: fourByteRunes(5, 2*maxLanguageLen),
		ScreenW:  1 << 30,
		ScreenH:  1 << 30,
	}, Enrichment{
		Time:      time.Now().UTC(),
		VisitorID: strings.Repeat("f", 2*visitorIDBytes),
		Country:   "TR",
	}, DefaultCampaignPolicy())

	batch := []Row{testRow(site, "/once"), maximal, testRow(site, "/sonra")}
	n, err := writer.WriteRows(ctx, batch)
	if err != nil {
		t.Fatalf("the largest row BuildRow makes failed its batch: %v\n"+
			"Every other visitor's events in that batch went with it. A value in an index "+
			"key needs a bound in bytes below 2704 for the whole entry - see "+
			"maxCampaignValueBytes for the one there is", err)
	}
	if n != int64(len(batch)) {
		t.Fatalf("wrote %d rows of %d", n, len(batch))
	}
	if got := countRows(t, site); got != len(batch) {
		t.Fatalf("%d rows in the database, want %d", got, len(batch))
	}

	// What bounds the indexed text columns a visitor does not fill.
	serverBounded := map[string]string{
		"site_id": "siteIDPattern - 64 ASCII characters - and an event is refused " +
			"unless its site is configured",
		"visitor_id": "an HMAC the server computes, 32 hex characters",
	}
	checked := 0
	for _, key := range indexes {
		for _, col := range key.text {
			if _, ok := serverBounded[col]; ok {
				continue
			}
			var bytes int
			err := reader.QueryRow(ctx, `SELECT octet_length(`+pgx.Identifier{col}.Sanitize()+`)
				FROM beacon_events WHERE site_id = $1 AND event_type = $2`, site, TypeEvent).Scan(&bytes)
			if err != nil {
				t.Fatalf("reading %s back: %v", col, err)
			}
			// Half of the campaign bound: enough to say the maximal row
			// reached the column, which is all this asks. How large a
			// value the column can take is the write's question.
			if bytes < maxCampaignValueBytes/2 {
				t.Errorf("%s keys on %s, and the maximal row put %d bytes there.\n"+
					"The write above measures only the columns it fills. If a visitor can "+
					"reach %s, fill it in the event above; if the server bounds it, say how "+
					"in serverBounded", key.name, col, bytes, col)
				continue
			}
			checked++
		}
	}
	// The campaign index's three columns, at least. A catalog query that
	// found nothing would pass everything above.
	if checked < 3 {
		t.Fatalf("measured %d visitor-filled index columns, want at least the campaign "+
			"index's three; the catalog query found %v", checked, indexes)
	}
}

// btreeKey is one B-tree index on beacon_events and its text key columns.
type btreeKey struct {
	name string
	text []string
}

// btreeKeys asks the catalog for every B-tree on beacon_events.
//
// Expression keys are refused rather than skipped: their size is the
// expression's, which this test cannot read off a column.
func btreeKeys(t *testing.T, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) []btreeKey {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT i.relname, bool_or(x.indexprs IS NOT NULL),
		       coalesce(array_agg(a.attname ORDER BY k.ord)
		                FILTER (WHERE a.atttypid IN ('text'::regtype, 'varchar'::regtype)), '{}')
		FROM pg_index x
		JOIN pg_class i ON i.oid = x.indexrelid
		JOIN pg_am am ON am.oid = i.relam AND am.amname = 'btree'
		CROSS JOIN LATERAL unnest(x.indkey) WITH ORDINALITY AS k(attnum, ord)
		LEFT JOIN pg_attribute a ON a.attrelid = x.indrelid AND a.attnum = k.attnum
		WHERE x.indrelid = 'public.beacon_events'::regclass
		GROUP BY i.relname
		ORDER BY i.relname`)
	if err != nil {
		t.Fatalf("reading the indexes: %v", err)
	}
	defer rows.Close()
	var out []btreeKey
	for rows.Next() {
		var k btreeKey
		var expr bool
		if err := rows.Scan(&k.name, &expr, &k.text); err != nil {
			t.Fatal(err)
		}
		if expr {
			t.Errorf("%s keys on an expression; its entry size is not a column's, so say "+
				"here what bounds it", k.name)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// declaredIndex is how schema.sql names an index it creates.
var declaredIndex = regexp.MustCompile(`(?m)^CREATE (?:UNIQUE )?INDEX IF NOT EXISTS (\w+)\s+ON beacon_events\b`)

// requireDeclaredIndexes asks for every index schema.sql creates on
// beacon_events before anything is measured against them.
//
// Asked, because the answer was no. internal/panel/preflight dropped a
// column of this table on the shared database to show its check notices
// a missing one, and PostgreSQL drops the indexes that use a column with
// it - the campaign index's predicate names click_source. Putting the
// column back did not put the index back, so every suite after it on
// that database ran against a beacon_events without the index this test
// is about, and would have passed with the defect in place.
func requireDeclaredIndexes(t *testing.T, have []btreeKey) {
	t.Helper()
	present := map[string]bool{}
	for _, k := range have {
		present[k.name] = true
	}
	var missing []string
	declared := declaredIndex.FindAllStringSubmatch(SchemaSQL, -1)
	for _, m := range declared {
		if !present[m[1]] {
			missing = append(missing, m[1])
		}
	}
	if len(declared) == 0 {
		t.Fatal("schema.sql declares no index on beacon_events by the pattern this reads; the pattern is stale")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("this database lacks %v, which internal/beacon/schema.sql creates.\n"+
			"Something changed the table's shape after the schema was applied - a suite "+
			"that alters a shared table, most likely. Re-apply the schema file; then find "+
			"what removed it, because the next run will measure the same missing thing", missing)
	}
}

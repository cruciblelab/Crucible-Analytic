//go:build integration

package testdb

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// Every text column in a B-tree key, and what bounds it in bytes
// (PLAN §H7, §H8).
//
// # The rule this holds
//
// PostgreSQL refuses a B-tree entry larger than 2704 bytes (8 KB pages),
// and refuses the statement that wrote it with it. Where the statement is
// a batch's COPY - the beacon's - one oversized value loses every row in
// the batch: three campaign values within their rune caps made a
// 3104-byte entry, and fifty visitors' events went with the fifty-first
// (H7). Where it is one INSERT, the record of something is lost: a failed
// sign-in, a log line (H8). A cap counted in runes is not a cap in bytes,
// and a value that compresses hides the limit from any test that repeats
// one character.
//
// So every text column that is part of an index key is listed here with
// what bounds it. Read from the catalogue, both ways: a new index on a
// text column fails here until somebody says what bounds it, and an entry
// for a column no index keys any more fails too, so the list cannot keep
// a reason for something that is gone.
//
// The reasons are prose and cannot measure anything. The measuring is
// done where the value comes from somebody else - each of those entries
// names the test that writes the largest value the product makes into
// the real index.
var indexedText = map[string]string{
	// Site identifiers. siteid.Valid (64 ASCII) in the configuration files;
	// siteid.Keyable (256 bytes, storable) for the panel's live list, the
	// beacon applying it, and every panel site page (siteAccess).
	"beacon_events.site_id":        "an event is refused unless its site is in the allowlist, and the allowlist keeps only siteid.Keyable entries (TestTheLiveSiteListKeepsOnlySiteIdentifiers)",
	"traffic_snapshots.site_id":    "the collector's own site_id, siteid.Valid at load",
	"traffic_rollup.site_id":       "copied from traffic_snapshots",
	"traffic_rollup_state.site_id": "copied from traffic_snapshots",
	"visitor_sketch.site_id":       "copied from traffic_snapshots (only with timescaledb_toolkit)",
	"visitor_sketch_state.site_id": "copied from traffic_snapshots (only with timescaledb_toolkit)",
	"panel_settings.site_id":       "a site page's site, siteid.Keyable at siteAccess",
	"panel_site_members.site_id":   "a site page's site, or the configured list the panel re-validates on read (TestTheConfiguredSitesLeaveOutWhatNoSiteCanBe)",
	"panel_member_invites.site_id": "a site page's site, siteid.Keyable at siteAccess",
	"panel_operations.site_id":     "BeginOperation refuses a site that is not siteid.Keyable (TestAnOperationsSiteMustBeASite); details go in target",
	"panel_audit_log.site_id":      "a site page's site, siteid.Keyable at siteAccess",
	"panel_logs.site_id":           "a log attribute, cut to logsink.maxKeyBytes (TestAnOverLongSiteOrOperationStillWritesItsLine)",

	// Visitor-supplied.
	"beacon_events.utm_source":   "maxCampaignValueBytes, 512 (TestWriter_RealTimescaleDB_AMaximalRowFitsEveryIndex)",
	"beacon_events.utm_medium":   "maxCampaignValueBytes, 512 (TestWriter_RealTimescaleDB_AMaximalRowFitsEveryIndex)",
	"beacon_events.utm_campaign": "maxCampaignValueBytes, 512 (TestWriter_RealTimescaleDB_AMaximalRowFitsEveryIndex)",

	// Typed into a form.
	"panel_login_attempts.email": "loginKey: storable and cut to panel.MaxEmailBytes, 254 - typed by anybody (TestAnAddressNoAccountCanHaveIsAnOrdinaryFailure)",
	"panel_users.email":          "panel.UsableEmail at every constructor (TestNoConstructorTakesAnAddressNoAccountCanHave)",

	// Written by this product, from nothing anybody typed.
	"beacon_events.visitor_id":    "an HMAC the beacon computes, 32 hex characters",
	"panel_api_tokens.sha256":     "a SHA-256 hex digest, 64 characters",
	"panel_dev_access.sha256":     "a SHA-256 hex digest, 64 characters",
	"panel_member_invites.sha256": "a SHA-256 hex digest, 64 characters",
	"panel_owner_claims.sha256":   "a SHA-256 hex digest, 64 characters",
	"panel_recovery_codes.sha256": "a SHA-256 hex digest, 64 characters",
	"panel_sessions.token":        "a token the panel draws",
	"panel_operations.id":         "an id the panel mints",
	"panel_logs.operation_id":     "an operation id, cut to logsink.maxKeyBytes like the site (TestAnOverLongSiteOrOperationStillWritesItsLine)",
	"panel_settings.key":          "a registry key, written by the panel's code",
	"panel_settings.scope":        "'global' or 'site', a CHECK",
	"service_heartbeat.service":   "current_user, a role name of at most 63 bytes",
}

// optional are entries whose table only some installations have.
var optionalIndexedText = map[string]bool{
	"visitor_sketch.site_id":       true,
	"visitor_sketch_state.site_id": true,
}

func TestEveryIndexedTextColumnIsBoundedInBytes(t *testing.T) {
	pool := Admin(t)
	rows, err := pool.Query(context.Background(), `
		SELECT DISTINCT c.relname || '.' || a.attname
		FROM pg_index x
		JOIN pg_class i ON i.oid = x.indexrelid
		JOIN pg_class c ON c.oid = x.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_am am ON am.oid = i.relam AND am.amname = 'btree'
		CROSS JOIN LATERAL unnest(x.indkey[0:x.indnkeyatts - 1]) AS k(attnum)
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		WHERE n.nspname = 'public'
		  AND a.atttypid IN ('text'::regtype, 'varchar'::regtype, 'bpchar'::regtype)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		present[col] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// The campaign index's three, at least: a catalogue query that found
	// nothing would pass both directions below.
	for _, must := range []string{"beacon_events.utm_source", "panel_login_attempts.email", "panel_logs.site_id"} {
		if !present[must] {
			t.Fatalf("the catalogue query does not see %s; it is not reading what it should", must)
		}
	}

	var unlisted, stale []string
	for col := range present {
		if _, ok := indexedText[col]; !ok {
			unlisted = append(unlisted, col)
		}
	}
	for col := range indexedText {
		if !present[col] && !optionalIndexedText[col] {
			stale = append(stale, col)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	if len(unlisted) > 0 {
		t.Errorf("text columns in an index key with nothing here saying what bounds them in bytes:\n  %s\n"+
			"PostgreSQL refuses an index entry over 2704 bytes along with the statement. If "+
			"anybody but this product's code can put a value there, bound it in bytes "+
			"(textsafe.FitBytes) and measure the largest value in the real index; then list it",
			strings.Join(unlisted, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("listed, and no longer in any index key: %s", strings.Join(stale, ", "))
	}
}

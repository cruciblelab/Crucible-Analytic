//go:build integration

package upgradepath

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The two databases this package builds. Fixed names rather than random
// ones, so a run that dies leaves two databases to find rather than a
// growing pile.
const (
	freshDB    = "ca_upgradepath_fresh"
	upgradedDB = "ca_upgradepath_upgraded"
	// walkDB is reused by every subtest of the walk over the whole tag
	// history: one at a time, dropped after each, so the run leaves
	// three databases behind at worst rather than one per release.
	walkDB = "ca_upgradepath_walk"
)

// roles are the five service roles, which exist cluster-wide already;
// only their rights inside a new database do not.
var roles = []string{"collector", "beacon_writer", "analytics_reader", "panel_user", "schema_admin"}

var (
	ready       bool
	superDSN    string
	previousTag string
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		superDSN = os.Getenv("CA_SUPERUSER_DSN")
		if superDSN == "" {
			fmt.Fprintln(os.Stderr,
				"CA_SUPERUSER_DSN is not set; the upgrade-path suite needs databases of its own")
			return m.Run()
		}

		tag, err := lastRelease()
		if err != nil {
			fmt.Fprintf(os.Stderr, "finding the previous release: %v\n", err)
			return 1
		}
		previousTag = tag

		ctx := context.Background()
		admin, err := pgxpool.New(ctx, superDSN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connecting as the superuser: %v\n", err)
			return 1
		}
		defer admin.Close()

		for _, db := range []string{freshDB, upgradedDB} {
			// FORCE on the drop too: a scratch database left by a run
			// that died makes the next run fail in its fixture, before
			// any test has asserted anything.
			for _, sql := range []string{
				`DROP DATABASE IF EXISTS ` + db + ` WITH (FORCE)`,
				`CREATE DATABASE ` + db,
			} {
				if _, err := admin.Exec(ctx, sql); err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", sql, err)
					return 1
				}
			}
		}
		defer func() {
			for _, db := range []string{freshDB, upgradedDB} {
				if _, err := admin.Exec(context.Background(),
					`DROP DATABASE IF EXISTS `+db+` WITH (FORCE)`); err != nil {
					fmt.Fprintf(os.Stderr, "dropping %s: %v\n", db, err)
				}
			}
		}()

		if err := buildFresh(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "building the fresh install: %v\n", err)
			return 1
		}
		if err := buildUpgraded(ctx, upgradedDB, previousTag); err != nil {
			fmt.Fprintf(os.Stderr, "building the upgraded deployment: %v\n", err)
			return 1
		}
		ready = true
		return m.Run()
	}())
}

// surfaces are everything a deployment's shape is made of, each read
// from the catalogue rather than from a list written here - so a table,
// column, sequence, function, policy, constraint or index added
// tomorrow is covered the day it is added.
//
// The first four are privileges: what a role may do. The rest are the
// shape the privileges sit in, and they were missing at first, which was
// the wrong way round - an unapplied GRANT breaks a feature and somebody
// notices, while unforced row security leaves a door open and nobody
// does, because the feature works.
//
// One definition, several tests: the newest-release comparison below,
// the walk over every release, and the row-security rule at the bottom.
// Two copies of this SQL would be two definitions of what "the same
// deployment" means.
var surfaces = []struct {
	what string
	sql  string
}{
	{"table", `
			SELECT grantee || ' | ' || table_name || ' | ' ||
			       string_agg(DISTINCT privilege_type, ',' ORDER BY privilege_type)
			FROM information_schema.role_table_grants
			WHERE grantee = ANY($1) AND table_schema = 'public'
			GROUP BY grantee, table_name
			ORDER BY 1`},
	{"column", `
			SELECT grantee || ' | ' || table_name || '.' || column_name || ' | ' ||
			       string_agg(DISTINCT privilege_type, ',' ORDER BY privilege_type)
			FROM information_schema.column_privileges
			WHERE grantee = ANY($1) AND table_schema = 'public'
			GROUP BY grantee, table_name, column_name
			ORDER BY 1`},
	{"sequence", `
			SELECT r.rolname || ' | ' || c.relname || ' | USAGE:' ||
			       has_sequence_privilege(r.rolname, c.oid, 'USAGE')::text || ' SELECT:' ||
			       has_sequence_privilege(r.rolname, c.oid, 'SELECT')::text
			FROM pg_class c, pg_roles r
			WHERE c.relkind = 'S' AND c.relnamespace = 'public'::regnamespace
			  AND r.rolname = ANY($1)
			ORDER BY 1`},
	{"function", `
			SELECT r.rolname || ' | ' || p.proname || '(' ||
			       pg_get_function_identity_arguments(p.oid) || ') | ' ||
			       has_function_privilege(r.rolname, p.oid, 'EXECUTE')::text
			FROM pg_proc p, pg_roles r
			WHERE p.pronamespace = 'public'::regnamespace AND r.rolname = ANY($1)
			ORDER BY 1`},

	// Row-level security, and the four surfaces below it, are not
	// privileges - which is why they were missing here and why that
	// mattered more than the privileges did.
	//
	// A GRANT that an upgrade failed to apply leaves a feature broken,
	// and somebody notices. RLS that an upgrade failed to enable leaves
	// the door open and nobody notices, because the feature works. This
	// project's first rule is that no table is left open, and the walk
	// that checked whether an upgrade ends up where an install would
	// have was not asking it.
	//
	// $1 is accepted and unused on these five, so every surface has the
	// same signature and rowsOf can pass the role list to all of them.
	{"rls", `
			SELECT c.relname || ' | enabled:' || c.relrowsecurity::text ||
			       ' forced:' || c.relforcerowsecurity::text
			FROM pg_class c
			WHERE c.relnamespace = 'public'::regnamespace
			  AND c.relkind IN ('r', 'p')
			  AND $1::text[] IS NOT NULL
			ORDER BY 1`},
	{"policy", `
			SELECT tablename || ' | ' || policyname || ' | ' || cmd ||
			       ' | to:' || COALESCE((SELECT string_agg(r::text, ',' ORDER BY r::text) FROM unnest(roles) r), '') ||
			       ' | using:' || COALESCE(qual, '') ||
			       ' | check:' || COALESCE(with_check, '')
			FROM pg_policies
			WHERE schemaname = 'public' AND $1::text[] IS NOT NULL
			ORDER BY 1`},
	{"owner", `
			SELECT c.relkind::text || ' ' || c.relname || ' | ' || pg_get_userbyid(c.relowner)
			FROM pg_class c
			WHERE c.relnamespace = 'public'::regnamespace
			  AND c.relkind IN ('r', 'p', 'S', 'v', 'm')
			  AND $1::text[] IS NOT NULL
			ORDER BY 1`},
	{"constraint", `
			SELECT c.conrelid::regclass::text || ' | ' || c.conname || ' | ' ||
			       pg_get_constraintdef(c.oid)
			FROM pg_constraint c
			WHERE c.connamespace = 'public'::regnamespace AND $1::text[] IS NOT NULL
			ORDER BY 1`},
	// Indexes, because a missing one is the O group's entire subject:
	// a deployment that answers correctly and too slowly, which the
	// panel reports as "could not be read" - a sentence about the
	// database for a query that is merely scanning.
	//
	// Definitions rather than names: an index that survived the upgrade
	// under the same name with a different column order is the case a
	// name comparison would pass.
	{"index", `
			SELECT schemaname || '.' || indexname || ' | ' || indexdef
			FROM pg_indexes
			WHERE schemaname = 'public' AND $1::text[] IS NOT NULL
			ORDER BY 1`},
	{"columnshape", `
			SELECT a.attrelid::regclass::text || '.' || a.attname || ' | ' ||
			       format_type(a.atttypid, a.atttypmod) ||
			       ' | notnull:' || a.attnotnull::text ||
			       ' | default:' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
			FROM pg_attribute a
			JOIN pg_class c ON c.oid = a.attrelid
			LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			WHERE c.relnamespace = 'public'::regnamespace
			  AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped
			  AND $1::text[] IS NOT NULL
			ORDER BY 1`},
}

// TestAnUpgradedDeploymentHasTheSameShapeAsAFreshInstall.
//
// A difference in either direction is a failure. Missing is the defect
// this was written for; extra is the other half of the same problem, a
// deployment that upgraded into rights a fresh install would not give
// it.
func TestAnUpgradedDeploymentHasTheSameShapeAsAFreshInstall(t *testing.T) {
	requireReady(t)

	for _, s := range surfaces {
		t.Run(s.what, func(t *testing.T) {
			compareSurface(t, s.what, s.sql, upgradedDB, previousTag)
		})
	}
}

// compareSurface is the one definition of "an upgrade ended up where an
// install would have", for one surface.
func compareSurface(t *testing.T, what, sql, db, tag string) {
	t.Helper()

	fresh := rowsOf(t, freshDB, sql)
	upgraded := rowsOf(t, db, sql)

	// A surface that reads empty on both sides would compare equal and
	// prove nothing.
	if len(fresh) == 0 {
		t.Fatalf("the %s query returned nothing on a fresh install; "+
			"an empty comparison passes whatever the upgrade did", what)
	}

	have := map[string]bool{}
	for _, row := range upgraded {
		have[row] = true
	}
	want := map[string]bool{}
	for _, row := range fresh {
		want[row] = true
	}

	for _, row := range fresh {
		if !have[row] {
			t.Errorf("a fresh install has this %s and an upgrade from %s does not:\n"+
				"    %s\n"+
				"The upgrade path runs the schema files and nothing else, so whatever "+
				"produces this is written only in release/sql/grants.sql. Put it in the "+
				"schema file that creates the object, guarded the way "+
				"internal/storage/schema.sql does.",
				what, tag, row)
		}
	}
	for _, row := range upgraded {
		if !want[row] {
			t.Errorf("an upgrade from %s has this %s and a fresh install does not:\n"+
				"    %s\n"+
				"Upgrading must not leave a deployment holding something installing "+
				"would not give it - for a privilege that is a right nobody granted, "+
				"and for anything else it is a shape this tree no longer describes.",
				tag, what, row)
		}
	}
}

// TestEveryReleasedVersionUpgradesToTheSameShape walks the whole
// tag history rather than the newest release.
//
// # Why one baseline is not enough
//
// The test above asks about the newest release whose schema differs
// from this tree's, and that baseline moves with the tree - which makes
// it able to hide the exact defect it exists to catch. Measured: delete
// the guarded GRANT block from internal/storage/schema.sql - the whole
// point of L4 - and the newest release's schema is suddenly *different*
// from this tree's, so it becomes the baseline, and applying it puts
// the block back. The comparison passed. The invariant had healed
// itself around the mutation.
//
// A customer is not a moving baseline. Somebody is on v0.19.0 and
// upgrades to this; somebody else is on v0.23.0. So every reachable
// release gets its own database, and each is asked the same four
// questions. A privilege that lives only in grants.sql then has
// nowhere to hide: the releases from before it was written are still
// in the list.
//
// The cost is one database per release, built and dropped in turn -
// measured at about a second each on this machine, and the reason they
// are not all built at once.
func TestEveryReleasedVersionUpgradesToTheSameShape(t *testing.T) {
	requireReady(t)
	ctx := context.Background()

	tags, err := releasesReachableFromHEAD()
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) < 2 {
		t.Fatalf("only %d release(s) reachable from HEAD; this test is a walk over the "+
			"history and a walk of one is the test above", len(tags))
	}

	admin, err := pgxpool.New(ctx, superDSN)
	if err != nil {
		t.Fatalf("connecting as the superuser: %v", err)
	}
	defer admin.Close()

	for _, tag := range tags {
		t.Run(tag, func(t *testing.T) {
			db := walkDB
			for _, sql := range []string{
				`DROP DATABASE IF EXISTS ` + db + ` WITH (FORCE)`,
				`CREATE DATABASE ` + db,
			} {
				if _, err := admin.Exec(ctx, sql); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			// Dropped on the way out, not left for the next subtest to
			// find: a database this suite abandoned is a database the
			// next run fails in its fixture.
			t.Cleanup(func() {
				if _, err := admin.Exec(context.Background(),
					`DROP DATABASE IF EXISTS `+db+` WITH (FORCE)`); err != nil {
					t.Errorf("dropping %s: %v", db, err)
				}
			})

			if err := buildUpgraded(ctx, db, tag); err != nil {
				t.Fatalf("building a deployment upgraded from %s: %v", tag, err)
			}
			for _, s := range surfaces {
				compareSurface(t, s.what, s.sql, db, tag)
			}
		})
	}
}

// releasesReachableFromHEAD is every release tag in this history except
// one pointing at HEAD itself, oldest first.
//
// Oldest first on purpose: the oldest release is the longest upgrade and
// the most likely to break, and a reader watching the subtests scroll
// past wants that answer before the easy ones.
func releasesReachableFromHEAD() ([]string, error) {
	out, err := git("tag", "--merged", "HEAD", "--sort=v:refname")
	if err != nil {
		return nil, err
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, tag := range strings.Fields(out) {
		at, err := git("rev-list", "-n", "1", tag)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(at) == strings.TrimSpace(head) {
			continue
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

// TestTheRowsSurviveTheUpgrade.
//
// The privileges are the half that was broken; this is the half that
// would be unforgivable. Both analytics tables and one panel table are
// seeded before the upgrade and counted after it.
func TestTheRowsSurviveTheUpgrade(t *testing.T) {
	requireReady(t)
	ctx := context.Background()

	pool := openAs(t, superDSN, upgradedDB)
	for _, q := range []struct {
		what string
		sql  string
		want int
	}{
		{"traffic_snapshots", `SELECT count(*) FROM traffic_snapshots WHERE site_id = 'upgrade-probe'`, 1},
		{"beacon_events", `SELECT count(*) FROM beacon_events WHERE site_id = 'upgrade-probe'`, 1},
		{"panel_users", `SELECT count(*) FROM panel_users WHERE email = 'upgrade-probe@example.invalid'`, 1},
	} {
		var got int
		if err := pool.QueryRow(ctx, q.sql).Scan(&got); err != nil {
			t.Fatalf("counting %s: %v", q.what, err)
		}
		if got != q.want {
			t.Errorf("%s holds %d rows after the upgrade, want %d - the rows seeded at %s are gone",
				q.what, got, q.want, previousTag)
		}
	}
}

// TestTheUpgradeStartedFromAnOlderSchema.
//
// Without this the suite could pass having compared two identical
// trees: if the previous tag ever resolved to HEAD, or its schema
// happened to equal this one, the privilege test above would be
// comparing a fresh install against a fresh install.
//
// lastRelease now picks its baseline on that same property, so this
// reads like a restatement of it - and is deliberately not derived from
// it. The two are independent: the fixture chooses, and this asserts
// what the choice has to be true of. Deriving the second from the first
// would make it pass by construction the day somebody loosens the
// choice, which is the only day it matters. Measured: making the
// difference check return true unconditionally is caught here and
// nowhere else.
//
// It also prints the span, which is the one number a reader wants when
// this package fails: how far back the upgrade being tested reaches.
func TestTheUpgradeStartedFromAnOlderSchema(t *testing.T) {
	requireReady(t)

	old, err := schemaAt(previousTag)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) == 0 {
		t.Fatalf("no schema files found at %s", previousTag)
	}

	var differs int
	for _, f := range schemafiles.InOrder {
		if old[f.Path] != f.SQL {
			differs++
		}
	}
	if differs == 0 {
		t.Errorf("every schema file at %s is byte-identical to this tree's, so the "+
			"upgraded database was never actually upgraded and the comparison is empty",
			previousTag)
	}
	t.Logf("upgrading from %s: %d of %d schema files differ", previousTag, differs, len(schemafiles.InOrder))
}

// TestNoTableEnablesRowSecurityWithoutForcingIt.
//
// # Why a rule rather than a list
//
// Row security that is enabled and not forced does nothing to the
// table's owner, and after release/sql/grants.sql runs the owner of
// every table is schema_admin - a role cmd/upgrader connects as. So
// "enabled, not forced" is not a weaker version of the rule the policies
// state; it is that rule being false for the one role holding the
// deployment's DDL credential.
//
// The shape this was found in is the shape this project keeps finding:
// seven tables had ENABLE and FORCE, two had only ENABLE. Nothing said
// which was intended, and both were written by the same hand. A list of
// the seven would have to be edited for the eighth; this asks the
// catalogue instead, so a table that enables row security tomorrow is
// covered the day it does.
//
// # Why it lives in this package
//
// Because the fixture is here. freshDB is built the way install.sh
// builds a deployment - every schema file, then the privilege matrix -
// and this question is about that database's shape. Building a
// twenty-second database to ask one more question of the same schema
// would be waste, and asking it of the shared development database
// would be asking about whatever shape other suites left behind.
func TestNoTableEnablesRowSecurityWithoutForcingIt(t *testing.T) {
	requireReady(t)

	rows := rowsOf(t, freshDB, `
		SELECT c.relname || ' | enabled:' || c.relrowsecurity::text ||
		       ' forced:' || c.relforcerowsecurity::text
		FROM pg_class c
		WHERE c.relnamespace = 'public'::regnamespace
		  AND c.relkind IN ('r', 'p')
		  AND $1::text[] IS NOT NULL
		ORDER BY 1`)
	if len(rows) == 0 {
		t.Fatal("no tables found in the fresh install; this check is reading nothing")
	}

	var enabled, forced int
	for _, row := range rows {
		if !strings.Contains(row, "enabled:true") {
			continue
		}
		enabled++
		if strings.Contains(row, "forced:true") {
			forced++
			continue
		}
		name := strings.SplitN(row, " | ", 2)[0]
		t.Errorf("%s has row security enabled and not forced.\n"+
			"Its policies therefore do not apply to its owner, which is schema_admin "+
			"in every deployment (release/sql/grants.sql transfers every table to it) "+
			"and the role cmd/upgrader connects as. Add:\n"+
			"    ALTER TABLE %s FORCE ROW LEVEL SECURITY;\n"+
			"or, if this table's policies are deliberately advisory, say so where the "+
			"ENABLE is and give this check a reason to skip it.", name, name)
	}

	// A table count of zero would make the loop above vacuous, and this
	// project has met that: a surface everybody trusts, asserting
	// nothing, because the thing it filters on stopped matching.
	if enabled == 0 {
		t.Fatal("no table in the fresh install has row security enabled at all.\n" +
			"Either the policies were removed - which is a much larger finding than " +
			"this test was written for - or this check no longer recognises the " +
			"catalogue's answer.")
	}
	t.Logf("%d of %d tables use row security, %d of those force it", enabled, len(rows), forced)
}

func requireReady(t *testing.T) {
	t.Helper()
	if !ready {
		t.Skip("set CA_SUPERUSER_DSN; this suite builds two databases of its own")
	}
}

func rowsOf(t *testing.T, db, sql string) []string {
	t.Helper()
	pool := openAs(t, superDSN, db)
	rows, err := pool.Query(context.Background(), sql, roles)
	if err != nil {
		t.Fatalf("querying %s: %v", db, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning from %s: %v", db, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading from %s: %v", db, err)
	}
	return out
}

// buildFresh does what release/install.sh does: every schema file, then
// the privilege matrix.
func buildFresh(ctx context.Context) error {
	if err := prepare(ctx, freshDB); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, swapDatabase(superDSN, freshDB))
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, f := range schemafiles.InOrder {
		if _, err := pool.Exec(ctx, f.SQL); err != nil {
			return fmt.Errorf("applying %s: %w", f.Path, err)
		}
	}
	grants, err := os.ReadFile(filepath.Join(repoRoot(), "release", "sql", "grants.sql"))
	if err != nil {
		return fmt.Errorf("reading grants.sql: %w", err)
	}
	if _, err := pool.Exec(ctx, string(grants)); err != nil {
		return fmt.Errorf("applying release/sql/grants.sql: %w", err)
	}
	return nil
}

// buildUpgraded does what a real deployment did: install the given
// release, put rows in it, then apply this tree's schema the way
// cmd/upgrader does - as schema_admin, schema files only, no grants.sql.
//
// The database and the release are both arguments because two tests
// need it: one for the newest release, one for every release in turn.
func buildUpgraded(ctx context.Context, db, tag string) error {
	if err := prepare(ctx, db); err != nil {
		return err
	}

	old, err := schemaAt(tag)
	if err != nil {
		return err
	}
	order, err := schemaOrderAt(tag)
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, swapDatabase(superDSN, db))
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, path := range order {
		if _, err := pool.Exec(ctx, old[path]); err != nil {
			return fmt.Errorf("applying %s at %s: %w", path, tag, err)
		}
	}
	oldGrants, err := fileAt(tag, "release/sql/grants.sql")
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, oldGrants); err != nil {
		return fmt.Errorf("applying grants.sql at %s: %w", tag, err)
	}

	// Rows a customer would already have. Written as the superuser
	// because the point is that they exist, not who wrote them.
	for _, sql := range []string{
		`INSERT INTO traffic_snapshots
		   (time, site_id, ip, ja4, prev_window_count, curr_window_count, request_rate, bot_score)
		 VALUES (now(), 'upgrade-probe', '198.51.100.0', 't13d', 0, 5, 0.1, 3)`,
		`INSERT INTO beacon_events (time, site_id, visitor_id, event_type, path)
		 VALUES (now(), 'upgrade-probe', 'v1', 'pageview', '/')`,
		`INSERT INTO panel_users (email, password_hash)
		 VALUES ('upgrade-probe@example.invalid', 'not-a-hash')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("seeding the old deployment: %w", err)
		}
	}

	// And the upgrade, as the role cmd/upgrader runs as. Applying it as
	// the superuser instead would make every new object owned by
	// postgres and invent an ownership difference this test would then
	// report as a defect.
	admin, err := pgxpool.New(ctx, schemaAdminDSN(db))
	if err != nil {
		return fmt.Errorf("connecting as schema_admin: %w", err)
	}
	defer admin.Close()
	for _, f := range schemafiles.InOrder {
		if _, err := admin.Exec(ctx, f.SQL); err != nil {
			return fmt.Errorf("upgrading with %s: %w", f.Path, err)
		}
	}
	return nil
}

func prepare(ctx context.Context, db string) error {
	pool, err := pgxpool.New(ctx, swapDatabase(superDSN, db))
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, sql := range []string{
		`CREATE EXTENSION IF NOT EXISTS timescaledb`,
		`GRANT CONNECT ON DATABASE ` + db + ` TO ` + strings.Join(roles, ", "),
		`GRANT USAGE ON SCHEMA public TO ` + strings.Join(roles, ", "),
		`GRANT CREATE ON SCHEMA public TO schema_admin`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("%s: %w", sql, err)
		}
	}

	// timescaledb_toolkit where the cluster has it, in both databases
	// identically.
	//
	// # Why it belongs in this comparison
	//
	// O3's two tables are created only where this extension is present,
	// and their GRANTs live in two places - the schema file and
	// release/sql/grants.sql - which is the exact arrangement L4 found
	// broken: an upgrade runs the schema files and nothing else, so a
	// table whose privileges are written only in grants.sql comes out of
	// an upgrade untouchable. Without the extension here, neither
	// database would have the tables and the diff would compare nothing.
	//
	// Asked of pg_available_extensions rather than attempted and
	// ignored, so that a cluster which has it never quietly skips the
	// comparison - and a cluster which does not still runs every other
	// surface. Both databases get the same answer because they are
	// prepared by this same function; if one had it and the other did
	// not, every surface would differ and the report would be noise.
	var available bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_available_extensions
		                WHERE name = 'timescaledb_toolkit')`).Scan(&available); err != nil {
		return fmt.Errorf("asking for timescaledb_toolkit: %w", err)
	}
	if available {
		if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS timescaledb_toolkit`); err != nil {
			return fmt.Errorf("timescaledb_toolkit: %w", err)
		}
	}
	return nil
}

// lastRelease is the newest release reachable from HEAD whose schema is
// not already this tree's.
//
// Derived rather than written down: a release cut tomorrow becomes the
// baseline without anybody editing this file, and the test then answers
// the question customers will actually be asking.
//
// # Why "whose schema differs" and not simply "the newest"
//
// The newest is what this asked first, and it made the suite fail on
// every commit that does not touch a schema file. The day after
// v0.24.0+L4 was cut, the newest release's twelve schema files were
// byte-identical to this tree's, so the database built by "upgrading"
// from it was built by applying the same files twice - and
// TestTheUpgradeStartedFromAnOlderSchema said so, correctly, and went
// red on a commit that had changed one Go query.
//
// Walking back to the last release whose schema actually differs fixes
// both halves of that. The fixture stops being vacuous between
// releases, and the span it covers is the one a customer really
// traverses: somebody on the last version with a different schema,
// arriving at this one. A release that changed no schema file needs no
// upgrade path tested, because there is no upgrade in it.
//
// # Why the failures below name the checkout
//
// This suite cannot run without the history it reads out of, and there
// are two ways to arrive without it. Both were met for real: the job
// that runs this package checked out at the default depth, which fetches
// no tags, and the message was
//
//	finding the previous release: no tag reachable from HEAD points
//	anywhere but HEAD
//
// - true, and about the wrong thing. There was no tag reachable from
// HEAD at all, because there was no tag. Diagnosing it took reading the
// workflow.
//
// It fails rather than skipping on purpose. An upgrade invariant that
// excuses itself when the checkout is thin is one that never runs, and
// the defect it exists to catch - a table whose GRANT lives only in
// grants.sql - is invisible on the fresh installs everything else
// tests. So: fail, and say which line of which file fixes it.
func lastRelease() (string, error) {
	shallow, err := git("rev-parse", "--is-shallow-repository")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(shallow) == "true" {
		return "", fmt.Errorf("this is a shallow checkout, and the previous release's " +
			"schema files cannot be read out of history that was never fetched; " +
			"a CI job that runs this package needs `fetch-depth: 0` on its " +
			"actions/checkout step, and a local clone needs `git fetch --unshallow`")
	}

	out, err := git("tag", "--merged", "HEAD", "--sort=-v:refname")
	if err != nil {
		return "", err
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	tags := strings.Fields(out)
	if len(tags) == 0 {
		return "", fmt.Errorf("this checkout has no tags, so there is no previous " +
			"release to upgrade from; actions/checkout fetches none at the default " +
			"depth, so a CI job that runs this package needs `fetch-depth: 0`, and " +
			"a local clone needs `git fetch --tags`")
	}

	var skipped []string
	for _, tag := range tags {
		at, err := git("rev-list", "-n", "1", tag)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(at) == strings.TrimSpace(head) {
			skipped = append(skipped, tag+" (is HEAD)")
			continue
		}
		differs, err := schemaDiffersFromThisTree(tag)
		if err != nil {
			return "", err
		}
		if !differs {
			skipped = append(skipped, tag+" (same schema)")
			continue
		}
		return tag, nil
	}
	return "", fmt.Errorf("no release reachable from HEAD has a schema this tree "+
		"would upgrade; skipped %s. Either every release carries this tree's schema "+
		"- in which case there is no upgrade to test and this package has nothing to "+
		"say - or the tags are unreachable", strings.Join(skipped, ", "))
}

// schemaDiffersFromThisTree reports whether a release's schema files are
// something this tree's would change.
//
// Byte comparison, not the schema version: a release that bumped the
// version without changing a file is not an upgrade, and a file edited
// without a version bump is one - and the second is the case worth
// catching, since it is how a GRANT gets added to a schema file.
//
// Asked in one direction only, from this tree's list. A release that
// *removed* a file and changed nothing else reads as "same schema" here
// and the walk goes further back - to a release that still has the file,
// which covers the removal as well and more besides. The other
// direction would be a branch no fixture can reach.
func schemaDiffersFromThisTree(tag string) (bool, error) {
	old, err := schemaAt(tag)
	if err != nil {
		return false, err
	}
	for _, f := range schemafiles.InOrder {
		if old[f.Path] != f.SQL {
			return true, nil
		}
	}
	return false, nil
}

// schemaOrderAt reads the previous release's own ordering, out of its
// own schemafiles.go. Reusing this tree's order would assume the two
// agree, which is exactly the kind of assumption this package exists to
// stop making.
func schemaOrderAt(tag string) ([]string, error) {
	src, err := fileAt(tag, "internal/schemafiles/schemafiles.go")
	if err != nil {
		return nil, err
	}
	paths := regexp.MustCompile(`\{"(internal/[^"]+\.sql)"`).FindAllStringSubmatch(src, -1)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no schema paths found in schemafiles.go at %s; "+
			"the list has changed shape and this extraction no longer reads it", tag)
	}
	out := make([]string, 0, len(paths))
	for _, m := range paths {
		out = append(out, m[1])
	}
	return out, nil
}

func schemaAt(tag string) (map[string]string, error) {
	order, err := schemaOrderAt(tag)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(order))
	for _, path := range order {
		sql, err := fileAt(tag, path)
		if err != nil {
			return nil, err
		}
		out[path] = sql
	}
	return out, nil
}

func fileAt(tag, path string) (string, error) {
	return git("show", tag+":"+path)
}

func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func schemaAdminDSN(db string) string {
	if dsn := os.Getenv("CA_DSN_schema_admin"); dsn != "" {
		return swapDatabase(dsn, db)
	}
	return fmt.Sprintf("postgres://schema_admin:schema_admin@127.0.0.1:5432/%s?sslmode=disable", db)
}

// swapDatabase points a DSN at another database on the same server.
func swapDatabase(dsn, db string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	rest := ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		rest = dsn[slash+q:]
	}
	return dsn[:slash+1] + db + rest
}

func openAs(t *testing.T, dsn, db string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), swapDatabase(dsn, db))
	if err != nil {
		t.Fatalf("connecting to %s: %v", db, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("no go.mod above " + dir)
		}
		dir = parent
	}
}

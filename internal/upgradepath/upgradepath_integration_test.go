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
		if err := buildUpgraded(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "building the upgraded deployment: %v\n", err)
			return 1
		}
		ready = true
		return m.Run()
	}())
}

// TestAnUpgradedDeploymentHasTheSamePrivilegesAsAFreshInstall.
//
// The four surfaces a role can hold something on, each read from the
// catalogue rather than from a list written here - so a table, column,
// sequence or function added tomorrow is covered the day it is added.
//
// A difference in either direction is a failure. Missing is the defect
// this was written for; extra is the other half of the same problem, a
// deployment that upgraded into rights a fresh install would not give
// it.
func TestAnUpgradedDeploymentHasTheSamePrivilegesAsAFreshInstall(t *testing.T) {
	requireReady(t)

	surfaces := []struct {
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
	}

	for _, s := range surfaces {
		t.Run(s.what, func(t *testing.T) {
			fresh := rowsOf(t, freshDB, s.sql)
			upgraded := rowsOf(t, upgradedDB, s.sql)

			// A surface that reads empty on both sides would compare
			// equal and prove nothing.
			if len(fresh) == 0 {
				t.Fatalf("the %s privilege query returned nothing on a fresh install; "+
					"an empty comparison passes whatever the upgrade did", s.what)
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
					t.Errorf("a fresh install has %s privilege %q and an upgrade from %s does not.\n"+
						"The upgrade path runs the schema files and nothing else, so this privilege "+
						"is written only in release/sql/grants.sql. Put it in the schema file that "+
						"creates the object, guarded the way internal/storage/schema.sql does.",
						s.what, row, previousTag)
				}
			}
			for _, row := range upgraded {
				if !want[row] {
					t.Errorf("an upgrade from %s has %s privilege %q and a fresh install does not; "+
						"a deployment must not gain rights by upgrading that installing would not give it",
						previousTag, s.what, row)
				}
			}
		})
	}
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

// buildUpgraded does what a real deployment did: install the previous
// release, put rows in it, then apply this tree's schema the way
// cmd/upgrader does - as schema_admin, schema files only, no grants.sql.
func buildUpgraded(ctx context.Context) error {
	if err := prepare(ctx, upgradedDB); err != nil {
		return err
	}

	old, err := schemaAt(previousTag)
	if err != nil {
		return err
	}
	order, err := schemaOrderAt(previousTag)
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, swapDatabase(superDSN, upgradedDB))
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, path := range order {
		if _, err := pool.Exec(ctx, old[path]); err != nil {
			return fmt.Errorf("applying %s at %s: %w", path, previousTag, err)
		}
	}
	oldGrants, err := fileAt(previousTag, "release/sql/grants.sql")
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, oldGrants); err != nil {
		return fmt.Errorf("applying grants.sql at %s: %w", previousTag, err)
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
	admin, err := pgxpool.New(ctx, schemaAdminDSN(upgradedDB))
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
	return nil
}

// lastRelease is the newest tag reachable from HEAD that HEAD is not.
//
// Derived rather than written down: a release cut tomorrow becomes the
// baseline without anybody editing this file, and the test then answers
// the question customers will actually be asking.
func lastRelease() (string, error) {
	out, err := git("tag", "--merged", "HEAD", "--sort=-v:refname")
	if err != nil {
		return "", err
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	for _, tag := range strings.Fields(out) {
		at, err := git("rev-list", "-n", "1", tag)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(at) != strings.TrimSpace(head) {
			return tag, nil
		}
	}
	return "", fmt.Errorf("no tag reachable from HEAD points anywhere but HEAD")
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

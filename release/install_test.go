//go:build release

// The install script, verified by running it against a real database.
//
// Under the `release` tag with the package tests, and for the same
// reason: it takes a database and a minute, and neither belongs in a
// merge gate.
package release

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

// superuserDSN is a connection this test may create databases and roles
// with. It must NOT be one of the four service roles: the whole point of
// the checks below is that a service role holds no authority it was not
// granted, and a superuser holds all of it.
func superuserDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CA_SUPERUSER_DSN")
	if dsn == "" {
		t.Skip("set CA_SUPERUSER_DSN to a superuser connection that is not one of the four service roles")
	}
	return dsn
}

// runInstall runs install.sh against a scratch database and returns its
// combined output plus whether it exited zero.
func runInstall(t *testing.T, db string, extraEnv ...string) (string, bool) {
	t.Helper()
	root := repoRoot(t)

	base := superuserDSN(t)
	cmd := exec.Command("./release/install.sh")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"SUPERUSER_DSN="+dsnFor(base, db),
		"DB_NAME="+db,
		"CONF_DIR="+t.TempDir(),
		"PREFIX="+t.TempDir(),
		// Nothing here may touch the machine's systemd or its users.
		"LOG_DIR="+t.TempDir(),
		"STATE_DIR="+t.TempDir(),
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// dsnFor swaps the database in a DSN.
func dsnFor(base, db string) string {
	if i := strings.LastIndex(base, "/"); i >= 0 {
		if q := strings.Index(base[i:], "?"); q >= 0 {
			return base[:i+1] + db + base[i+q:]
		}
		return base[:i+1] + db
	}
	return base
}

// demoteServiceSuperusers strips SUPERUSER from any of the four service
// roles that has it, and restores it afterwards.
//
// PostgreSQL roles are cluster-wide, so a development machine whose
// `collector` role is a superuser - which this project's own dev setup
// does, deliberately - gives every scratch database a collector that
// holds every privilege. install.sh then refuses, correctly: a superuser
// service role has silently lost every isolation property the design
// rests on.
//
// The check is right and the cluster is the anomaly, so the test adapts
// to the cluster rather than the other way round. A CI runner with a
// fresh Postgres has none of these roles and this does nothing.
func demoteServiceSuperusers(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsnFor(superuserDSN(t), "postgres"))
	if err != nil {
		t.Skipf("cannot reach the superuser connection: %v", err)
	}
	defer admin.Close()

	rows, err := admin.Query(ctx, `
		SELECT rolname FROM pg_roles
		WHERE rolsuper AND rolname IN
		  ('collector','beacon_writer','analytics_reader','panel_user')`)
	if err != nil {
		t.Fatal(err)
	}
	var demoted []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		demoted = append(demoted, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, name := range demoted {
		t.Logf("%s is a superuser on this cluster; demoting for the test and restoring after", name)
		if _, err := admin.Exec(ctx, "ALTER ROLE "+name+" NOSUPERUSER"); err != nil {
			t.Fatalf("demoting %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		back, err := pgxpool.New(context.Background(), dsnFor(superuserDSN(t), "postgres"))
		if err != nil {
			t.Errorf("cannot restore superuser on %v: %v", demoted, err)
			return
		}
		defer back.Close()
		for _, name := range demoted {
			if _, err := back.Exec(context.Background(), "ALTER ROLE "+name+" SUPERUSER"); err != nil {
				t.Errorf("restoring superuser on %s: %v", name, err)
			}
		}
	})
}

// scratchDatabase creates an empty database and drops it afterwards.
func scratchDatabase(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsnFor(superuserDSN(t), "postgres"))
	if err != nil {
		t.Skipf("cannot reach the superuser connection: %v", err)
	}
	t.Cleanup(admin.Close)

	exec := func(sql string) {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec("DROP DATABASE IF EXISTS " + name)
	exec("CREATE DATABASE " + name)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
	})
}

// TestInstallProducesTheIsolationItClaims is the phase.
//
// The role separation is half this system's security foundation and was
// typed by hand until now. A wrong GRANT does not fail: it produces an
// installation that serves customers and quietly does not have the
// property the design rests on.
func TestInstallProducesTheIsolationItClaims(t *testing.T) {
	const db = "ca_install_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	out, ok := runInstall(t, db)
	if !ok {
		t.Fatalf("install.sh failed:\n%s", out)
	}
	if !strings.Contains(out, "every assertion holds") {
		t.Errorf("the install did not report a verified privilege matrix:\n%s", out)
	}

	// Asked again here, independently of the script, so this test is not
	// merely reading the script's own opinion of itself.
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsnFor(superuserDSN(t), db))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	cases := []struct {
		role, table, priv string
		want              bool
		why               string
	}{
		{"collector", "traffic_snapshots", "INSERT", true, "the collector writes its own table"},
		{"beacon_writer", "beacon_events", "INSERT", true, "the beacon writes its own table"},
		{"analytics_reader", "traffic_snapshots", "SELECT", true, "the API reads traffic"},
		{"analytics_reader", "beacon_events", "SELECT", true, "the API reads beacon events"},

		// The half that matters, and the half a GRANT block cannot show.
		{"panel_user", "traffic_snapshots", "SELECT", false, "the panel must never read analytics"},
		{"panel_user", "beacon_events", "SELECT", false, "the panel must never read analytics"},
		{"analytics_reader", "traffic_snapshots", "INSERT", false, "the read API must never write"},
		{"analytics_reader", "beacon_events", "INSERT", false, "the read API must never write"},
		{"beacon_writer", "beacon_events", "SELECT", false, "the beacon never reads back what it wrote"},
		{"collector", "beacon_events", "SELECT", false, "the collector has no business in the beacon's table"},
		{"panel_user", "panel_audit_log", "DELETE", false, "a compromised panel must not erase what it did"},
		{"panel_user", "panel_audit_log", "UPDATE", false, "a compromised panel must not rewrite what it did"},
	}

	for _, tc := range cases {
		var got bool
		err := pool.QueryRow(ctx,
			`SELECT has_table_privilege($1, $2, $3)`, tc.role, tc.table, tc.priv).Scan(&got)
		if err != nil {
			t.Fatalf("has_table_privilege(%s, %s, %s): %v", tc.role, tc.table, tc.priv, err)
		}
		if got != tc.want {
			t.Errorf("%s %s on %s = %v, want %v - %s",
				tc.role, tc.priv, tc.table, got, tc.want, tc.why)
		}
	}
}

// TestNoServiceRoleOwnsATable is the finding that made this test file
// worth more than the script.
//
// A table's owner holds every privilege on it implicitly, for ever,
// regardless of what was granted. So schemas applied over a connection
// authenticated as `collector` leave that role owning every table, and
// the isolation is void while looking perfect: the grants read correctly,
// a privilege listing reads correctly, and the panel can read analytics.
//
// This happened during the phase. The first run used a superuser DSN that
// happened to be the collector role, every GRANT applied correctly, and
// the verification reported that the collector could touch beacon_events.
func TestNoServiceRoleOwnsATable(t *testing.T) {
	const db = "ca_install_owner_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	if out, ok := runInstall(t, db); !ok {
		t.Fatalf("install.sh failed:\n%s", out)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsnFor(superuserDSN(t), db))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `
		SELECT tablename, tableowner FROM pg_tables
		WHERE schemaname = 'public'
		  AND tableowner IN ('collector','beacon_writer','analytics_reader','panel_user')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var table, owner string
		if err := rows.Scan(&table, &owner); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s is owned by %s: an owner holds every privilege implicitly, so every "+
			"isolation check above is void while reading as correct", table, owner)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestInstallRefusesAnInstallationWhoseIsolationIsWrong.
//
// Watching a clean install pass proves the check runs, not that it
// refuses. So each isolation property is broken in turn and the script is
// re-run: it must exit non-zero and name the assertion that failed.
//
// Re-running rather than installing fresh is the point - this is what an
// operator does after fixing something, and it is the run that has to
// notice a grant somebody added by hand in between.
func TestInstallRefusesAnInstallationWhoseIsolationIsWrong(t *testing.T) {
	const db = "ca_install_refuse_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	if out, ok := runInstall(t, db); !ok {
		t.Fatalf("the first install failed:\n%s", out)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsnFor(superuserDSN(t), db))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	cases := []struct {
		name   string
		break_ string
		undo   string
		names  string
	}{
		{
			name:   "the panel can read analytics",
			break_: "GRANT SELECT ON traffic_snapshots TO panel_user",
			undo:   "REVOKE SELECT ON traffic_snapshots FROM panel_user",
			names:  "the panel CANNOT read traffic",
		},
		{
			name:   "the read API can write",
			break_: "GRANT INSERT ON traffic_snapshots TO analytics_reader",
			undo:   "REVOKE INSERT ON traffic_snapshots FROM analytics_reader",
			names:  "the API CANNOT write traffic",
		},
		{
			name:   "the audit log can be erased",
			break_: "GRANT DELETE ON panel_audit_log TO panel_user",
			undo:   "REVOKE DELETE ON panel_audit_log FROM panel_user",
			names:  "nobody can erase the audit log",
		},
		{
			name:   "a service role became a superuser",
			break_: "ALTER ROLE panel_user SUPERUSER",
			undo:   "ALTER ROLE panel_user NOSUPERUSER",
			names:  "none of the four is a superuser",
		},
		{
			// The mail account is the only recoverable secret in this
			// database, so it is the only table where a stray SELECT
			// hands somebody a working credential rather than a number.
			//
			// The realistic way this happens is not malice: panel_smtp
			// gets appended to the panel_settings GRANT by somebody
			// tidying up a list, and two internet-facing processes
			// silently gain the ability to read a mail password.
			name:   "the collector can read the mail password",
			break_: "GRANT SELECT ON panel_smtp TO collector",
			undo:   "REVOKE SELECT ON panel_smtp FROM collector",
			names:  "the collector CANNOT read the mail account",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tc.break_); err != nil {
				t.Fatalf("breaking it: %v", err)
			}
			defer func() {
				if _, err := pool.Exec(context.Background(), tc.undo); err != nil {
					t.Fatalf("putting it back: %v", err)
				}
			}()

			out, ok := runInstall(t, db)
			if ok {
				t.Fatalf("install.sh finished on an installation where %s:\n%s", tc.name, out)
			}
			if !strings.Contains(out, tc.names) {
				t.Errorf("it refused, but did not name %q - so it may have failed for an "+
					"unrelated reason:\n%s", tc.names, out)
			}
			if !strings.Contains(out, "refusing to finish") {
				t.Errorf("it exited non-zero without the refusal message:\n%s", out)
			}
		})
	}
}

// TestInstallIsIdempotent. An operator re-runs this after fixing
// something, and a second run that failed - or that rotated the role
// passwords its configuration files already hold - would make the script
// something people run once and then avoid.
func TestInstallIsIdempotent(t *testing.T) {
	const db = "ca_install_idempotent_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	first, ok := runInstall(t, db)
	if !ok {
		t.Fatalf("the first install failed:\n%s", first)
	}
	second, ok := runInstall(t, db)
	if !ok {
		t.Fatalf("the second install failed:\n%s", second)
	}

	// The first run creates roles and prints their passwords; the second
	// must not, because it does not read the configuration files those
	// passwords went into and cannot put new ones there.
	if !strings.Contains(second, "already exists, keeping its password") {
		t.Errorf("the second run did not keep the existing roles:\n%s", second)
	}
	for _, role := range []string{"collector", "beacon_writer", "analytics_reader", "panel_user"} {
		if strings.Contains(second, fmt.Sprintf("  %-18s", role)) {
			t.Errorf("the second run printed a new password for %s, which would not match "+
				"the configuration file the first run's password went into", role)
		}
	}
	if !strings.Contains(second, "every assertion holds") {
		t.Errorf("the second run did not re-verify the privilege matrix:\n%s", second)
	}
}

// TestTheGrantsAreOneFile: KURULUM.md must point at release/sql, not
// carry its own copy.
//
// A privilege block in both a document and a script drifts, and it drifts
// in one direction every time: the script gets fixed because it runs, and
// the document keeps telling the next operator to grant something else.
func TestTheGrantsAreOneFile(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "KURULUM.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(body)

	if !strings.Contains(doc, "release/sql/grants.sql") {
		t.Error("KURULUM.md does not point at release/sql/grants.sql")
	}
	// A GRANT statement in the document is a second copy by definition.
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "GRANT ") && !strings.Contains(trimmed, "release/sql") {
			t.Errorf("KURULUM.md carries its own GRANT, which will drift from the file that "+
				"actually runs: %q", trimmed)
		}
	}
}

// TestInstallCatchesAMismatchedIPKey is the specific gap PLAN.md's F2
// names, and the reason this script owns it rather than preflight.
//
// The key goes into two configuration files and the two services never
// read each other's - they must not. Preflight sees one file, so it can
// check the key is present and cannot check the two are the same.
//
// Different keys do not fail anything. The crossover join matches
// nothing, silently, so the one view that demonstrates this product's
// whole claim reports zero and reads like a quiet week.
//
// The first implementation of this check generated a key, wrote it to
// both files, and then compared them - comparing the script's own output
// with itself, which can never disagree. A hand-edited beacon.toml with a
// mistyped key passed it without a word. That is why the assertion below
// edits a file between two runs rather than trusting a single one.
func TestInstallCatchesAMismatchedIPKey(t *testing.T) {
	const db = "ca_install_ipkey_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	conf := t.TempDir()
	run := func() (string, bool) {
		cmd := exec.Command("./release/install.sh")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(),
			"SUPERUSER_DSN="+dsnFor(superuserDSN(t), db),
			"DB_NAME="+db, "CONF_DIR="+conf,
			"PREFIX="+t.TempDir(), "LOG_DIR="+t.TempDir(), "STATE_DIR="+t.TempDir(),
		)
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}

	out, ok := run()
	if !ok {
		t.Fatalf("the first install failed:\n%s", out)
	}
	if !strings.Contains(out, "ip_hash_key matches in both files") {
		t.Fatalf("the first install did not report a matching key:\n%s", out)
	}

	// Both files must have ended up with the same key, read back from the
	// files rather than from the script's own claim about them.
	collectorKey := readIPKey(t, filepath.Join(conf, "collector.toml"))
	beaconKey := readIPKey(t, filepath.Join(conf, "beacon.toml"))
	if collectorKey == "" {
		t.Fatal("collector.toml has no ip_hash_key")
	}
	if collectorKey != beaconKey {
		t.Fatalf("the two files disagree after a clean install")
	}

	// Now the failure this exists for: a key copied into the second file
	// by hand, wrongly.
	replaceIPKey(t, filepath.Join(conf, "beacon.toml"), "elle-yanlis-kopyalanmis-anahtar")

	out, ok = run()
	if ok {
		t.Fatalf("the install finished with two different ip_hash_key values:\n%s", out)
	}
	if !strings.Contains(out, "different ip_hash_key") {
		t.Errorf("it refused without naming the mismatch, so it may have failed for an "+
			"unrelated reason:\n%s", out)
	}
	// The message must not print either key: a mismatch is worth
	// reporting, the secret is not.
	if strings.Contains(out, collectorKey) {
		t.Error("the refusal printed the actual key")
	}
}

// TestInstallDoesNotRotateAKeyAlreadyInUse.
//
// Rotating it would break the pseudonym of every row already stored: the
// visitor id and the crossover join are both derived from it, so a new
// key silently disconnects today's traffic from yesterday's.
func TestInstallDoesNotRotateAKeyAlreadyInUse(t *testing.T) {
	const db = "ca_install_norotate_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	conf := t.TempDir()
	run := func() (string, bool) {
		cmd := exec.Command("./release/install.sh")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(),
			"SUPERUSER_DSN="+dsnFor(superuserDSN(t), db),
			"DB_NAME="+db, "CONF_DIR="+conf,
			"PREFIX="+t.TempDir(), "LOG_DIR="+t.TempDir(), "STATE_DIR="+t.TempDir(),
		)
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}

	if out, ok := run(); !ok {
		t.Fatalf("the first install failed:\n%s", out)
	}
	before := readIPKey(t, filepath.Join(conf, "collector.toml"))

	if out, ok := run(); !ok {
		t.Fatalf("the second install failed:\n%s", out)
	}
	after := readIPKey(t, filepath.Join(conf, "collector.toml"))

	if before != after {
		t.Error("re-running rotated the ip_hash_key, which would disconnect every row " +
			"already stored from everything written after it")
	}
}

// readIPKey pulls ip_hash_key out of a TOML file.
func readIPKey(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "ip_hash_key") {
			continue
		}
		if _, value, ok := strings.Cut(trimmed, "="); ok {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

// replaceIPKey rewrites the key in a file, the way a wrong copy-paste
// would.
func replaceIPKey(t *testing.T, path, key string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ip_hash_key") {
			line = `ip_hash_key = "` + key + `"`
		}
		out = append(out, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestInstallGeneratesAUsablePanelSecretKey, and does not rotate it.
//
// Two properties in one test because the second is only interesting if
// the first holds. A key the script writes but internal/sealed cannot
// parse would produce a panel that refuses to start with "secret_key:
// the encryption key must be 32 bytes" - and the person reading that
// message did not write the key, the installer did.
//
// So this asserts the link between the shell and the Go: whatever
// install.sh produces has to be something ParseKey accepts and something
// that round-trips a password. A test that only counted characters would
// pass on 64 characters of anything.
func TestInstallGeneratesAUsablePanelSecretKey(t *testing.T) {
	const db = "ca_install_secretkey_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	conf := t.TempDir()
	run := func() (string, bool) {
		cmd := exec.Command("./release/install.sh")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(),
			"SUPERUSER_DSN="+dsnFor(superuserDSN(t), db),
			"DB_NAME="+db, "CONF_DIR="+conf,
			"PREFIX="+t.TempDir(), "LOG_DIR="+t.TempDir(), "STATE_DIR="+t.TempDir(),
		)
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}

	if out, ok := run(); !ok {
		t.Fatalf("the first install failed:\n%s", out)
	}

	first := readTOMLValue(t, filepath.Join(conf, "panel.toml"), "secret_key")
	if first == "" {
		t.Fatal("install.sh did not write a secret_key into panel.toml")
	}

	key, err := sealed.ParseKey(first)
	if err != nil {
		t.Fatalf("install.sh wrote a secret_key the panel cannot parse: %v (%q)", err, first)
	}
	box, err := key.Seal("panel_smtp.password", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	got, err := key.Open("panel_smtp.password", box)
	if err != nil || got != "hunter2" {
		t.Fatalf("the generated key does not round-trip: %q, %v", got, err)
	}

	// The commented-out line in panel.example.toml must have been
	// replaced rather than left alongside a second definition - TOML
	// takes the first, so a duplicate key would leave the panel reading
	// whichever the substitution missed.
	if n := countTOMLKey(t, filepath.Join(conf, "panel.toml"), "secret_key"); n != 1 {
		t.Errorf("panel.toml has %d uncommented secret_key lines, want 1", n)
	}

	if out, ok := run(); !ok {
		t.Fatalf("the second install failed:\n%s", out)
	}
	second := readTOMLValue(t, filepath.Join(conf, "panel.toml"), "secret_key")
	if first != second {
		t.Error("re-running rotated the panel secret_key; every stored mail password " +
			"would stop opening, and invitations would quietly stop being delivered " +
			"while the panel reported itself healthy")
	}
}

// readTOMLValue pulls a top-level key out of a TOML file, ignoring
// commented lines.
func readTOMLValue(t *testing.T, path, key string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, key) {
			continue
		}
		if _, value, ok := strings.Cut(trimmed, "="); ok {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

// countTOMLKey counts uncommented definitions of a key.
func countTOMLKey(t *testing.T, path, key string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if name, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(name) == key {
			n++
		}
	}
	return n
}

// TestInstallClosesTheDefaultsNobodyChose is the H5 audit, kept as a
// test.
//
// Every property below was measured as *wrong* on a real installation
// before harden.sql existed, and none of them appears as a missing GRANT
// in any privilege listing - which is why the install looked correct.
//
// Asked of a real TimescaleDB rather than of the script's output: what
// matters is the state the database ended in, and "the REVOKE ran" and
// "the REVOKE took effect" are different facts. The loop in harden.sql
// proved that on its first execution by aborting half way through on a
// procedure while reporting success.
func TestInstallClosesTheDefaultsNobodyChose(t *testing.T) {
	const db = "ca_install_harden_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	if out, ok := runInstall(t, db); !ok {
		t.Fatalf("the install failed:\n%s", out)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsnFor(superuserDSN(t), db))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	t.Run("telemetry is off", func(t *testing.T) {
		// Read through a fresh connection: ALTER DATABASE ... SET takes
		// effect for sessions started after it, so the pool above is
		// what an application would see.
		var level string
		if err := pool.QueryRow(ctx,
			`SELECT current_setting('timescaledb.telemetry_level', true)`).Scan(&level); err != nil {
			t.Fatal(err)
		}
		if level != "off" {
			t.Errorf("telemetry_level = %q; TimescaleDB reports to telemetry.timescale.com "+
				"every twenty-four hours by default, which contradicts a product whose premise "+
				"is that a customer's traffic never leaves their machine", level)
		}
	})

	t.Run("PUBLIC cannot connect", func(t *testing.T) {
		var public bool
		if err := pool.QueryRow(ctx,
			`SELECT has_database_privilege('public', current_database(), 'CONNECT')`).Scan(&public); err != nil {
			t.Fatal(err)
		}
		if public {
			t.Error("any role on the cluster can connect to this database; TimescaleDB's catalog " +
				"is world-readable, so a connected stranger can enumerate the hypertables and chunks")
		}
		// And the four still can, which is the half that would break a
		// working installation if the REVOKE were too broad.
		for _, role := range []string{"collector", "beacon_writer", "analytics_reader", "panel_user"} {
			var ok bool
			if err := pool.QueryRow(ctx,
				`SELECT has_database_privilege($1, current_database(), 'CONNECT')`, role).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Errorf("%s can no longer connect; the hardening revoked more than it should", role)
			}
		}
	})

	t.Run("no role can schedule a background job", func(t *testing.T) {
		var open int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE p.proname IN ('add_job','alter_job','delete_job','run_job',
			                    'add_retention_policy','remove_retention_policy',
			                    'add_compression_policy','remove_compression_policy',
			                    'add_continuous_aggregate_policy','remove_continuous_aggregate_policy',
			                    'add_reorder_policy','remove_reorder_policy')
			  AND n.nspname NOT IN ('pg_catalog','information_schema')
			  AND has_function_privilege('public', p.oid, 'EXECUTE')`).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open != 0 {
			t.Errorf("%d job-management routines are still executable by PUBLIC; a job outlives "+
				"the session, the pool and a restart of the application that created it", open)
		}
	})

	// And the thing that proves the three above are not vacuous: a
	// service role really is refused now.
	//
	// SET ROLE rather than a second connection, because the role
	// passwords are generated by install.sh into a config file this test
	// never reads. A superuser that has SET ROLE to a non-superuser is
	// checked as that role - the superuser bypass goes with the
	// identity - so this asks the question the deployment will ask.
	t.Run("a service role is refused in practice", func(t *testing.T) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()

		if _, err := conn.Exec(ctx, `SET ROLE panel_user`); err != nil {
			t.Fatalf("SET ROLE panel_user: %v", err)
		}
		defer func() { _, _ = conn.Exec(context.Background(), `RESET ROLE`) }()

		var jobID int
		err = conn.QueryRow(ctx, `SELECT add_job('pg_sleep', '1 hour')`).Scan(&jobID)
		if err == nil {
			_, _ = conn.Exec(ctx, `RESET ROLE`)
			_, _ = conn.Exec(ctx, `SELECT delete_job($1)`, jobID)
			t.Fatalf("panel_user scheduled job %d - a role with no rights outside the panel_* "+
				"tables just left behind something that survives a restart", jobID)
		}
		if !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("add_job failed for the wrong reason: %v", err)
		}
	})
}

// TestTheHeartbeatRefusesACrossServiceWrite is the assertion verify.sql
// cannot make.
//
// Four services write to one table, so "only your own row" comes from
// row-level security rather than from a privilege - and no privilege
// query can see it. has_table_privilege reports UPDATE for all four,
// correctly, because all four do hold UPDATE; what stops the beacon
// rewriting the collector's row is a policy, and a policy is only
// visible by trying.
//
// Why it matters: without it a compromised beacon could write "collector,
// healthy, beat_at: now" over the collector's row and hide an outage from
// the one page built to show it.
func TestTheHeartbeatRefusesACrossServiceWrite(t *testing.T) {
	const db = "ca_install_heartbeat_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	if out, ok := runInstall(t, db); !ok {
		t.Fatalf("the install failed:\n%s", out)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsnFor(superuserDSN(t), db))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// SET ROLE rather than a second connection: the role passwords are
	// generated into a config file this test never reads, and a
	// superuser that has SET ROLE to a non-superuser is checked as that
	// role - RLS included, since only the table's owner and a superuser
	// bypass policies, and SET ROLE gives up both.
	as := func(t *testing.T, role string, fn func(conn *pgxpool.Conn)) {
		t.Helper()
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, "SET ROLE "+role); err != nil {
			t.Fatalf("SET ROLE %s: %v", role, err)
		}
		defer func() { _, _ = conn.Exec(context.Background(), "RESET ROLE") }()
		fn(conn)
	}

	// Each service writes its own row, which must work or the feature is
	// pointless.
	for _, role := range []string{"collector", "beacon_writer", "analytics_reader"} {
		as(t, role, func(conn *pgxpool.Conn) {
			_, err := conn.Exec(ctx, `
				INSERT INTO service_heartbeat (service, version, started_at, beat_at)
				VALUES (current_user, 'v-test', now(), now())`)
			if err != nil {
				t.Fatalf("%s could not write its own heartbeat row: %v", role, err)
			}
		})
	}

	t.Run("a service cannot insert another's row", func(t *testing.T) {
		as(t, "beacon_writer", func(conn *pgxpool.Conn) {
			_, err := conn.Exec(ctx, `
				INSERT INTO service_heartbeat (service, started_at, beat_at)
				VALUES ('panel_user', now(), now())`)
			if err == nil {
				t.Fatal("the beacon wrote a row belonging to panel_user")
			}
			if !strings.Contains(err.Error(), "row-level security") {
				t.Errorf("the insert failed for the wrong reason: %v", err)
			}
		})
	})

	t.Run("a service cannot overwrite another's row", func(t *testing.T) {
		as(t, "beacon_writer", func(conn *pgxpool.Conn) {
			tag, err := conn.Exec(ctx,
				`UPDATE service_heartbeat SET version = 'FALSIFIED' WHERE service = 'collector'`)
			if err != nil {
				t.Fatalf("the update errored rather than matching nothing: %v", err)
			}
			// RLS filters rather than refuses, so this is zero rows
			// affected rather than an error. Asserted on the count for
			// that reason - a test expecting an error here would fail
			// against correct behaviour.
			if tag.RowsAffected() != 0 {
				t.Errorf("the beacon updated %d of the collector's rows", tag.RowsAffected())
			}
		})

		// And the collector's row really is untouched, read back
		// independently of the connection that tried.
		var version string
		if err := pool.QueryRow(ctx,
			`SELECT version FROM service_heartbeat WHERE service = 'collector'`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != "v-test" {
			t.Errorf("the collector's row now says %q", version)
		}
	})

	t.Run("a service cannot rename its own row into another's", func(t *testing.T) {
		// Update the row you are allowed to touch, and change its key
		// to somebody else's.
		//
		// This passes with or without an explicit WITH CHECK - measured,
		// after a mutation of the schema left it green: a policy with no
		// WITH CHECK uses its USING expression for the post-image too.
		// The test asserts the property rather than the mechanism, which
		// is why it keeps holding when the mechanism changes.
		as(t, "analytics_reader", func(conn *pgxpool.Conn) {
			_, err := conn.Exec(ctx,
				`UPDATE service_heartbeat SET service = 'collector' WHERE service = current_user`)
			if err == nil {
				t.Fatal("a service renamed its own heartbeat row into another service's")
			}
			if !strings.Contains(err.Error(), "row-level security") {
				t.Errorf("the rename failed for the wrong reason: %v", err)
			}
		})
	})

	t.Run("the panel reads every row and writes none of them", func(t *testing.T) {
		as(t, "panel_user", func(conn *pgxpool.Conn) {
			var n int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM service_heartbeat`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n < 3 {
				t.Errorf("the panel sees %d heartbeat rows, want at least the three just written", n)
			}
			tag, err := conn.Exec(ctx, `UPDATE service_heartbeat SET version = 'x' WHERE service = 'collector'`)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("the panel rewrote %d rows it does not own", tag.RowsAffected())
			}
		})
	})
}

// TestInstallHonoursTheDatabaseItWasGiven.
//
// The test that did not exist, and its absence is the whole story.
//
// psql_db used to pass SUPERUSER_DSN through untouched, so DB_NAME was
// silently ignored whenever a DSN was given: the script created the
// named database, left it empty, and applied every schema, every grant
// and every REVOKE to whatever database the DSN happened to point at. It
// then reported success, because verify.sql asks current_database() -
// and so it was checking the database it had just hardened by accident.
//
// Every existing test here passes dsnFor(superuserDSN(t), db), which
// already names the target. That is the single arrangement in which the
// two cannot disagree, so a dozen runs of install.sh proved nothing
// about the one line that was wrong. This test is the arrangement they
// were all missing: a DSN naming one database, DB_NAME naming another.
//
// The `sudo -u postgres` path was always correct, which is why this
// never appeared on a hand-installed server - only on the path a
// container uses, where the database is reached over TCP.
func TestInstallHonoursTheDatabaseItWasGiven(t *testing.T) {
	const db = "ca_install_target"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	// The DSN points at `postgres`, the maintenance database, and DB_NAME
	// at the scratch one. An install that ignores the second lands
	// everything in the first.
	base := superuserDSN(t)
	root := repoRoot(t)
	cmd := exec.Command("./release/install.sh")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"SUPERUSER_DSN="+dsnFor(base, "postgres"),
		"DB_NAME="+db,
		"CONF_DIR="+t.TempDir(),
		"PREFIX="+t.TempDir(),
		"LOG_DIR="+t.TempDir(),
		"STATE_DIR="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}

	ctx := context.Background()
	target, err := pgxpool.New(ctx, dsnFor(base, db))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	// The database it was told to install into has the schema.
	var tables int
	if err := target.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables < 10 {
		t.Errorf("%s holds %d tables after an install into it; the schemas went somewhere else", db, tables)
	}

	// And the one it was merely connected through does not.
	//
	// Named tables rather than a count: the maintenance database on a
	// developer's cluster may legitimately hold other things, and a
	// count would turn somebody else's table into a failure here.
	maintenance, err := pgxpool.New(ctx, dsnFor(base, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	for _, table := range []string{"traffic_snapshots", "beacon_events", "panel_users"} {
		var present bool
		if err := maintenance.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
			  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname = 'public' AND c.relname = $1)`, table).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if present {
			t.Errorf("the maintenance database now holds %s; the install went into the database it "+
				"connected through rather than the one it was given", table)
		}
	}

	// The hardening landed on the target too. ALTER DATABASE and REVOKE
	// take an explicit name, so they could be right while the schemas
	// were wrong - which is exactly what happened: the two halves
	// disagreed and verify.sql was the only thing that noticed.
	var telemetry string
	if err := target.QueryRow(ctx,
		`SELECT current_setting('timescaledb.telemetry_level', true)`).Scan(&telemetry); err != nil {
		t.Fatal(err)
	}
	if telemetry != "off" {
		t.Errorf("telemetry on %s is %q; the hardening was applied to another database", db, telemetry)
	}
}

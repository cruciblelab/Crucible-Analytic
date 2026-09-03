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
	"github.com/cruciblelab/crucible-analytic/internal/collector"
	"github.com/cruciblelab/crucible-analytic/internal/profile"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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

	// --no-systemd, and it is load-bearing rather than tidy.
	//
	// The comment below used to say this suite touches neither systemd
	// nor the machine's users, and it was false: redirecting LOG_DIR and
	// STATE_DIR moves two directories and nothing else. Measured on a
	// machine where this had been running as root - four unit files in
	// /etc/systemd/system and a `crucible` system account, written every
	// run by a suite whose comment said it wrote neither.
	//
	// On a CI runner the same step fails rather than succeeding, because
	// there is no root there, which is how it was finally noticed: the
	// merge gate had been red on every push since the systemd stage was
	// added, one line from the end of a script that had already created
	// four roles, seven schemas and three secrets.
	cmd := exec.Command("./release/install.sh", "--no-systemd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"SUPERUSER_DSN="+dsnFor(base, db),
		"DB_NAME="+db,
		"CONF_DIR="+t.TempDir(),
		"PREFIX="+t.TempDir(),
		// These move the log tree and the state directory into the
		// test's own space. What keeps systemd and the service account
		// untouched is the flag above, not these.
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
		cmd := exec.Command("./release/install.sh", "--no-systemd")
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
		cmd := exec.Command("./release/install.sh", "--no-systemd")
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
		cmd := exec.Command("./release/install.sh", "--no-systemd")
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
	cmd := exec.Command("./release/install.sh", "--no-systemd")
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

// TestNoSystemdWritesNoUnitFiles.
//
// The claim this suite makes about the machine it runs on, checked
// rather than asserted in a comment.
//
// It is here because the comment version of this claim was false for as
// long as it existed: runInstall redirected LOG_DIR and STATE_DIR and
// said that meant systemd was untouched, while every run as root wrote
// four unit files into /etc/systemd/system and created a `crucible`
// account. Nobody noticed, because on a developer's machine the writes
// succeed silently and on CI the failure looked like a permissions
// problem with the runner.
//
// So the flag is measured. A test suite that modifies the machine it
// runs on is one that behaves differently the second time.
func TestNoSystemdWritesNoUnitFiles(t *testing.T) {
	const db = "ca_install_nosystemd_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	before := unitFiles(t)

	out, ok := runInstall(t, db)
	if !ok {
		t.Fatalf("install.sh failed:\n%s", out)
	}

	// It has to say so, not just do it. A step that skips in silence is
	// one nobody can tell from a step that ran.
	if !strings.Contains(out, "no service units written") {
		t.Errorf("the install did not report that it skipped systemd:\n%s", out)
	}

	after := unitFiles(t)
	if len(after) != len(before) {
		t.Errorf("unit files changed: %d before, %d after (%v). "+
			"A test run must not install services on the machine running it",
			len(before), len(after), after)
	}
}

// unitFiles lists the crucible units currently on this machine.
func unitFiles(t *testing.T) []string {
	t.Helper()
	found, err := filepath.Glob("/etc/systemd/system/crucible-*.service")
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestTheServicesCanReadWhatInstallWrote.
//
// The stage nothing had ever run.
//
// Every other test in this file passes --no-systemd, and the comment on
// runInstall calls that flag "load-bearing" - correctly, because a test
// suite must not install services on the machine running it. What went
// unnoticed is what the flag was carrying: the branch behind it creates
// the service account and installs the units, and *nothing else in this
// repository ever entered it*.
//
// What was in there: install.sh created the `crucible` account, installed
// four units that run as it, and left the configuration directory mode
// 0750 owned by root:root with four 0640 root:root files inside. The
// account could not enter the directory, let alone read a file. Every one
// of the four services failed at startup on its own configuration file.
//
// A whole installation that does not start, produced by the script whose
// job is to produce one that does. It survived a phase specifically about
// the release package because the tests all took the door that led round
// it.
//
// So this one goes through the door. It needs root, and it says what it
// touches rather than moving quietly: the units go to a scratch
// SYSTEMD_DIR, and the account is removed afterwards if this test was
// what created it.
func TestTheServicesCanReadWhatInstallWrote(t *testing.T) {
	const db = "ca_install_perms_test"

	if os.Geteuid() != 0 {
		// Not a failure. The systemd stage needs root by design and says
		// so; a machine without it can still run every other test here.
		t.Skip("the systemd stage needs root (it creates a system account)")
	}
	for _, tool := range []string{"useradd", "su"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH; this test runs as the service account", tool)
		}
	}

	const runAs = "crucible"
	const runAsUpgrader = "crucible-upgrader"
	for _, account := range []string{runAs, runAsUpgrader} {
		if _, err := exec.Command("id", "-u", account).Output(); err != nil {
			t.Cleanup(func() { _ = exec.Command("userdel", account).Run() })
		}
	}

	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	confDir := t.TempDir()
	systemdDir := t.TempDir()

	// t.TempDir() builds a chain of 0700 directories owned by whoever
	// runs the test, so the service account cannot traverse it - and that
	// would fail this test for a reason install.sh had nothing to do
	// with. The chain is the harness's artifact; what is under test is
	// what install.sh does to confDir itself and to the files in it.
	for dir := confDir; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if err := os.Chmod(dir, 0o711); err != nil {
			t.Fatalf("opening the temp chain: %v", err)
		}
	}

	root := repoRoot(t)
	cmd := exec.Command("./release/install.sh") // no --no-systemd: that is the point
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"SUPERUSER_DSN="+dsnFor(superuserDSN(t), db),
		"DB_NAME="+db,
		"CONF_DIR="+confDir,
		"SYSTEMD_DIR="+systemdDir,
		"PREFIX="+t.TempDir(),
		"LOG_DIR="+t.TempDir(),
		"STATE_DIR="+t.TempDir(),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install.sh failed:\n%s", out)
	}

	// It really did take the branch. Without this the test could pass by
	// having skipped the stage it exists to cover - the exact failure
	// being fixed, one level up.
	units, err := filepath.Glob(filepath.Join(systemdDir, "crucible-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(units) == 0 {
		t.Fatal("no units were installed, so the systemd stage did not run and " +
			"this test proved nothing")
	}

	// The question, asked the way the machine asks it.
	for _, name := range []string{
		"collector.toml", "beacon.toml", "analytics-api.toml", "panel.toml",
	} {
		path := filepath.Join(confDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not written: %v", name, err)
			continue
		}
		out, err := exec.Command("su", "-s", "/bin/sh", runAs, "-c",
			"cat "+path+" >/dev/null").CombinedOutput()
		if err != nil {
			t.Errorf("%s cannot read %s, so the service that reads it will not start.\n%s",
				runAs, name, strings.TrimSpace(string(out)))
		}
	}

	// And the half that is not about starting: a service reads its
	// configuration and must never rewrite it. root owning the file and
	// the account getting the group bit is what makes both true at once,
	// so both are asserted - a chown to the account would pass the check
	// above and quietly lose this one.
	panelToml := filepath.Join(confDir, "panel.toml")
	if err := exec.Command("su", "-s", "/bin/sh", runAs, "-c",
		"echo x >> "+panelToml).Run(); err == nil {
		t.Errorf("%s can write panel.toml. A compromised panel could point its own "+
			"next restart at a database of its choosing", runAs)
	}
}

// TestOnlyTheUpgraderCanReadTheDDLCredential.
//
// upgrader.toml carries schema_admin's DSN - the one connection in the
// deployment that can ALTER a table. The panel runs as `crucible` and its
// own database role holds no privilege on the analytics tables at all,
// which is the isolation B6 and H5 established and that the whole design
// rests on. A file mode is enough to undo it: a panel that could read
// upgrader.toml could connect as the role that owns every table.
//
// So both halves are measured, against the file install.sh actually
// wrote, as the accounts it actually created.
//
// Split from the test above rather than added to it because the two ask
// opposite questions - "can this account read this file" and "can this
// account not" - and a single test that mixed them would report a
// permission that is too narrow and one that is too wide with the same
// sentence.
func TestOnlyTheUpgraderCanReadTheDDLCredential(t *testing.T) {
	const db = "ca_install_ddlperms_test"

	if os.Geteuid() != 0 {
		t.Skip("the systemd stage needs root (it creates system accounts)")
	}
	for _, tool := range []string{"useradd", "su"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH; this test runs as the service accounts", tool)
		}
	}

	const runAs = "crucible"
	const runAsUpgrader = "crucible-upgrader"
	for _, account := range []string{runAs, runAsUpgrader} {
		if _, err := exec.Command("id", "-u", account).Output(); err != nil {
			t.Cleanup(func() { _ = exec.Command("userdel", account).Run() })
		}
	}

	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	confDir := t.TempDir()
	for dir := confDir; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if err := os.Chmod(dir, 0o711); err != nil {
			t.Fatalf("opening the temp chain: %v", err)
		}
	}

	cmd := exec.Command("./release/install.sh")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(),
		"SUPERUSER_DSN="+dsnFor(superuserDSN(t), db),
		"DB_NAME="+db,
		"CONF_DIR="+confDir,
		"SYSTEMD_DIR="+t.TempDir(),
		"PREFIX="+t.TempDir(),
		"LOG_DIR="+t.TempDir(),
		"STATE_DIR="+t.TempDir(),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install.sh failed:\n%s", out)
	}

	upgraderToml := filepath.Join(confDir, "upgrader.toml")
	if _, err := os.Stat(upgraderToml); err != nil {
		t.Fatalf("install.sh wrote no upgrader.toml, so there is nothing to protect "+
			"and nothing here is measured: %v", err)
	}

	// It has to contain the credential, or the check below is a check on
	// an empty file.
	body, err := os.ReadFile(upgraderToml)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "postgres://schema_admin:") {
		t.Fatalf("upgrader.toml carries no schema_admin DSN; this test would then be "+
			"asserting that nobody can read a file with no secret in it:\n%s", body)
	}

	if err := exec.Command("su", "-s", "/bin/sh", runAsUpgrader, "-c",
		"cat "+upgraderToml+" >/dev/null").Run(); err != nil {
		t.Errorf("%s cannot read upgrader.toml, so the upgrader will not start: %v",
			runAsUpgrader, err)
	}

	if err := exec.Command("su", "-s", "/bin/sh", runAs, "-c",
		"cat "+upgraderToml+" >/dev/null").Run(); err == nil {
		t.Errorf("%s can read upgrader.toml. That account runs the panel, and the file "+
			"holds schema_admin's DSN - the panel could connect as the role that owns "+
			"every table it is forbidden to read", runAs)
	}
}

// TestTheInstallWritesTheDirectoriesItWasGiven.
//
// # The check a phase said it had done
//
// N2 fixed exactly this for LOG_DIR: the variable was accepted, used for
// one mkdir, and written into no configuration at all, so setting it
// created a directory nothing opened while every service went on using
// whatever its example said. That fix shipped, and its done-criterion
// read:
//
//	LOG_DIR verilen bir kurulumda dort yapilandirmanin dordu de o
//	dizini gosteriyor ... PREFIX / CONF_DIR / STATE_DIR ailesinin geri
//	kalaninin da ayni soruyu gectigi ayrica olculuyor - bir tanesi
//	yazilmiyorsa oburleri de sorgusuz sayilmamali.
//
// Neither half was measured. No test asserted that LOG_DIR reached the
// files, and the family was never asked. STATE_DIR turned out to carry
// the identical defect, still open three phases later: created,
// chowned, and named by nothing - so a collector went on writing its bot
// data wherever the example pointed, which on a systemd host is a
// directory ProtectSystem=strict will not let it write at all.
//
// That is a worse failure than the defect it hid. A plan entry does not
// go red. It is ticked once and then believed, so a criterion that
// promises a measurement and is never run reads exactly like one that
// passed.
//
// # Both directions, from one install
//
// A file that ships the key must come out naming the directory the
// install was given. A file that ships no key must still have none
// afterwards - a service whose example omits `dir` logs to stderr on
// purpose, and an installer that helpfully added one would turn a
// deliberate default into a surprise.
//
// The second half is why this does not run a second install with the
// variables unset. That would write to the real /var/log and /var/lib on
// whatever machine is running the suite - which succeeds silently as
// root and is exactly the thing TestNoSystemdWritesNoUnitFiles exists to
// forbid. The files that ship no key give the same evidence for free.
func TestTheInstallWritesTheDirectoriesItWasGiven(t *testing.T) {
	const db = "ca_install_dirs_test"
	demoteServiceSuperusers(t)
	scratchDatabase(t, db)

	confDir, logDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	out, ok := runInstall(t, db, "CONF_DIR="+confDir, "LOG_DIR="+logDir, "STATE_DIR="+stateDir)
	if !ok {
		t.Fatalf("install.sh failed:\n%s", out)
	}

	// The configuration files as installed, derived by reading the
	// directory rather than from a list here: a sixth service is covered
	// the day somebody adds one.
	installed, err := filepath.Glob(filepath.Join(confDir, "*.toml"))
	if err != nil || len(installed) == 0 {
		t.Fatalf("the install wrote no .toml files into %s (%v), so this test is "+
			"checking nothing", confDir, err)
	}

	// What each file looked like before the install, so "left alone" can
	// be checked rather than assumed. install.sh copies one example per
	// configuration; the mapping is read out of the script so a renamed
	// example cannot leave this comparing a file against nothing.
	examples := exampleForConfig(t, repoRoot(t))

	named, untouched := 0, 0
	for _, path := range installed {
		name := filepath.Base(path)
		example, ok := examples[name]
		if !ok {
			t.Errorf("%s was installed but install.sh copies no example to that name; "+
				"this test cannot tell what it looked like before", name)
			continue
		}
		_, shipped := tomlValue(t, filepath.Join(repoRoot(t), example), "dir")

		got, found := tomlValue(t, path, "dir")
		switch {
		case !shipped && found:
			t.Errorf("%s ships no dir key in %s and has one after the install (%q).\n"+
				"A service whose example omits dir logs to stderr on purpose; an "+
				"installer that adds one turns a deliberate default into a surprise",
				name, example, got)
		case !shipped:
			untouched++
		case !found:
			t.Errorf("%s ships a dir key in %s and has none after the install.\n"+
				"The rewrite removed a key instead of changing it", name, example)
		case got != logDir:
			t.Errorf("%s: dir = %q, want %q.\n"+
				"LOG_DIR was given and this file does not name it, so the install made "+
				"a directory nothing will open - the defect N2 was supposed to close",
				name, got, logDir)
		default:
			named++
		}
	}

	if named == 0 {
		t.Error("no installed configuration names the log directory it was given.\n" +
			"Either the rewrite has stopped working or every example has stopped " +
			"shipping an uncommented dir - and this check cannot tell which, which is " +
			"itself the reason to look")
	}
	if untouched == 0 {
		t.Log("every installed configuration ships a dir key, so the 'left alone' " +
			"half of this test proved nothing on this run")
	}
	t.Logf("%d configuration(s) named the log directory, %d correctly left without one",
		named, untouched)

	// The state directory, which is one file and one key: bot_data.path
	// is the only uncommented state path the examples ship.
	got, found := tomlValue(t, filepath.Join(confDir, "collector.toml"), "path")
	if !found {
		t.Fatal("collector.toml no longer carries an uncommented bot_data path, so the " +
			"state half of this test is blind; find where it went before deleting this")
	}
	if want := filepath.Join(stateDir, "known_bots.json"); got != want {
		t.Errorf("collector.toml: bot_data path = %q, want %q.\n"+
			"STATE_DIR was given, created and chowned, and then named by nothing - so "+
			"the collector writes bot data somewhere the install never prepared, and "+
			"on a systemd host cannot write it at all",
			got, want)
	}
}

// tomlValue reads one bare key's string value out of a TOML file.
//
// Deliberately crude, and deliberately not a TOML parser: what is being
// checked is a line a shell script rewrote with sed, and a parser could
// agree with the file about a mistake the sed made - a duplicated key,
// where the parser takes the last and the service takes the last and
// both are wrong together. So duplicates are reported rather than
// resolved.
//
// Commented lines are skipped, because a commented key is precisely what
// the install is supposed to leave alone.
func tomlValue(t *testing.T, path, key string) (string, bool) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var found []string
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		found = append(found, strings.Trim(strings.TrimSpace(value), `"`))
	}
	switch len(found) {
	case 0:
		return "", false
	case 1:
		return found[0], true
	default:
		t.Errorf("%s carries %d uncommented %s keys (%v); a configuration with a "+
			"duplicated key is one where the service and anything reading it can "+
			"disagree about which one wins",
			filepath.Base(path), len(found), key, found)
		return found[len(found)-1], true
	}
}

// exampleForConfig maps each installed configuration back to the example
// install.sh copied it from, by reading the script's own copy_example
// calls.
//
// Derived rather than listed, for the reason this whole file keeps
// finding: a list here would go on describing an arrangement the script
// had already left.
func exampleForConfig(t *testing.T, root string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "release", "install.sh"))
	if err != nil {
		t.Fatalf("reading install.sh: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "copy_example ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) != 3 {
			continue
		}
		out[fields[2]] = fields[1]
	}
	if len(out) == 0 {
		t.Fatal("install.sh no longer calls copy_example in a form this can read, so " +
			"every configuration would be compared against nothing")
	}
	return out
}

// TestInstallSaysWhatToDoWhenThereIsNoDatabase.
//
// # What was measured, and why it is a product defect rather than a
// cosmetic one
//
// Run on a machine with no PostgreSQL - which is the first minute of the
// first install for every customer who does not want containers - the
// whole output a person saw was psql's own connection error, printed
// twice, and not one sentence from this script about what to do next.
//
// This repository says what to do everywhere else. It was silent exactly
// where somebody is most likely to be stuck, and "stuck in the first
// minute" is the difference between a product and a repository.
//
// # Why the check belongs in preflight rather than where it failed
//
// The same rule the systemd check in that block already follows, learned
// the same way: a prerequisite found at its own stage stops the install
// after roles and schemas exist, leaving a half-installed machine. This
// test asserts both halves - the sentence, and that nothing was created.
func TestInstallSaysWhatToDoWhenThereIsNoDatabase(t *testing.T) {
	root := repoRoot(t)
	conf := t.TempDir()

	// A port nothing listens on. Deterministic, and it needs no database
	// at all - which is the point: this is the one test in this file that
	// describes a machine that does not have one.
	cmd := exec.Command("./release/install.sh", "--no-systemd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"SUPERUSER_DSN=postgres://nobody@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=2",
		"DB_NAME=ca_no_database_test",
		"CONF_DIR="+conf,
		"PREFIX="+t.TempDir(),
		"LOG_DIR="+t.TempDir(),
		"STATE_DIR="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	body := string(out)

	if err == nil {
		t.Fatalf("the install reported success against a database that is not there:\n%s", body)
	}

	// The sentence, and the two ways out it has to name. Checked by
	// content rather than by exit code alone: a script that fails with
	// psql's error and nothing else fails just as loudly and helps
	// nobody, which is the state this test was written from.
	for _, want := range []string{
		"cannot reach PostgreSQL",
		"Nothing has been created",
		"SUPERUSER_DSN",          // how to point it at a database elsewhere
		"KURULUM.md section 1.5", // the container path, for somebody who would rather not
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the failure does not mention %q:\n%s", want, body)
		}
	}

	// And nothing was written. A refusal that leaves half a
	// configuration behind is a refusal somebody has to clean up before
	// they can try again.
	entries, readErr := os.ReadDir(conf)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the install wrote %v before giving up; the check has to happen "+
			"before anything is created, not after", names)
	}
}

// TestAFreshInstallSaysTheSiteIdIsStillTheExample.
//
// # The one irreversible thing this script does not do
//
// install.sh writes four config files and does not set site_id: the
// examples ship "example-site" and a person decides. Every stored row is
// keyed by it, and changing it later renames nothing - it starts a
// second site whose history begins that day and leaves the old rows
// under the old name.
//
// So it is a required manual step, it is irreversible, and until this
// was written the installer neither did it nor mentioned it. A step in
// that shape is a step that gets skipped, and the cost of skipping it is
// discovered weeks later by somebody looking for their own data.
//
// Found by a mutation: deleting the paragraph broke no test.
func TestAFreshInstallSaysTheSiteIdIsStillTheExample(t *testing.T) {
	const db = "ca_siteid_test"
	scratchDatabase(t, db)

	out, ok := runInstall(t, db)
	if !ok {
		t.Fatalf("the install failed:\n%s", out)
	}

	for _, want := range []string{
		"example-site", // what it actually is right now
		"site_id",      // the key to change, by name
		"second site",  // what changing it later really does
		"KURULUM.md section 6",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the next-steps list does not mention %q.\n"+
				"This is the one irreversible decision the installer leaves to a "+
				"person, so it is the one it must not leave silent:\n%s", want, out)
		}
	}
}

// TestADryRunStillChecksTheDatabase.
//
// # The defect this is guarding, which a mutation found by not failing
//
// The database checks were first written inside `if DRY_RUN -eq 0`,
// which reads as caution and was the opposite of it. Measured on a
// machine with no database at all: `--dry-run` printed every stage,
// ended with "done", and exited 0.
//
// That is the worse failure of the two this file covers. A real run that
// says nothing leaves somebody confused; a dry run that says "ready"
// leaves them confident. It is the mode a person runs precisely to ask
// whether this machine is ready before committing to anything.
//
// It was fixed, and then a mutation put the guard back and every test in
// this file still passed - so the fix had no guard of its own, which is
// a fix that comes back. This is the guard.
//
// Nothing here needs a database: every check is a read, which is exactly
// why they are safe to run in a dry run.
func TestADryRunStillChecksTheDatabase(t *testing.T) {
	root := repoRoot(t)
	conf := t.TempDir()

	cmd := exec.Command("./release/install.sh", "--dry-run", "--no-systemd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"SUPERUSER_DSN=postgres://nobody@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=2",
		"DB_NAME=ca_dry_run_test",
		"CONF_DIR="+conf,
		"PREFIX="+t.TempDir(),
		"LOG_DIR="+t.TempDir(),
		"STATE_DIR="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	body := string(out)

	if err == nil {
		t.Fatalf("--dry-run reported success on a machine with no database.\n"+
			"This is the mode somebody runs to find out whether the machine is ready, "+
			"so answering yes here is worse than saying nothing:\n%s", body)
	}
	if !strings.Contains(body, "cannot reach PostgreSQL") {
		t.Errorf("--dry-run failed without saying why:\n%s", body)
	}
	// And it really did stop at the prerequisite rather than running the
	// whole dry run and failing somewhere later, which would be a
	// different bug wearing the same exit code.
	if strings.Contains(body, "== done") {
		t.Error("--dry-run reached the end after a prerequisite failed; the check has " +
			"to stop it, not decorate it")
	}
}

// TestTheNextStepsNameUnitsThatExist.
//
// # Why this test exists at all
//
// The next-steps list printed at the end of install.sh is the last thing
// a person reads before they start typing, and a command in it that does
// not work is worse than no list: it costs the reader the time to run it,
// the time to disbelieve it, and their trust in the rest of the page.
//
// It happened in the first draft of that list, twice in three lines.
// crucible-api does not exist - the unit is crucible-analytics-api - and
// the timer is crucible-upgrader.timer, not crucible-upgrade.timer. Both
// were caught by looking at release/systemd/, which is exactly the
// looking a test can do every time instead of once.
//
// # Derived, not listed
//
// One side is the directory. The other is whatever unit names the script
// happens to print. Neither is a hand-written list, so a unit renamed or
// added cannot leave this behind.
func TestTheNextStepsNameUnitsThatExist(t *testing.T) {
	root := repoRoot(t)

	entries, err := os.ReadDir(filepath.Join(root, "release", "systemd"))
	if err != nil {
		t.Fatal(err)
	}
	units := map[string]bool{}
	for _, e := range entries {
		units[e.Name()] = true
	}
	if len(units) < 4 {
		t.Fatalf("found %d unit files; this repository ships more, so this test is "+
			"comparing against the wrong directory", len(units))
	}

	script, err := os.ReadFile(filepath.Join(root, "release", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}

	// Every crucible-* name the script mentions with a unit suffix. The
	// suffix is what makes this unambiguous: the script also writes the
	// binaries' own names, and those are not units.
	named := regexp.MustCompile(`crucible-[a-z-]+\.(service|timer)`)
	found := named.FindAllString(string(script), -1)
	if len(found) == 0 {
		t.Fatal("install.sh names no systemd units at all; either the next-steps list " +
			"lost them or this pattern has stopped matching how they are written")
	}

	seen := map[string]bool{}
	for _, name := range found {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !units[name] {
			t.Errorf("install.sh tells the operator to run %s and release/systemd/ has "+
				"no such file.\nThe units that exist: %s",
				name, strings.Join(sortedUnitNames(units), " "))
		}
	}
}

func sortedUnitNames(units map[string]bool) []string {
	out := make([]string, 0, len(units))
	for name := range units {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestTheContainerSetupWritesEveryKeyTheExampleDocuments.
//
// # The two-way mirror, and the failure it is for
//
// docker/setup.sh writes docker/.env, and docker/.env.example is where
// every one of those values is explained. Two files, one list, and the
// list grows: a value added to the compose file gets a block in the
// example, and the script that writes the file for people is exactly
// where somebody forgets to add it.
//
// The failure is quiet in the way that costs the most. compose reads a
// missing variable as empty, and an empty SITE_BACKEND is a collector
// proxying to nothing - a stack that comes up, stays up, and serves
// errors.
//
// One side is the example, read for its keys. The other is what the
// script actually produces, run for real. Neither is written by hand
// here, so a key added to one and not the other cannot pass.
func TestTheContainerSetupWritesEveryKeyTheExampleDocuments(t *testing.T) {
	root := repoRoot(t)
	env := filepath.Join(t.TempDir(), ".env")

	cmd := exec.Command("./docker/setup.sh",
		"--site", "mirror-test", "--backend", "site:443", "--image", "ca:test")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CA_ENV_FILE="+env)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup.sh failed: %v\n%s", err, out)
	}

	written := envKeys(t, env)
	// Ranging the map's keys, not its values. Written the other way
	// round first, which produced a failure naming "site:443" as a key
	// the script had not written - wrong, but loudly wrong, which is the
	// only kind of wrong worth having in a test.
	for key := range envKeys(t, filepath.Join(root, "docker", ".env.example")) {
		if _, ok := written[key]; !ok {
			t.Errorf("docker/.env.example documents %s and setup.sh does not write it.\n"+
				"compose reads a missing variable as empty, which is a stack that comes "+
				"up and serves errors", key)
			continue
		}
		if written[key] == "" {
			t.Errorf("setup.sh wrote %s with no value", key)
		}
	}

	// The password is generated rather than defaulted, and long enough
	// to be worth generating. Asserted because the whole reason it is
	// not asked for is that a generated one is better than a typed one.
	if len(written["POSTGRES_PASSWORD"]) < 32 {
		t.Errorf("POSTGRES_PASSWORD is %d characters; that is not a generated password",
			len(written["POSTGRES_PASSWORD"]))
	}
	if written["SITE_ID"] != "mirror-test" {
		t.Errorf("SITE_ID = %q, want the value passed on the command line",
			written["SITE_ID"])
	}
}

// TestTheContainerSetupRefusesToOverwrite.
//
// The file holds the database superuser's password. A second run that
// generated a new one would leave a stack whose database no longer
// accepts its own services, and nothing would say so until the next
// restart - which might be weeks later, and would look like the database
// had broken on its own.
func TestTheContainerSetupRefusesToOverwrite(t *testing.T) {
	root := repoRoot(t)
	env := filepath.Join(t.TempDir(), ".env")

	run := func() (string, error) {
		cmd := exec.Command("./docker/setup.sh", "--site", "twice", "--backend", "site:443")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CA_ENV_FILE="+env)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run(); err != nil {
		t.Fatalf("the first run failed: %v\n%s", err, out)
	}
	first := envKeys(t, env)

	out, err := run()
	if err == nil {
		t.Fatalf("the second run overwrote an existing .env:\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	if second := envKeys(t, env); second["POSTGRES_PASSWORD"] != first["POSTGRES_PASSWORD"] {
		t.Error("the password changed despite the refusal; the file was written before " +
			"the check, which is the failure the check exists for")
	}
}

// envKeys reads a KEY=VALUE file, ignoring comments and blank lines.
func envKeys(t *testing.T, path string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		t.Fatalf("%s carries no KEY=VALUE lines at all", path)
	}
	return out
}

// TestTheInstallerOffersTheProfilesThePackageDefines.
//
// # Two lists, one set
//
// internal/profile defines the resource profiles and what each costs.
// install.sh accepts them by id in a shell case statement, which is a
// second copy of that list in a language that cannot import the first.
//
// The drift is silent in both directions and both are bad. A profile
// added to the package and not to the script is one nobody can install;
// an id the script accepts that the package does not know is a
// configuration the collector will refuse to name at startup, after the
// database is already built.
//
// One side is read out of the Go source, the other out of the shell.
// Neither is written here.
func TestTheInstallerOffersTheProfilesThePackageDefines(t *testing.T) {
	root := repoRoot(t)

	// The ids as internal/profile declares them: ID: "hafif", ...
	source := readFile(t, root, filepath.Join("internal", "profile", "profile.go"))
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`ID:\s*"([a-z]+)"`).FindAllStringSubmatch(source, -1) {
		declared[m[1]] = true
	}
	if len(declared) < 2 {
		t.Fatalf("found %d profile ids in internal/profile; the pattern has stopped "+
			"matching how they are declared", len(declared))
	}

	// The ids install.sh accepts, out of the case arm that validates
	// --profile. Anchored on the empty alternative so this reads the
	// validation rather than any other case statement in the file.
	script := readFile(t, root, filepath.Join("release", "install.sh"))
	arm := regexp.MustCompile(`\n  ""\|([a-z|]+)\) ;;`).FindStringSubmatch(script)
	if arm == nil {
		t.Fatal("could not find the --profile validation in install.sh; either it " +
			"changed shape or the flag stopped being validated, and an unvalidated " +
			"profile reaches the config files as whatever was typed")
	}
	accepted := map[string]bool{}
	for _, id := range strings.Split(arm[1], "|") {
		accepted[id] = true
	}

	for id := range declared {
		if !accepted[id] {
			t.Errorf("internal/profile offers %q and install.sh will not accept it, "+
				"so it is a profile nobody can install", id)
		}
	}
	for id := range accepted {
		if !declared[id] {
			t.Errorf("install.sh accepts --profile %q and internal/profile has no such "+
				"profile; the collector would refuse to name it at startup, after the "+
				"database was already built", id)
		}
	}
}

// TestEachProfileWritesTheConfigurationItNames.
//
// # Why this loads the file with the collector's own loader
//
// The id list is mirrored above, and that catches a profile nobody can
// select. It does not catch the worse case: an id the script accepts and
// writes wrongly. Swapping one boolean makes --profile dengeli produce
// the Tam configuration - twice the memory, silently, on the machine
// somebody chose the smaller profile for.
//
// So this asserts the loop rather than the file: the installer writes,
// internal/collector reads, and the level it derives has to be the level
// internal/profile declares for that id. Nothing here says "country_only
// should be true"; that fact lives in one place and is read from it.
//
// Found by a mutation - inverting dengeli's country_only broke no test.
func TestEachProfileWritesTheConfigurationItNames(t *testing.T) {
	for _, p := range profile.All() {
		t.Run(p.ID, func(t *testing.T) {
			db := "ca_profile_" + p.ID
			scratchDatabase(t, db)

			conf := t.TempDir()
			out, ok := runInstall(t, db, "CONF_DIR="+conf, "PROFILE="+p.ID)
			if !ok {
				t.Fatalf("the install failed:\n%s", out)
			}

			cfg, err := collector.Load(filepath.Join(conf, "collector.toml"))
			if err != nil {
				t.Fatalf("the collector cannot load the config this profile wrote: %v", err)
			}
			if got := cfg.ProfileLevel(); got != p.Level {
				t.Errorf("--profile %s produced level %q, and internal/profile says that "+
					"profile is level %q.\nThe installer and the package disagree about "+
					"what this profile is, which is memory the operator did not choose",
					p.ID, got, p.Level)
			}

			// The beacon has to agree, and that is not decoration: a
			// beacon loading the ASN datasets its collector no longer
			// fills pays the full 136 MB for a column nobody writes.
			beacon := readFileAt(t, filepath.Join(conf, "beacon.toml"))
			wantEnabled := p.Level != "kapali"
			if got := strings.Contains(beacon, "enabled = true"); got != wantEnabled {
				t.Errorf("beacon.toml has asn_lookup enabled=%v, want %v - the two "+
					"writers have to load the same datasets", got, wantEnabled)
			}
		})
	}
}

// readFileAt reads a file by absolute path.
func readFileAt(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestEveryUnitRunsSomethingTheInstallerPutsThere.
//
// # The gap this was written to prove
//
// Every unit in release/systemd runs /opt/crucible-analytic/bin/<name>.
// install.sh creates the roles, the database, the schema, the config
// files, the service accounts, the directories and the units themselves
// - and puts nothing in that directory. It only quotes those paths back
// in its closing instructions.
//
// So an operator who follows KURULUM.md exactly reaches `systemctl
// enable --now crucible-collector` and gets status=203/EXEC on all four
// services, with the only clue being a path inside a unit file they were
// never told to read. The guide's build section says "copy the bin/
// directory" and does not say where to.
//
// # Why a mirror rather than a checklist
//
// The destination is written in five places already - one per unit - and
// the installer is the sixth. Asserting the sixth agrees with the five
// costs nothing and cannot go stale, because a new service arrives here
// with its own ExecStart.
func TestEveryUnitRunsSomethingTheInstallerPutsThere(t *testing.T) {
	root := repoRoot(t)

	entries, err := os.ReadDir(filepath.Join(root, "release", "systemd"))
	if err != nil {
		t.Fatal(err)
	}

	execStart := regexp.MustCompile(`(?m)^ExecStart=(\S+)`)
	want := map[string]string{} // binary path -> the unit that runs it
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".service") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, "release", "systemd", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		m := execStart.FindSubmatch(body)
		if m == nil {
			t.Errorf("%s has no ExecStart, so systemd has nothing to run", e.Name())
			continue
		}
		want[string(m[1])] = e.Name()
	}
	if len(want) < 4 {
		t.Fatalf("read %d ExecStart paths; this repository ships more services, so "+
			"this test is looking at the wrong place", len(want))
	}

	script, err := os.ReadFile(filepath.Join(root, "release", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)

	for path, unit := range want {
		name := filepath.Base(path)
		dir := filepath.Dir(path) // /opt/crucible-analytic/bin

		// The installer has to do two things: know the directory, and
		// know this binary belongs in it. Checking only the directory
		// would pass on a script that installs four of the five.
		installsDir := strings.Contains(text, `${PREFIX}/bin`) ||
			strings.Contains(text, dir)
		if !installsDir {
			t.Errorf("%s runs %s and install.sh never writes into %s.\n"+
				"Every service starts with status=203/EXEC and nothing said the "+
				"binaries had to be put there by hand", unit, path, dir)
			continue
		}
		if !strings.Contains(text, "install_binaries") && !strings.Contains(text, name) {
			t.Errorf("%s runs %s and install.sh never names %q, so that one binary "+
				"is the one nobody copies", unit, path, name)
		}
	}

	// And then it is run, because reading the script is not the claim.
	// The claim is that a file arrives, executable, at the path the unit
	// names - and the first version of this test asserted a spelling of
	// the fix instead, which passed and failed for reasons that had
	// nothing to do with whether anything was installed.
	binDir := t.TempDir()
	prefix := t.TempDir()
	for name := range map[string]bool{"collector": true, "panel": true} {
		if err := os.WriteFile(filepath.Join(binDir, name),
			[]byte("#!/bin/sh\necho stub\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	const db = "ca_install_binaries_test"
	scratchDatabase(t, db)
	base := superuserDSN(t)

	cmd := exec.Command("./release/install.sh", "--no-systemd",
		"--bin-dir", binDir, "--prefix", prefix)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"SUPERUSER_DSN="+dsnFor(base, db),
		"DB_NAME="+db,
		"CONF_DIR="+t.TempDir(),
		"PREFIX="+prefix,
		"LOG_DIR="+t.TempDir(),
		"STATE_DIR="+t.TempDir(),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}

	for _, name := range []string{"collector", "panel"} {
		got := filepath.Join(prefix, "bin", name)
		info, err := os.Stat(got)
		if err != nil {
			t.Errorf("install.sh finished and %s is not there: %v.\n"+
				"Every unit runs a binary out of this directory, so systemd answers "+
				"status=203/EXEC on all of them", got, err)
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is mode %v, which systemd cannot execute", got, info.Mode().Perm())
		}
	}
}

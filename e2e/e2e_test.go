//go:build e2e

// The whole product, once, from a tarball.
//
// Every other test in this repository checks a part. The integration
// suites run one package against a real database; release/ checks that
// install.sh produces the privilege matrix it claims. None of them has
// ever answered the question a customer asks first:
//
//	does a real request end up as a number on the dashboard?
//
// That was halves - the collector writes rows, the beacon writes its
// own, and separately rows come back through the API to the panel - and
// halves that each pass are not a chain that works. This runs the chain,
// both sides of it: a proxied HTTPS request and a pageview from the
// snippet, ending at six dashboard cards that all have to carry a
// number.
//
//	go test -tags e2e ./e2e/ -v -timeout 15m
//
// It needs a superuser connection (CA_SUPERUSER_DSN) and it builds the
// release package, so it is slow and lives behind its own tag. It is
// deliberately not in the merge gate: a test that takes minutes and
// needs a database is a test people learn to skip when it is in the way.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// site is what the collector stamps on every row and what the API
	// exposes the numbers under.
	site = "e2e-site"
	// rolePassword is set on all four roles, and this is the one place
	// the test steps outside what an operator does.
	//
	// install.sh now generates the four passwords and writes them into
	// the configuration files itself - which it did not do until this
	// test went looking, and the absence of it is why an unattended
	// install produced four configs that could not connect. But the four
	// roles are cluster-wide, so on a machine that already has them (a
	// development cluster, this one) nothing is generated and the
	// example placeholders stay. Setting known passwords here covers
	// both cases without the test depending on the wording of a message.
	rolePassword = "e2e-role-password"
)

func superuserDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CA_SUPERUSER_DSN")
	if dsn == "" {
		t.Skip("set CA_SUPERUSER_DSN to a superuser connection")
	}
	return dsn
}

// TestARequestBecomesANumberOnTheDashboard is the chain.
func TestARequestBecomesANumberOnTheDashboard(t *testing.T) {
	root := repoRoot(t)
	dsn := superuserDSN(t)
	ctx := context.Background()

	pkg := buildAndUnpack(t, root)
	db := scratchDatabase(t, dsn)
	install(t, pkg, dsnFor(dsn, db), db)

	token := writeConfigs(t, pkg, db)

	// The site the collector fronts. A TLS server, because the collector
	// runs in passthrough mode: it reads the ClientHello for the
	// fingerprint and forwards the bytes without decrypting them, so
	// there has to be something on the other end that can finish the
	// handshake.
	origin := startOrigin(t)
	setConfig(t, pkg, "collector.toml", `^backend_addr = ".*"$`,
		fmt.Sprintf(`backend_addr = %q`, origin.addr))

	// High ports, so a run of this test cannot collide with anything the
	// developer already has listening. The shipped defaults no longer
	// collide with each other - they did, both on 127.0.0.1:8080, until
	// this test put the whole package on one machine and the collector
	// proxied every visitor to the analytics API. release/ports_test.go
	// now holds them apart.
	collectorAddr := "127.0.0.1:18443"
	beaconAddr := "127.0.0.1:18081"
	apiAddr := "127.0.0.1:18080"
	panelAddr := "127.0.0.1:18090"
	setConfig(t, pkg, "collector.toml", `^listen_addr = ".*"$`, fmt.Sprintf(`listen_addr = %q`, collectorAddr))
	setConfig(t, pkg, "beacon.toml", `^listen_addr = ".*"$`, fmt.Sprintf(`listen_addr = %q`, beaconAddr))
	setConfig(t, pkg, "analytics-api.toml", `^listen_addr = ".*"$`, fmt.Sprintf(`listen_addr = %q`, apiAddr))
	setConfig(t, pkg, "panel.toml", `^listen_addr = ".*"$`, fmt.Sprintf(`listen_addr = %q`, panelAddr))
	setConfig(t, pkg, "panel.toml", `^analytics_api_url = ".*"$`,
		fmt.Sprintf(`analytics_api_url = "http://%s"`, apiAddr))

	// The one setting this test changes that a customer would not.
	//
	// A real panel is behind TLS and the session cookie is Secure, which
	// is the default and stays the default. This test drives it over
	// plain HTTP on loopback, and a Secure cookie is one a client will
	// not send back - so the first run signed in successfully and then
	// arrived at the dashboard as a stranger, was handed the login form,
	// and reported "the dashboard shows no figure". The example file
	// documents this switch for exactly this case.
	setConfig(t, pkg, "panel.toml", `^# secure_cookies = true$`, `secure_cookies = false`)

	start(t, pkg, "collector", "collector.toml")
	start(t, pkg, "beacon", "beacon.toml")
	start(t, pkg, "analytics-api", "analytics-api.toml")
	start(t, pkg, "panel", "panel.toml")

	waitListening(t, collectorAddr)
	waitListening(t, beaconAddr)
	waitListening(t, apiAddr)
	waitListening(t, panelAddr)

	// ---- a real request, through the proxy, to the real origin ----
	body := throughProxy(t, collectorAddr, origin)
	if !strings.Contains(body, "origin-served-this") {
		t.Fatalf("the proxy did not return the origin's body: %q", body)
	}

	// ---- it reaches the database ----
	pool, err := pgxpool.New(ctx, dsnFor(dsn, db))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var rows int
	deadline := time.Now().Add(flushWait)
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM traffic_snapshots WHERE site_id = $1`, site).Scan(&rows); err == nil && rows > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if rows == 0 {
		t.Fatalf("no row reached traffic_snapshots within %s; the request was proxied but never recorded", flushWait)
	}
	t.Logf("the collector wrote %d row(s)", rows)

	// A fingerprint was taken. Passthrough mode's entire reason for
	// existing: without it the row is a hit counter.
	var ja4 string
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(ja4, '') FROM traffic_snapshots WHERE site_id = $1 ORDER BY time DESC LIMIT 1`,
		site).Scan(&ja4); err != nil {
		t.Fatal(err)
	}
	if ja4 == "" {
		t.Error("the row carries no JA4; the ClientHello was proxied without being read")
	} else {
		t.Logf("JA4 = %s", ja4)
	}

	// ---- and the collector bounded how long it will be kept ----
	//
	// Here rather than in the retention package's own suite, because
	// the thing that was broken is invisible from there. That suite
	// connects as `collector` to a development database `collector`
	// created, where it owns the hypertables; on an installed
	// deployment they belong to the superuser and TimescaleDB checks
	// ownership, not privilege. Every retention test passed for years
	// against the one arrangement in which the feature works.
	var policyDays string
	if err := pool.QueryRow(ctx, `
		SELECT coalesce((config->>'drop_after'), '')
		FROM timescaledb_information.jobs
		WHERE proc_name = 'policy_retention' AND hypertable_name = 'traffic_snapshots'`,
	).Scan(&policyDays); err != nil {
		t.Errorf("no retention policy on traffic_snapshots after the collector started: %v", err)
		t.Error("the table will grow until the disk fills, on the machine that also serves the site")
	} else {
		t.Logf("retention on traffic_snapshots: %s", policyDays)
	}

	// ---- and the other half of the product: a pageview ----
	//
	// The beacon is a separate process, a separate role and a separate
	// table, and the four cards a customer looks at first come from it.
	// Without this the dashboard renders "no snippet installed" four
	// times and the chain is only half proved - which is exactly the
	// shape of half-proof this whole test was written against.
	sendPageview(t, beaconAddr, site)

	var events int
	deadline = time.Now().Add(flushWait)
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM beacon_events WHERE site_id = $1`, site).Scan(&events); err == nil && events > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if events == 0 {
		t.Fatalf("no row reached beacon_events within %s; the beacon accepted the event and never wrote it", flushWait)
	}
	t.Logf("the beacon wrote %d row(s)", events)

	// ---- the read API can see it ----
	//
	// Under a different database role, over HTTP, with a token whose
	// hash lives in the other config file. Everything between the write
	// and this read is exercised: the grant matrix, the token's two
	// halves, and the API's own query.
	summary := apiSummary(t, apiAddr, token)
	if summary.Snapshots <= 0 {
		t.Fatalf("the read API reports %d snapshots for %s", summary.Snapshots, site)
	}
	if summary.UniqueIPs <= 0 {
		t.Errorf("the read API reports %d unique addresses; the rows carry no client address", summary.UniqueIPs)
	}
	t.Logf("the API reports %d snapshot(s), %d address(es)", summary.Snapshots, summary.UniqueIPs)

	// ---- and the panel draws it ----
	//
	// Signed in, through the panel's own dashboard, exactly as somebody
	// reaches it on a fresh installation: a one-time link minted on the
	// server. The number is checked rather than the page merely
	// rendering, because a dashboard that answers 200 with every figure
	// blank is the failure this whole chain exists to catch.
	page := panelDashboard(t, pkg, panelAddr)
	if !strings.Contains(page, site) {
		t.Fatalf("the dashboard does not mention %s", site)
	}
	// Every card, not just one. Both halves of the product wrote a row
	// by now - the collector's proxied request and the beacon's
	// pageview - so a card still saying "no measurement has arrived" is
	// a real gap rather than a deployment nobody has finished.
	//
	// The weaker "some card has a number" version passed while all four
	// beacon cards read "the snippet is not installed", which is what a
	// customer sees first and was the half this test did not cover.
	for _, c := range cards(page) {
		if !c.filled {
			t.Errorf("a dashboard card has nothing in it: %s = %s", c.title, c.value)
		}
	}
	if !hasANumber(page) {
		t.Errorf("the dashboard rendered but shows no figure; the panel reached the API and got nothing")
	}
	// Logged either way. A card that is empty says *why* it is empty -
	// never installed, nothing in range, unreachable, refused - and that
	// sentence is the whole diagnosis when this assertion fails. Reading
	// it out of the failure beats going back and adding a print.
	if lines := cardLines(page); len(lines) > 0 {
		for _, line := range lines {
			t.Log("card: " + line)
		}
	} else if t.Failed() {
		// No cards at all is a different failure from cards with nothing
		// in them, and the page itself is the only thing that can say
		// which. Printed only when something already went wrong.
		t.Logf("--- the page the panel served ---\n%s", page)
	}
	if !t.Failed() {
		t.Log("the request reached the dashboard")
	}
}

// ---------------------------------------------------------------- steps

// buildAndUnpack builds the release tarball and unpacks it somewhere
// new, so what runs is what a customer downloads.
func buildAndUnpack(t *testing.T, root string) string {
	t.Helper()

	cmd := exec.Command("./release/build.sh")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build.sh failed: %v\n%s", err, out)
	}

	// The path build.sh printed, not the newest thing matching a glob.
	//
	// The glob version ran the whole chain against a stale tarball and
	// reported the bug it had just been used to fix: dist/ keeps every
	// package ever built, the version string carries -dirty in a working
	// tree, and "crucible-analytic-58e2acd-linux" sorts after
	// "crucible-analytic-58e2acd-dirty-linux". Twenty minutes of reading
	// a fixed collector's old log.
	tarball := lastLine(string(out))
	if !strings.HasSuffix(tarball, ".tar.gz") {
		t.Fatalf("build.sh did not end by naming its package:\n%s", out)
	}
	if !filepath.IsAbs(tarball) {
		tarball = filepath.Join(root, tarball)
	}

	dir := t.TempDir()
	untar := exec.Command("tar", "xzf", tarball, "-C", dir)
	if out, err := untar.CombinedOutput(); err != nil {
		t.Fatalf("unpacking %s: %v\n%s", tarball, err, out)
	}
	inner, err := filepath.Glob(filepath.Join(dir, "crucible-analytic-*"))
	if err != nil || len(inner) != 1 {
		t.Fatalf("expected one directory in the tarball, got %v", inner)
	}
	t.Logf("unpacked %s", filepath.Base(tarball))
	return inner[0]
}

func scratchDatabase(t *testing.T, dsn string) string {
	t.Helper()
	name := fmt.Sprintf("ca_e2e_%d", time.Now().UnixNano()%1_000_000)

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsnFor(dsn, "postgres"))
	if err != nil {
		t.Skipf("cannot reach the superuser connection: %v", err)
	}
	t.Cleanup(admin.Close)

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return name
}

// install runs the package's own install.sh, from inside the package.
func install(t *testing.T, pkg, dsn, db string) {
	t.Helper()

	// --no-systemd for the same reason the CI workflow passes it: a runner
	// is not a machine anybody deploys to, there is no service to start,
	// and install.sh refuses outright rather than write unit files it
	// cannot enable. Without it every nightly ended at the preflight -
	//
	//	install: systemd units need root, and this is running as runner.
	//
	// - which is install.sh being right, and this helper never asking for
	// the install it actually wanted. The gate's own integration job had
	// the flag from the start; this path was written later and did not.
	cmd := exec.Command("./release/install.sh", "--no-systemd")
	cmd.Dir = pkg
	cmd.Env = append(os.Environ(),
		"SUPERUSER_DSN="+dsn,
		"DB_NAME="+db,
		"CONF_DIR="+filepath.Join(pkg, "conf"),
		"PREFIX="+filepath.Join(pkg, "opt"),
		"LOG_DIR="+filepath.Join(pkg, "log"),
		"STATE_DIR="+filepath.Join(pkg, "state"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("every assertion holds")) {
		t.Fatalf("the install did not verify its privilege matrix:\n%s", out)
	}
}

// writeConfigs does the operator's part: four database passwords, a
// site id, an API token and the beacon's allowlist, pasted into four
// files.
//
// Every line here is a hand edit a real operator makes, and the number
// of them is worth looking at. The token is the sharpest: it goes in one
// file and its SHA-256 in another, which is the same shape as the IP key
// that install.sh grew a whole verification step for.
func writeConfigs(t *testing.T, pkg, db string) string {
	t.Helper()

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsnFor(superuserDSN(t), db))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, role := range []string{"collector", "beacon_writer", "analytics_reader", "panel_user"} {
		if _, err := admin.Exec(ctx,
			fmt.Sprintf("ALTER ROLE %s PASSWORD '%s'", role, rolePassword)); err != nil {
			t.Fatalf("setting %s's password: %v", role, err)
		}
	}

	host := hostOf(t, superuserDSN(t))
	dsnFmt := func(role string) string {
		return fmt.Sprintf(`postgres://%s:%s@%s/%s`, role, rolePassword, host, db)
	}

	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := "e2e-" + hex.EncodeToString(raw[:])
	digest := sha256.Sum256([]byte(token))

	setConfig(t, pkg, "collector.toml", `^timescale_dsn = ".*"$`,
		fmt.Sprintf(`timescale_dsn = %q`, dsnFmt("collector")))
	setConfig(t, pkg, "collector.toml", `^site_id = ".*"$`, fmt.Sprintf(`site_id = %q`, site))
	setConfig(t, pkg, "analytics-api.toml", `^timescale_dsn = ".*"$`,
		fmt.Sprintf(`timescale_dsn = %q`, dsnFmt("analytics_reader")))
	setConfig(t, pkg, "analytics-api.toml", `^sha256 = ".*"$`,
		fmt.Sprintf(`sha256 = %q`, hex.EncodeToString(digest[:])))
	setConfig(t, pkg, "panel.toml", `^panel_dsn = ".*"$`,
		fmt.Sprintf(`panel_dsn = %q`, dsnFmt("panel_user")))
	setConfig(t, pkg, "panel.toml", `^analytics_api_token = ".*"$`,
		fmt.Sprintf(`analytics_api_token = %q`, token))

	// The beacon. Its site list is an allowlist rather than a
	// credential - the snippet is public, so the site in a POST body is
	// a claim - and a deployment that forgets this line has a beacon
	// that answers every request and stores nothing.
	setConfig(t, pkg, "beacon.toml", `^timescale_dsn = ".*"$`,
		fmt.Sprintf(`timescale_dsn = %q`, dsnFmt("beacon_writer")))
	setConfig(t, pkg, "beacon.toml", `^sites = \[.*\]$`, fmt.Sprintf(`sites = [%q]`, site))

	return token
}

// setConfig rewrites one line of a configuration file.
func setConfig(t *testing.T, pkg, name, pattern, replacement string) {
	t.Helper()
	path := filepath.Join(pkg, "conf", name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?m)" + pattern)
	found := re.FindAll(body, -1)
	switch len(found) {
	case 0:
		t.Fatalf("%s: nothing matched %s", name, pattern)
	case 1:
	default:
		// The comment here used to say "once" while the code below
		// replaced every match, which is the kind of claim this project
		// keeps catching itself making. A pattern matching twice would
		// rewrite a commented-out example or a second section as well,
		// and the config would then be wrong in a way that shows up
		// only as a service refusing to start.
		t.Fatalf("%s: %s matched %d times; a config edit that hits more than one line is not an edit",
			name, pattern, len(found))
	}
	replaced := re.ReplaceAll(body, []byte(replacement))
	if err := os.WriteFile(path, replaced, 0o600); err != nil {
		t.Fatal(err)
	}
}

// start runs one of the packaged binaries until the test ends.
func start(t *testing.T, pkg, binary, config string) {
	t.Helper()

	cmd := exec.Command(filepath.Join(pkg, "bin", binary), "-config", filepath.Join(pkg, "conf", config))
	cmd.Dir = pkg
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", binary, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		// Printed only on failure. A passing run that dumped four
		// services' logs would bury the one line that mattered.
		if t.Failed() {
			t.Logf("--- %s output ---\n%s", binary, out.String())
		}
	})
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("nothing is listening on %s after twenty seconds", addr)
}

// ---------------------------------------------------------------- origin

// --------------------------------------------------------------- beacon

// ------------------------------------------------------------------ api

// summary is the part of the API's answer this test asserts on.
//
// Deliberately not the whole struct: a test that decoded every field
// would fail the day a field is added, which is not a fact about the
// chain. Snapshots is how many rows backed the answer, and unique_ips
// is how many addresses were behind them - the first proves the read
// reached the rows, the second that the rows carry a client.
type summary struct {
	Snapshots int `json:"snapshots"`
	UniqueIPs int `json:"unique_ips"`
}

// apiSummary asks the read API what it can see for the site.
func apiSummary(t *testing.T, apiAddr, token string) summary {
	t.Helper()

	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	url := fmt.Sprintf("http://%s/api/v1/sites/%s/summary?from=%s&to=%s", apiAddr, site, from, to)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("the read API did not answer: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the read API answered %d: %s", resp.StatusCode, body)
	}

	var out summary
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the API's answer does not parse: %v\n%s", err, body)
	}
	return out
}

// ---------------------------------------------------------------- panel

// panelDashboard redeems a one-time developer link and returns the
// site's dashboard.
//
// The developer link rather than an owner account, and that is the
// honest path for what this test installs: on a deployment with no
// accounts the panel mints an auto-approved link, which is exactly how
// the first person gets in. Inventing an owner by writing rows into
// panel_users would test a state the product never produces.
func panelDashboard(t *testing.T, pkg, panelAddr string) string {
	t.Helper()

	// Minted by the packaged binary, from the packaged config, the way
	// an installer does it - a second process against the same database.
	cmd := exec.Command(filepath.Join(pkg, "bin", "panel"),
		"-config", filepath.Join(pkg, "conf", "panel.toml"),
		"-dev-link", "-base-url", "http://"+panelAddr)
	cmd.Dir = pkg
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("minting a developer link: %v\n%s", err, out)
	}
	link := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if !strings.Contains(link, DevAccessSegment) {
		t.Fatalf("the first line of -dev-link is not a link:\n%s", out)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}

	// Where redemption lands, not what the page says. A link that was
	// refused still answers 200 - with the login form - so a check on
	// the body would have to guess at wording, and the wording is
	// Turkish prose somebody will improve.
	landed, _, _ := getPage(t, client, link)
	if !strings.Contains(landed, SetupPathPrefix) {
		t.Fatalf("redeeming the developer link landed on %s, not the wizard; the session was not created", landed)
	}
	return dashboard(t, client, panelAddr, site)
}

// ---------------------------------------------------------------- utils

func dsnFor(base, db string) string {
	if i := strings.LastIndex(base, "/"); i >= 0 {
		if q := strings.Index(base[i:], "?"); q >= 0 {
			return base[:i+1] + db + base[i+q:]
		}
		return base[:i+1] + db
	}
	return base
}

// hostOf pulls host:port out of a DSN, for building the service DSNs.
func hostOf(t *testing.T, dsn string) string {
	t.Helper()
	rest := dsn
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.Index(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		t.Fatalf("cannot find a host in %q", dsn)
	}
	return rest
}

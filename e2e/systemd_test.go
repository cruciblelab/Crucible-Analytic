//go:build systemd

// The panel's version update, under the units install.sh writes, on a
// real systemd.
//
//	go test -c -tags systemd -o systemd.test ./e2e/
//	sudo CA_SYSTEMD_TEST=1 CA_SUPERUSER_DSN=... CA_SYSTEMD_DIST=dist \
//	     CA_SYSTEMD_PUBKEY=... ./systemd.test -test.v
//
// # Why this exists
//
// Every other suite runs the product's binaries outside systemd. The
// tarball suite passes --no-systemd; the release suite writes the unit
// files into a scratch directory and asks systemd-analyze whether they
// parse. Nothing had ever started one of them, so the sandboxes the
// units declare - ProtectSystem=strict with a short ReadWritePaths list,
// PrivateTmp, every capability dropped - had never been in force while
// the code they confine was running.
//
// That is how the panel's update button shipped without ever working
// on a systemd install. The upgrader writes the new binaries into
// /opt/crucible-analytic/bin, which install.sh leaves root's and which
// the upgrader's unit makes read-only; and it rings the restart
// doorbell in /run/crucible-analytic, which that unit never listed as
// writable either - only the unit that deletes the file did. Measured
// first with the real upgrader in a mount namespace reproducing the
// sandbox (NOTES, "V systemd altında"), and asked here of systemd
// itself.
//
// # What it needs, and why it refuses rather than skips
//
// Root and systemd as PID 1: it runs install.sh without --no-systemd,
// so it writes /opt, /etc and /etc/systemd/system, creates two system
// accounts and starts units. A runner VM, never a developer's machine:
// this suite changes the machine it runs on and does not put it back.
//
// Anywhere else it fails. The one job that runs it exists for nothing
// else, and a skip there would be a green job that measured nothing -
// the shape this repository keeps finding in its own CI.
package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/proxy"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
)

const (
	// The two versions the workflow builds and signs. Only the stamp
	// differs, which is the point: what is measured is the path from one
	// to the other, not anything either version does.
	fromVersion = "v0.98.0"
	toVersion   = "v0.99.0"

	// Where install.sh puts things when nothing overrides it, which is
	// what the unit files name. Written out rather than taken from the
	// script: the units hard-code them, and this suite is about the units.
	installPrefix = "/opt/crucible-analytic"
	installConf   = "/etc/crucible-analytic"
	doorbellDir   = "/run/crucible-analytic"
	unitDir       = "/etc/systemd/system"

	// The headings KURULUM.md gives the two opt-ins under, and whose
	// first shell blocks this suite runs as written. internal/docs holds
	// the document to them on every push, so a renamed heading fails
	// there rather than only here, at night.
	panelUpdatesHeading = "### İsteğe bağlı: panelden güncellemeyi açın"
	restarterHeading    = "### İsteğe bağlı: yeniden başlatmayı da devredin"
)

// services are the four units the restarter restarts, with the role each
// one's heartbeat is written under. relupdate.HealthServices is the
// role side; the unit side is spelled here because a test reading it
// out of restart.sh would agree with restart.sh by construction.
var services = []struct{ unit, role string }{
	{"crucible-collector.service", "collector"},
	{"crucible-beacon.service", "beacon_writer"},
	{"crucible-analytics-api.service", "analytics_reader"},
	{"crucible-panel.service", "panel_user"},
}

// systemdEnv is what the workflow hands the suite.
type systemdEnv struct {
	dist   string // the directory holding both signed packages
	pubkey string // the public half of the key that signed them
}

// requireSystemd refuses to run anywhere but a machine this suite may
// change.
func requireSystemd(t *testing.T) systemdEnv {
	t.Helper()
	var missing []string
	if os.Getenv("CA_SYSTEMD_TEST") != "1" {
		missing = append(missing, "CA_SYSTEMD_TEST=1 (this suite installs into /opt and /etc and starts units)")
	}
	if os.Geteuid() != 0 {
		missing = append(missing, "root")
	}
	if comm, err := os.ReadFile("/proc/1/comm"); err != nil || strings.TrimSpace(string(comm)) != "systemd" {
		missing = append(missing, fmt.Sprintf("systemd as PID 1 (PID 1 is %q)", strings.TrimSpace(string(comm))))
	}
	env := systemdEnv{dist: os.Getenv("CA_SYSTEMD_DIST"), pubkey: os.Getenv("CA_SYSTEMD_PUBKEY")}
	if env.dist == "" {
		missing = append(missing, "CA_SYSTEMD_DIST (the directory with both signed packages)")
	}
	if env.pubkey == "" {
		missing = append(missing, "CA_SYSTEMD_PUBKEY (the key that signed them)")
	}
	if os.Getenv("CA_SUPERUSER_DSN") == "" {
		missing = append(missing, "CA_SUPERUSER_DSN")
	}
	if len(missing) > 0 {
		t.Fatalf("this suite runs only where it may install the product for real, and here it "+
			"is missing:\n  - %s\nIt fails rather than skips: the job that runs it exists for "+
			"nothing else.", strings.Join(missing, "\n  - "))
	}
	return env
}

func TestAPanelUpdateFinishesUnderTheRealUnits(t *testing.T) {
	env := requireSystemd(t)
	ctx := context.Background()
	dsn := superuserDSN(t)

	// ---- the older version, installed the way KURULUM.md says ----
	pkg := unpackPackage(t, env.dist, fromVersion)
	db := scratchDatabase(t, dsn)
	installForReal(t, pkg, dsnFor(dsn, db), db)

	// writeConfigs edits <pkg>/conf, which on this install is the
	// system's configuration directory. A link rather than a second
	// helper: the operator's edits are the same edits wherever the files
	// live, and two copies of them would be two answers.
	if err := os.Symlink(installConf, filepath.Join(pkg, "conf")); err != nil {
		t.Fatal(err)
	}
	writeConfigs(t, pkg, db)

	origin := startOrigin(t)
	setConfig(t, pkg, "collector.toml", `^backend_addr = ".*"$`,
		fmt.Sprintf(`backend_addr = %q`, origin.addr))
	setConfig(t, pkg, "collector.toml", `^listen_addr = ".*"$`, fmt.Sprintf(`listen_addr = %q`, collectorAddr))
	setConfig(t, pkg, "beacon.toml", `^listen_addr = ".*"$`, `listen_addr = "127.0.0.1:18081"`)
	setConfig(t, pkg, "analytics-api.toml", `^listen_addr = ".*"$`, `listen_addr = "127.0.0.1:18080"`)
	setConfig(t, pkg, "panel.toml", `^listen_addr = ".*"$`, `listen_addr = "127.0.0.1:18090"`)
	setConfig(t, pkg, "panel.toml", `^analytics_api_url = ".*"$`, `analytics_api_url = "http://127.0.0.1:18080"`)

	// The upgrader's database role. writeConfigs sets the four service
	// roles and leaves this one, because the tarball suite never runs
	// the upgrader.
	admin := superPool(t, dsnFor(dsn, db))
	if _, err := admin.Exec(ctx, fmt.Sprintf("ALTER ROLE schema_admin PASSWORD '%s'", rolePassword)); err != nil {
		t.Fatal(err)
	}
	setConfig(t, pkg, "upgrader.toml", `^schema_admin_dsn = ".*"$`,
		fmt.Sprintf(`schema_admin_dsn = "postgres://schema_admin:%s@%s/%s"`, rolePassword, hostOf(t, dsn), db))

	// ---- the four services, started by systemd ----
	//
	// "Newer than" is asked of the database's clock, which is the one
	// that writes beat_at - the same reason relupdate.Doorbell.Since
	// gives for not using this machine's.
	var started time.Time
	if err := admin.QueryRow(ctx, "SELECT now()").Scan(&started); err != nil {
		t.Fatal(err)
	}
	systemctl(t, "daemon-reload")
	args := []string{"enable", "--now"}
	for _, s := range services {
		args = append(args, s.unit)
	}
	systemctl(t, args...)
	t.Cleanup(func() { dumpUnits(t) })
	waitHeartbeats(t, admin, started, "after the first start")

	// ---- a release source the upgrader trusts ----
	base, downloads := serveRelease(t, env.dist, toVersion)
	body, err := os.ReadFile(filepath.Join(installConf, "upgrader.toml"))
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(fmt.Sprintf("\n[release]\nbase_url = %q\npublic_key = %q\n",
		base+"/rel", env.pubkey))...)
	// WriteFile keeps an existing file's owner and mode, which here are
	// the whole point: root:crucible-upgrader 0640, the one file the
	// panel's account must not read.
	if err := os.WriteFile(filepath.Join(installConf, "upgrader.toml"), body, 0o640); err != nil {
		t.Fatal(err)
	}
	panelPool := servicePool(t, dsn, db, "panel_user")

	// ---- first, without the opt-in: refused, and before the download ----
	//
	// The state every systemd install is in until the operator takes the
	// step. Measured before this was fixed (nightly run 41, this job):
	// the package was downloaded and verified, and the install failed at
	// its first write with "read-only file system".
	if _, err := relupdate.Ask(ctx, panelPool, relupdate.Actor{Kind: "user", Label: "systemd-suite"},
		"", fromVersion, toVersion); err != nil {
		t.Fatalf("queueing the update the way the panel does: %v", err)
	}
	systemctl(t, "start", "crucible-upgrader.service")
	refused, err := relupdate.Latest(ctx, admin)
	if err != nil || refused == nil {
		t.Fatalf("reading the refused request back: %v", err)
	}
	if refused.State != relupdate.StateFailed ||
		!strings.Contains(refused.ErrorChain, strings.TrimPrefix(panelUpdatesHeading, "### ")) {
		t.Fatalf("before the opt-in the update ended %s with %q; want a refusal that names "+
			"the KURULUM.md heading\n%s", refused.State, refused.ErrorChain,
			journal(t, "crucible-upgrader.service"))
	}
	if n := downloads.Load(); n != 0 {
		t.Errorf("the package was downloaded %d time(s) by a machine that could not install it", n)
	}

	// ---- the two opt-ins, by the commands KURULUM.md gives ----
	runDocumentedBlock(t, pkg, panelUpdatesHeading)
	runDocumentedBlock(t, pkg, restarterHeading)
	if info, err := os.Stat(doorbellDir); err != nil || !info.IsDir() {
		t.Fatalf("KURULUM's restarter commands ran and %s is not a directory (%v); the upgrader "+
			"reads that directory's existence as 'a restarter is listening'", doorbellDir, err)
	}

	// ---- nothing root runs is where the upgrader may now write ----
	//
	// Asked of the machine rather than the files: the opt-in has just
	// opened the binary directory, and the restarter runs as root.
	for _, dir := range []string{filepath.Join(installPrefix, "libexec"),
		filepath.Join(installPrefix, "libexec", "restart.sh")} {
		if err := exec.Command("runuser", "-u", "crucible-upgrader", "--", "test", "-w", dir).Run(); err == nil {
			t.Errorf("crucible-upgrader can write %s, and root runs the restarter from there", dir)
		}
	}
	if line := systemctlOut(t, "show", "--property=ExecStart", "crucible-restart.service"); !strings.Contains(line, "/libexec/restart.sh") {
		t.Errorf("crucible-restart.service does not run the libexec script: %s", line)
	}
	if _, err := os.Stat(filepath.Join(installPrefix, "bin", "restart.sh")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("restart.sh is in the binary directory the upgrader can now write (%v)", err)
	}

	// ---- the panel's request, through the panel's own call ----
	if _, err := relupdate.Ask(ctx, panelPool, relupdate.Actor{Kind: "user", Label: "systemd-suite"},
		"", fromVersion, toVersion); err != nil {
		t.Fatalf("queueing the update the way the panel does: %v", err)
	}

	before := invocations(t)

	// ---- a visitor, with the site open in a tab ----
	//
	// One request through the collector on a keep-alive connection, which
	// then stays open and idle - what every browser that has just loaded a
	// page holds, and what the collector is restarted in the middle of on
	// any site with visitors. Without it the restart above measured an
	// empty site: the collector stopped at once because nothing was open.
	//
	// Measured on the real binary before Z7 (NOTES): in the default
	// passthrough mode, SIGTERM closed the listener in 0.05 s and the
	// process was still waiting for this one idle connection 40 seconds
	// later. Here, on systemd, that wait ended at the unit's 90-second stop
	// timeout with a SIGKILL, the site refused new connections for all of
	// it, and the upgrader - which waits 30 - undid the update (nightly
	// run 43).
	visitor := proxyClient(t, collectorAddr, origin, true)
	visit(t, visitor, "before the update")
	newcomers := watchRefusals(collectorAddr)

	// ---- the timer's job, once ----
	//
	// Started by hand rather than by enabling the timer, so there is one
	// run and this suite knows when it ends: a oneshot's `systemctl
	// start` returns when the process exits.
	systemctl(t, "start", "crucible-upgrader.service")
	// The upgrader can give up while a unit is still stopping, and the
	// outage is not over until every unit is back. Waited for without
	// failing - what this measures is how long, and the assertions below
	// say whether that was acceptable - but bounded, past systemd's own
	// 90-second stop timeout, so a unit that never returns ends the wait.
	settled := waitActive(t, 150*time.Second)
	t.Logf("every unit active again %s after the upgrader returned", settled.Round(100*time.Millisecond))
	outage := newcomers.stop()
	t.Logf("the site refused new connections for %s at its longest (%d refused of %d attempts)",
		outage.longest.Round(10*time.Millisecond), outage.refused, outage.attempts)

	// ---- what the page will say ----
	req, err := relupdate.Latest(ctx, admin)
	if err != nil || req == nil {
		t.Fatalf("reading the request back: %v", err)
	}
	t.Logf("request: state=%s installed=%q rolled_back=%v", req.State, req.InstalledVersion, req.RolledBack)
	if req.State != relupdate.StateSucceeded {
		t.Fatalf("the update did not finish under the real units:\n  %s\n%s",
			req.ErrorChain, journal(t, "crucible-upgrader.service", "crucible-restart.service"))
	}
	if req.InstalledVersion != toVersion || req.RolledBack {
		t.Errorf("the row says installed %q, rolled back %v; want %s and false",
			req.InstalledVersion, req.RolledBack, toVersion)
	}

	// ---- and what is actually true on the machine ----
	out := runAs(t, "crucible", filepath.Join(installPrefix, "bin", "panel"), "-version")
	if !strings.Contains(out, toVersion) {
		t.Errorf("the installed panel reports %q; want %s", strings.TrimSpace(out), toVersion)
	}
	after := invocations(t)
	for _, s := range services {
		if after[s.unit] == before[s.unit] {
			t.Errorf("%s was not restarted: the same invocation %s before and after", s.unit, after[s.unit])
		}
		if state := systemctlOut(t, "is-active", s.unit); strings.TrimSpace(state) != "active" {
			t.Errorf("%s is %q after the restart", s.unit, strings.TrimSpace(state))
		}
	}
	if _, err := os.Stat(filepath.Join(doorbellDir, relupdate.DoorbellName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the doorbell is still there after the restart (%v); restart.sh clears it first", err)
	}
	if leftover, _ := filepath.Glob(filepath.Join(installPrefix, "bin", ".previous-*")); len(leftover) > 0 {
		t.Errorf("the checkpoint was kept after a healthy restart: %v", leftover)
	}
	if j := journal(t, "crucible-restart.service"); !strings.Contains(j, "restart.sh: restarting") {
		t.Errorf("crucible-restart.service's journal does not show a restart:\n%s", j)
	}

	// ---- and the visitor ----
	//
	// The site was closed to new connections while the collector
	// restarted; how long is the number a customer notices. Before Z7 it
	// was 90 seconds - systemd's stop timeout, ending in a SIGKILL - and
	// the update was undone (nightly run 43). Now the idle connection is
	// closed as idle: the refusals last the drain's quiet period at most,
	// plus a start. A drain that could not tell idle from busy would wait
	// out DrainTimeout instead, and land above this.
	if limit := proxy.DrainIdle + 3*time.Second; outage.longest >= limit {
		t.Errorf("the site refused new connections for %s during the restart; an idle "+
			"connection should be closed after %s of quiet, and the restart should refuse "+
			"visitors for less than %s", outage.longest.Round(10*time.Millisecond), proxy.DrainIdle, limit)
	}
	// The same tab, afterwards. Its connection was closed under it by the
	// restart; a browser opens a new one, and so does this client.
	visit(t, visitor, "after the update")
}

// collectorAddr is where the suite's collector listens.
const collectorAddr = "127.0.0.1:18443"

// visit makes one request through the collector and wants the origin's
// answer.
func visit(t *testing.T, c *http.Client, when string) {
	t.Helper()
	resp, err := c.Get("https://127.0.0.1/")
	if err != nil {
		t.Fatalf("a visit %s: %v", when, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "origin-served-this" {
		t.Fatalf("a visit %s: %d %q; want the origin's page", when, resp.StatusCode, body)
	}
}

// refusals is what a stream of new visitors saw while something
// restarted: how many of their connections were refused, and the longest
// unbroken stretch of refusals.
type refusals struct {
	attempts, refused int
	longest           time.Duration
}

// refusalWatch dials the collector every 100 ms until stopped.
type refusalWatch struct {
	quit chan struct{}
	done chan refusals
}

// watchRefusals starts measuring, from outside, whether the site accepts
// a new connection. A dial is enough: the listener is what closes while a
// service stops, and a refused dial is what a new visitor gets.
func watchRefusals(addr string) *refusalWatch {
	w := &refusalWatch{quit: make(chan struct{}), done: make(chan refusals, 1)}
	go func() {
		var r refusals
		var since time.Time // the start of the current run of refusals
		for {
			select {
			case <-w.quit:
				w.done <- r
				return
			default:
			}
			r.attempts++
			c, err := net.DialTimeout("tcp", addr, time.Second)
			now := time.Now()
			if err != nil {
				r.refused++
				if since.IsZero() {
					since = now
				}
				if d := now.Sub(since); d > r.longest {
					r.longest = d
				}
			} else {
				c.Close()
				since = time.Time{}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return w
}

func (w *refusalWatch) stop() refusals {
	close(w.quit)
	return <-w.done
}

// waitActive waits until every service unit is active and returns how
// long that took, or gives up at the limit and returns the limit.
func waitActive(t *testing.T, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < limit {
		all := true
		for _, s := range services {
			if strings.TrimSpace(systemctlOut(t, "is-active", s.unit)) != "active" {
				all = false
				break
			}
		}
		if all {
			return time.Since(start)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return limit
}

// ---------------------------------------------------------------- steps

// unpackPackage unpacks one of the workflow's signed packages.
func unpackPackage(t *testing.T, dist, version string) string {
	t.Helper()
	tarball := filepath.Join(dist, fmt.Sprintf("crucible-analytic-%s-linux-amd64.tar.gz", version))
	dir := t.TempDir()
	if out, err := exec.Command("tar", "xzf", tarball, "-C", dir).CombinedOutput(); err != nil {
		t.Fatalf("unpacking %s: %v\n%s", tarball, err, out)
	}
	return filepath.Join(dir, "crucible-analytic-"+version)
}

// installForReal runs the package's install.sh the way KURULUM.md does:
// as root, with systemd, into the paths the units name.
func installForReal(t *testing.T, pkg, dsn, db string) {
	t.Helper()
	cmd := exec.Command("./release/install.sh")
	cmd.Dir = pkg
	cmd.Env = append(os.Environ(), "SUPERUSER_DSN="+dsn, "DB_NAME="+db)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("every assertion holds")) {
		t.Fatalf("the install did not verify its privilege matrix:\n%s", out)
	}
	if !bytes.Contains(out, []byte("unit files -> "+unitDir)) {
		t.Fatalf("install.sh finished without writing the units into %s:\n%s", unitDir, out)
	}
}

// runDocumentedBlock runs the first shell block under a KURULUM.md
// heading, as written.
//
// Read out of the package's own copy, which is the one a customer has
// in front of them. A suite that ran its own spelling of the same
// commands would prove the commands it wrote, and the document could
// say something else for as long as nobody compared the two.
func runDocumentedBlock(t *testing.T, pkg, heading string) {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join(pkg, "KURULUM.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	at := strings.Index(text, "\n"+heading+"\n")
	if at < 0 {
		t.Fatalf("KURULUM.md has no heading %q; the suite runs the commands under it", heading)
	}
	rest := text[at+len(heading)+2:]
	if next := strings.Index(rest, "\n### "); next >= 0 {
		rest = rest[:next]
	}
	open := strings.Index(rest, "```bash\n")
	if open < 0 {
		t.Fatalf("no ```bash block under %q", heading)
	}
	block := rest[open+len("```bash\n"):]
	shut := strings.Index(block, "```")
	if shut < 0 {
		t.Fatalf("the block under %q is not closed", heading)
	}
	block = block[:shut]
	t.Logf("running, from KURULUM.md %q:\n%s", heading, block)
	out, err := exec.Command("sh", "-eu", "-c", block).CombinedOutput()
	if err != nil {
		t.Fatalf("the commands under %q failed: %v\n%s", heading, err, out)
	}
}

// serveRelease serves one signed package where the upgrader will look
// for it, over https, and makes the upgrader trust the certificate.
//
// The trust is the one line this suite adds that a customer would not:
// a drop-in naming the certificate in SSL_CERT_FILE. A real release host
// has a certificate the system already trusts.
func serveRelease(t *testing.T, dist, version string) (string, *atomic.Int64) {
	t.Helper()
	name := fmt.Sprintf("crucible-analytic-%s-linux-amd64.tar.gz", version)
	want := "/rel/" + version + "/" + name
	pkgPath := filepath.Join(dist, name)
	var downloads atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != want {
			http.NotFound(w, r)
			return
		}
		downloads.Add(1)
		http.ServeFile(w, r, pkgPath)
	}))
	cert := selfSigned(t)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// Outside the configuration directory's prefix on purpose. A second
	// directory under it is what release/dirfamily_test.go reads as the
	// configuration directory spelt two ways, and it said so the first
	// time this certificate was put beside it.
	caFile := "/etc/ssl/systemd-suite-ca.pem"
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(caFile, ca, 0o644); err != nil {
		t.Fatal(err)
	}
	dropIn := filepath.Join(unitDir, "crucible-upgrader.service.d")
	if err := os.MkdirAll(dropIn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropIn, "zz-systemd-suite.conf"),
		[]byte("[Service]\nEnvironment=SSL_CERT_FILE="+caFile+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	systemctl(t, "daemon-reload")
	return "https://" + ln.Addr().String(), &downloads
}

// waitHeartbeats waits for all four services to write a heartbeat newer
// than since - the same signal the upgrader waits for.
func waitHeartbeats(t *testing.T, pool *pgxpool.Pool, since time.Time, when string) {
	t.Helper()
	deadline := time.Now().Add(relupdate.HealthWindow)
	for {
		beats, err := heartbeat.Read(context.Background(), pool)
		if err != nil {
			t.Fatalf("reading the heartbeat: %v", err)
		}
		seen := map[string]bool{}
		for _, b := range beats {
			if b.BeatAt.After(since) {
				seen[b.Service] = true
			}
		}
		var missing []string
		for _, s := range services {
			if !seen[s.role] {
				missing = append(missing, s.unit)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no heartbeat from %s within %s", when, strings.Join(missing, ", "),
				relupdate.HealthWindow)
		}
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------- utils

func superPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// servicePool connects as one of the service roles, with the password
// writeConfigs gave it.
func servicePool(t *testing.T, superDSN, db, role string) *pgxpool.Pool {
	t.Helper()
	return superPool(t, fmt.Sprintf("postgres://%s:%s@%s/%s", role, rolePassword, hostOf(t, superDSN), db))
}

func systemctl(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		t.Fatalf("systemctl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// systemctlOut returns what systemctl printed, and does not fail on a
// non-zero exit: is-active answers "inactive" with status 3.
func systemctlOut(t *testing.T, args ...string) string {
	t.Helper()
	out, _ := exec.Command("systemctl", args...).CombinedOutput()
	return string(out)
}

// invocations reads each service's InvocationID, which changes on every
// start - a restart that systemd performed, not one this suite assumes.
func invocations(t *testing.T) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, s := range services {
		ids[s.unit] = strings.TrimSpace(systemctlOut(t, "show", "--property=InvocationID", "--value", s.unit))
		if ids[s.unit] == "" {
			t.Fatalf("%s has no invocation id; it is not running", s.unit)
		}
	}
	return ids
}

func runAs(t *testing.T, user string, argv ...string) string {
	t.Helper()
	out, err := exec.Command("runuser", append([]string{"-u", user, "--"}, argv...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("running %v as %s: %v\n%s", argv, user, err, out)
	}
	return string(out)
}

func journal(t *testing.T, units ...string) string {
	t.Helper()
	args := []string{"--no-pager", "-o", "short-iso", "-n", "200"}
	for _, u := range units {
		args = append(args, "-u", u)
	}
	out, _ := exec.Command("journalctl", args...).CombinedOutput()
	return "--- journal (" + strings.Join(units, ", ") + ") ---\n" + string(out)
}

// dumpUnits prints every unit's state and journal when the suite has
// already failed. A passing run that printed four services' logs would
// bury the one line that mattered.
func dumpUnits(t *testing.T) {
	if !t.Failed() {
		return
	}
	units := []string{"crucible-upgrader.service", "crucible-restart.service", "crucible-restart.path"}
	for _, s := range services {
		units = append(units, s.unit)
	}
	for _, u := range units {
		t.Logf("--- systemctl status %s ---\n%s", u, systemctlOut(t, "status", "--no-pager", u))
	}
	t.Log(journal(t, units...))
}

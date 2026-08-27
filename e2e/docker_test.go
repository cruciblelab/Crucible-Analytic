//go:build docker

// The same chain, from the container image and the shipped compose file.
//
//	go test -tags docker ./e2e/ -v -timeout 30m
//
// e2e_test.go proves the tarball. This proves the other deployment - one
// stack per customer, which is how this product is actually sold - and
// they are not the same claim. The tarball install runs on a machine
// with systemd, a log directory and a database on localhost; the
// container has none of those, reaches its database over TCP, and logs
// to stdout. Every one of those differences has already broken something
// that the tarball path did not notice:
//
//   - install.sh ignored --db whenever a DSN was given, which is the
//     only way a container ever connects.
//   - Its config edits need GNU sed. BusyBox sed accepted the GNU-only
//     form silently and wrote nothing, so the install reported keys it
//     had not written.
//   - The ip_hash_key and the panel's secret_key were written into the
//     wrong TOML tables, where nothing read them.
//
// None of those is a container bug. They were bugs everywhere, and the
// container is where they showed up, because a container is an
// installation nobody has quietly fixed by hand.
//
// Its own tag: it builds an image and starts five containers, which is
// minutes and a docker daemon. Not in the merge gate.
package e2e

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	dockerSite  = "docker-site"
	composeName = "ca-e2e"
)

// dockerAvailable skips rather than fails when there is no daemon.
func dockerAvailable(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("no docker daemon: %v\n%s", err, out)
	}
}

// TestTheStackWorksFromItsOwnComposeFile.
func TestTheStackWorksFromItsOwnComposeFile(t *testing.T) {
	dockerAvailable(t)
	root := repoRoot(t)

	image := buildImage(t, root)
	origin := startOriginOn(t, "0.0.0.0:0")

	// Free ports, so a run cannot collide with whatever the developer
	// already has listening.
	collectorPort := freePort(t)
	beaconPort := freePort(t)

	panelPort := freePort(t)
	env := writeComposeEnv(t, root, image, origin.addr, collectorPort, beaconPort)
	stack := composeUp(t, root, env, panelPort)

	// ---- a real request, through the published collector ----
	body := throughProxy(t, fmt.Sprintf("127.0.0.1:%d", collectorPort), origin)
	if !strings.Contains(body, "origin-served-this") {
		t.Fatalf("the proxy did not return the origin's body: %q", body)
	}
	sendPageview(t, fmt.Sprintf("127.0.0.1:%d", beaconPort), dockerSite)

	// ---- the panel draws both halves ----
	//
	// Read from inside the compose network, because the panel is not
	// published to the host - which is the property the next assertion
	// is about, and the reason this cannot be a plain HTTP client.
	page := dashboard(t, stack, fmt.Sprintf("127.0.0.1:%d", panelPort))
	for _, want := range []string{"Ziyaretçi", "İnsan trafiği"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the dashboard does not mention %q", want)
		}
	}
	empty := 0
	for _, line := range cardLines(page) {
		t.Log("card: " + line)
		if strings.HasSuffix(line, "(empty)") {
			empty++
		}
	}
	if empty > 0 {
		t.Errorf("%d dashboard cards have nothing in them", empty)
	}

	// The boundary the compose file claims - that the panel and the read
	// API are not published - is asserted in release/ports_test.go, by
	// reading the file. Probing a host port from here would prove little:
	// this test publishes the panel itself, through an override, because
	// a busybox image has no HTTP client that keeps a session cookie.
}

// buildImage builds the image under test and returns its tag.
func buildImage(t *testing.T, root string) string {
	t.Helper()
	tag := "crucible-analytic:e2e-test"

	args := []string{"build", "--build-arg", "VERSION=e2e-test", "-t", tag}
	// An extra CA for a build behind a TLS-terminating proxy. The
	// Dockerfile takes it as an optional secret; this passes one through
	// when the environment names it, so a locked-down CI runner can run
	// this test at all.
	if ca := os.Getenv("CA_BUILD_EXTRA_CA"); ca != "" {
		args = append(args, "--secret", "id=extra_ca,src="+ca)
		args = append(args, "--network", "host")
	}
	args = append(args, ".")

	cmd := exec.Command("docker", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build failed: %v\n%s", err, lastLines(string(out), 25))
	}
	return tag
}

// writeComposeEnv writes the .env the shipped compose file reads.
//
// Into a temporary file passed with --env-file rather than docker/.env,
// which is a developer's own and must not be overwritten by a test.
func writeComposeEnv(t *testing.T, root, image, backend string, collectorPort, beaconPort int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.env")
	body := fmt.Sprintf(`POSTGRES_PASSWORD=e2e-postgres-password
SITE_ID=%s
SITE_BACKEND=%s
COLLECTOR_PORT=%d
BEACON_PORT=%d
CA_IMAGE=%s
`, dockerSite, hostAsSeenFromContainer(backend), collectorPort, beaconPort, image)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// hostAsSeenFromContainer turns a 127.0.0.1 address into one a container
// can reach.
//
// The origin runs in this test process, on the host's loopback. Inside a
// container 127.0.0.1 is that container, so the collector would proxy to
// itself and the request would hang rather than fail - which is a worse
// symptom than a refused connection.
func hostAsSeenFromContainer(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return "host.docker.internal:" + port
}

// stack is one running compose project.
type stack struct {
	dir      string
	envFile  string
	override string
}

// base is the argument prefix every compose command shares.
func (s stack) base(args ...string) []string {
	return append([]string{
		"compose", "-p", composeName,
		"-f", "compose.yml", "-f", s.override,
		"--env-file", s.envFile,
	}, args...)
}

func (s stack) command(args ...string) *exec.Cmd {
	cmd := exec.Command("docker", s.base(args...)...)
	cmd.Dir = s.dir
	return cmd
}

// composeUp brings the shipped compose file up, with one override.
//
// The override adds host.docker.internal to the collector, because the
// TLS origin this test proxies to runs in the test process on the host
// and a container cannot reach the host's loopback by that name on
// Linux. It is written to a temporary file and passed with a second -f
// rather than edited into docker/compose.yml: the shipped file is what
// is under test, and a test that modifies its subject is testing
// something else.
func composeUp(t *testing.T, root, envFile string, panelPort int) stack {
	t.Helper()

	override := filepath.Join(t.TempDir(), "override.yml")
	// Two changes, and both are the test's arrangement rather than a
	// customer's.
	//
	// host.docker.internal, because the TLS origin this test proxies to
	// runs in the test process on the host and a container cannot reach
	// the host's loopback by that name on Linux.
	//
	// A published panel port, because the image carries busybox wget,
	// which has no cookie support - so there is no way to sign in from
	// inside the network. That the shipped file publishes neither the
	// panel nor the read API is asserted by reading it, in
	// release/ports_test.go, which is a sharper check than probing a
	// host port that nothing was listening on anyway.
	if err := os.WriteFile(override, fmt.Appendf(nil, `services:
  collector:
    extra_hosts:
      - "host.docker.internal:host-gateway"
  panel:
    ports:
      - "%d:8090"
`, panelPort), 0o600); err != nil {
		t.Fatal(err)
	}

	s := stack{dir: filepath.Join(root, "docker"), envFile: envFile, override: override}

	down := func() { _ = s.command("down", "-v", "--remove-orphans").Run() }
	down() // in case a previous run died before its own cleanup
	t.Cleanup(down)

	if out, err := s.command("up", "-d", "--wait").CombinedOutput(); err != nil {
		details, _ := s.command("logs", "--tail", "40").CombinedOutput()
		t.Fatalf("docker compose up failed: %v\n%s\n--- logs ---\n%s",
			err, lastLines(string(out), 20), lastLines(string(details), 40))
	}

	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		out, _ := s.command("logs", "--tail", "60").CombinedOutput()
		t.Logf("--- compose logs ---\n%s", out)
	})
	return s
}

// dashboard redeems a developer link and returns the site page.
//
// Through a normal HTTP client with a cookie jar, against the panel port
// this test publishes. The link itself is minted the way an installer
// does it - `panel -dev-link` in a one-off container on the compose
// network, reading the same configuration volume the running panel does.
func dashboard(t *testing.T, s stack, panelAddr string) string {
	t.Helper()

	// Plain HTTP means the session cookie has to stop being Secure,
	// which is the one setting these suites change that a customer would
	// not - see the note in e2e_test.go.
	run(t, s, "panel-cli", "sh", "-c",
		`sed -i 's|^# secure_cookies = true|secure_cookies = false|' /etc/crucible-analytic/panel.toml`)
	restart(t, s, "panel")

	link := strings.TrimSpace(run(t, s, "panel-cli", "sh", "-c",
		`panel -config /etc/crucible-analytic/panel.toml -dev-link -base-url http://`+panelAddr+` 2>/dev/null | head -1`))
	if !strings.Contains(link, DevAccessSegment) {
		t.Fatalf("minting a developer link did not produce one: %q", link)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second}

	landed, _, _ := getPage(t, client, link)
	if !strings.Contains(landed, "/kurulum/") {
		t.Fatalf("redeeming the developer link landed on %s, not the wizard", landed)
	}

	_, status, body := getPage(t, client, "http://"+panelAddr+"/site/"+dockerSite)
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}
	return body
}

// run executes a command in a one-off container and returns its stdout.
//
// stdout and stderr kept apart, which matters more here than it looks:
// `docker compose run` writes its own "Container … Creating" progress to
// stderr, and a helper that merged the two handed those lines back as
// part of the developer link it had just captured. The failure was a
// wget refusing a URL with a newline in it, three steps later.
func run(t *testing.T, s stack, service string, args ...string) string {
	t.Helper()
	cmd := s.command(append([]string{"run", "--rm", "--no-deps", "-T", service}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("compose run %s %v: %v\n--- stdout ---\n%s\n--- stderr ---\n%s",
			service, args, err, out.String(), lastLines(errOut.String(), 15))
	}
	return out.String()
}

func restart(t *testing.T, s stack, service string) {
	t.Helper()
	if out, err := s.command("restart", service).CombinedOutput(); err != nil {
		t.Fatalf("restarting %s: %v\n%s", service, err, out)
	}
	time.Sleep(3 * time.Second)
}

// reachable reports whether anything accepts a connection there.
func reachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

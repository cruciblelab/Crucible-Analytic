//go:build e2e || systemd

// What the tarball suite and the systemd suite both need: a database, a
// package's configuration files filled in the way an operator fills
// them, and a way to wait for a port.
//
// Moved here verbatim from e2e_test.go when systemd_test.go arrived. The
// two suites install the same package - one with --no-systemd into a
// scratch tree, one for real into /opt and /etc under the units - and a
// second copy of these helpers would be a second answer to "what does an
// operator write into these files", which is the question both suites
// are asking.
package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
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

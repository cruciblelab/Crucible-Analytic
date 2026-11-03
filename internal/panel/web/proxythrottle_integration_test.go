//go:build integration

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// forwardedAs is a transport that adds the header a reverse proxy would:
// the address it saw the visitor connect from.
type forwardedAs struct {
	addr string
	next http.RoundTripper
}

func (f forwardedAs) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", f.addr)
	return f.next.RoundTrip(r)
}

// H12, as the live rig measured it. Behind the reverse proxy KURULUM
// section 8 sets up, thirty wrong passwords from one machine, for
// addresses that do not even exist, and the owner - somewhere else, with
// the right password - was told to wait fifteen minutes. Here the panel
// trusts its proxy, so the attacker's failures are counted against the
// attacker.
func TestSignInLimitsCountTheVisitorNotTheProxy(t *testing.T) {
	srv, store := setupTestServer(t)
	resolver, err := Config{TrustedProxies: []string{"127.0.0.1/32", "::1/128"}}.ClientIPResolver()
	if err != nil {
		t.Fatal(err)
	}
	srv.ClientIP = resolver
	owner := makeUser(t, store, "vekil-arkasi", false)

	const attacker, visitor = "203.0.113.66", "198.51.100.77"
	clearAttemptsFrom(t, attacker, visitor)
	t.Cleanup(func() { clearAttemptsFrom(t, attacker, visitor) })

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	from := func(addr string) *http.Client {
		c := newClient(t, server.URL)
		c.Transport = forwardedAs{addr: addr, next: http.DefaultTransport}
		return c
	}

	// A different address each time, so the per-account limit never
	// fires: only the per-address one can stop this.
	spray := from(attacker)
	var last int
	for i := 0; i < 40; i++ {
		resp := signIn(t, spray, server.URL, fmt.Sprintf("yok%d-vekil-arkasi@saldirgan.example", i), "parola123456")
		resp.Body.Close()
		last = resp.StatusCode
		if last == http.StatusTooManyRequests {
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("forty failures from one address never produced a 429 (last %d); the "+
			"per-address limit is not counting", last)
	}

	// The owner, behind the same proxy, from their own address.
	resp := signIn(t, from(visitor), server.URL, owner.Email, testAccountPassword)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the owner's correct password from %s answered %d after somebody else's "+
			"failures from %s; behind the proxy every visitor is still one address",
			visitor, resp.StatusCode, attacker)
	}

	// And what was recorded is the visitor, not the proxy.
	var proxyRows, attackerRows int
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE ip <<= '127.0.0.0/8' OR ip = '::1'),
		       count(*) FILTER (WHERE ip = $1::inet)
		  FROM panel_login_attempts WHERE email LIKE '%vekil-arkasi%'`, attacker).
		Scan(&proxyRows, &attackerRows); err != nil {
		t.Fatal(err)
	}
	if proxyRows != 0 || attackerRows == 0 {
		t.Errorf("attempts recorded against the proxy: %d, against the attacker: %d", proxyRows, attackerRows)
	}
}

// clearAttemptsFrom removes the sign-in attempts recorded against addrs,
// at both ends: a run that stopped halfway leaves a full budget behind.
func clearAttemptsFrom(t *testing.T, addrs ...string) {
	t.Helper()
	admin := testdb.Admin(t)
	for _, a := range addrs {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM panel_login_attempts WHERE ip = $1::inet`, a); err != nil {
			t.Fatalf("clearing attempts from %s: %v", a, err)
		}
	}
}

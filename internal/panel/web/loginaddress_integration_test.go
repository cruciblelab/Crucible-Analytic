//go:build integration

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// An address no account can have is an ordinary failed sign-in.
//
// # The defect this holds (PLAN §H8)
//
// The sign-in form takes the address from anybody, and three records
// key on or carry it: the attempt (idx_panel_login_attempts_email), the
// throttle's count over those attempts, and the audit entry. Before H8
// none of the three bounded or cleaned it. Measured with this test, built
// against the code before the fix:
//
//   - an address of 700 four-byte characters (2.8 KB, inside the 256 KB
//     form cap) made an index entry PostgreSQL refuses, so the attempt
//     was never recorded and never counted towards the address's limit;
//   - a NUL byte made the throttle's own query fail, and the page
//     answered 500 with an ERROR line - every time, for anybody;
//   - invalid UTF-8 lost the audit entry, whose label is what was typed.
//     Its attempt was recorded, but only because NormalizeEmail
//     lowercases and strings.ToLower rewrites invalid bytes as U+FFFD - a
//     side effect a refactor removes, which this case says out loud is
//     relied on.
//
// None of them reached an account: no account has such an address. But a
// failed sign-in that leaves no record is the record an attacker
// working through a list would most like to not leave, and a request
// anybody can make into a 500 and an ERROR line is a way to fill the
// log an operator reads.
//
// So each case asserts the ordinary outcome: the refusal page, the
// attempt in the table, the audit entry beside it. The addresses start
// with a prefix no fixture uses, so the rows are this test's and the
// cleanup removes exactly them - before, for a run that died, and after.
func TestAnAddressNoAccountCanHaveIsAnOrdinaryFailure(t *testing.T) {
	srv, _ := setupTestServer(t)
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)

	admin := testdb.Admin(t)
	const prefix = "h8-adres-"
	clean := func() {
		for _, sql := range []string{
			`DELETE FROM panel_login_attempts WHERE email LIKE '` + prefix + `%'`,
			`DELETE FROM panel_audit_log WHERE actor_label LIKE '` + prefix + `%'`,
		} {
			if _, err := admin.Exec(context.Background(), sql); err != nil {
				t.Errorf("%s: %v", sql, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)

	count := func(sql string) int {
		t.Helper()
		var n int
		if err := admin.QueryRow(context.Background(), sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}

	for _, c := range []struct {
		name, email string
	}{
		{"700 dört baytlık karakter", prefix + "uzun-" + fourByteAddressRunes(700) + "@ornek.test"},
		{"NUL baytı", prefix + "nul\x00bayt@ornek.test"},
		{"geçersiz UTF-8", prefix + "gecersiz\xff@ornek.test"},
	} {
		t.Run(c.name, func(t *testing.T) {
			attempts := count(`SELECT count(*) FROM panel_login_attempts WHERE email LIKE '` + prefix + `%' AND NOT success`)
			audits := count(`SELECT count(*) FROM panel_audit_log WHERE actor_label LIKE '` + prefix + `%'`)

			resp := signIn(t, newClient(t, server.URL), server.URL, c.email, "yanlis-parola-h8")
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("the sign-in answered %d, want %d - an address no account can have "+
					"is a wrong address, not a server fault", resp.StatusCode, http.StatusUnauthorized)
			}
			if got := count(`SELECT count(*) FROM panel_login_attempts WHERE email LIKE '`+prefix+`%' AND NOT success`) - attempts; got != 1 {
				t.Errorf("%d failed attempts recorded, want 1 - an unrecorded attempt is one the "+
					"address's limit never counts", got)
			}
			if got := count(`SELECT count(*) FROM panel_audit_log WHERE actor_label LIKE '`+prefix+`%'`) - audits; got != 1 {
				t.Errorf("%d audit entries written, want 1", got)
			}
		})
	}
}

// fourByteAddressRunes is n distinct-ish four-byte runes, so neither a
// rune cap nor compression hides how many bytes they are.
func fourByteAddressRunes(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteRune(rune(0x20000 + (i*104729)%0xA6E0))
	}
	return b.String()
}

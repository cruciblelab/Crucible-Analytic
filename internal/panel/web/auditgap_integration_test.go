//go:build integration

package web

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// The entries an audit log is opened for.
//
// # What was wrong
//
// recordFailedLogin wrote an AuditEntry with no ActorKind. The column
// has a check constraint naming the kinds it allows, the empty string is
// not one of them, the database refused every such row, and the caller
// discarded the error with `_ =`. Three throttle records did the same.
//
// Nothing failed. The request answered 401 exactly as it should, the
// tests stayed green, and the log quietly kept only the successes.
// Counted on a development database before the fix: 1075
// `login.succeeded` entries, and zero `login.failed` from the sign-in
// path.
//
// That is the worst possible half to lose. Somebody working through a
// list of addresses against the door leaves no trace at all; somebody
// who gets in leaves a row.
//
// # Why this test reads the database rather than the handler
//
// The handler behaved correctly the whole time. The 401 was right, the
// message was right, and the audit call was made. What went wrong was
// three layers down, in a constraint, and it was invisible from
// everywhere above it.
//
// So the assertion is the row: after a failed sign-in, an entry exists.
//
// *Yokluğu, "sayı bulamadım" ile aynı şey sanan bir kontrol, kusurun tam
// da ürettiği şekli görmez.*

// auditEntriesFor is how many entries name this address with this action.
//
// Counted through the pool rather than through Store.Audit, because that
// reader filters by site and these entries have no site: a sign-in
// happens before anybody has chosen one.
func auditEntriesFor(t *testing.T, store *panel.Store, action, label string) int {
	t.Helper()
	var n int
	err := store.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM panel_audit_log
		WHERE action = $1 AND actor_label = $2`, action, label).Scan(&n)
	if err != nil {
		t.Fatalf("counting audit entries: %v", err)
	}
	return n
}

// TestAFailedSignInIsWrittenToTheAuditLog.
func TestAFailedSignInIsWrittenToTheAuditLog(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "denetim-basarisiz", false)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	before := auditEntriesFor(t, store, panel.ActionLoginFailed, user.Email)

	resp := signIn(t, client, server.URL, user.Email, "yanlis-parola-yeterince-uzun")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("a wrong password answered %d, want 401", resp.StatusCode)
	}

	after := auditEntriesFor(t, store, panel.ActionLoginFailed, user.Email)
	if after != before+1 {
		t.Fatalf("failed sign-ins recorded: %d before, %d after.\n"+
			"A refused sign-in that leaves no entry is the one an audit log is "+
			"read for. The request answers 401 either way, so nothing else in "+
			"this suite can tell.", before, after)
	}
}

// TestTheFailedSignInEntryNamesAnActorTheDatabaseAccepts.
//
// The row existing is half of it. The other half is what is in it: the
// kind has to be a value the constraint allows, or the insert is refused
// again the moment somebody widens the entry - and it has to be honest
// about there being no account behind it, because "user" would say an
// account acted when not proving that is the whole event.
func TestTheFailedSignInEntryNamesAnActorTheDatabaseAccepts(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "denetim-aktor", false)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	resp := signIn(t, client, server.URL, user.Email, "yanlis-parola-yeterince-uzun")
	resp.Body.Close()

	var kind string
	var actorID *int64
	err := store.Pool().QueryRow(context.Background(), `
		SELECT actor_kind, actor_id FROM panel_audit_log
		WHERE action = $1 AND actor_label = $2
		ORDER BY id DESC LIMIT 1`, panel.ActionLoginFailed, user.Email).Scan(&kind, &actorID)
	if err != nil {
		t.Fatalf("reading the entry back: %v", err)
	}
	if panel.PrincipalKind(kind) != panel.PrincipalAnonymous {
		t.Errorf("the entry is recorded as %q, want %q", kind, panel.PrincipalAnonymous)
	}
	if actorID != nil {
		t.Errorf("the entry names account %d. Nobody proved they are that "+
			"account - that is what failed", *actorID)
	}
}

// TestAnAddressWithNoAccountIsRecordedToo.
//
// The address nobody has an account for is the one an attacker is most
// likely to be trying, and it takes a different path through the
// handler: there is no user row to look up. A version that recorded only
// known addresses would leave exactly the wrong half.
func TestAnAddressWithNoAccountIsRecordedToo(t *testing.T) {
	srv, store := setupTestServer(t)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	// Cleaned up by hand: makeUser is what usually registers that, and
	// there is deliberately no account here.
	email := "hic-yok-boyle-biri" + testEmailSuffix
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(),
			`DELETE FROM panel_login_attempts WHERE email = $1`, email)
	})

	before := auditEntriesFor(t, store, panel.ActionLoginFailed, email)
	resp := signIn(t, client, server.URL, email, "yanlis-parola-yeterince-uzun")
	resp.Body.Close()

	if after := auditEntriesFor(t, store, panel.ActionLoginFailed, email); after != before+1 {
		t.Errorf("an unknown address left %d entries, want one more than %d",
			after, before)
	}
}

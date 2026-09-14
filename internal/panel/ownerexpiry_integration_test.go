//go:build integration

package panel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The database refuses an owner with an end date, asked of the database.
//
// # Why this test exists when one already covered the rule
//
// It did not cover the rule. TestOwnershipCannotExpire goes through
// AddMember, which refuses the grant in Go and returns
// ErrOwnershipCannotExpire - so the statement never reaches the
// database and the CHECK constraint is never consulted. Measured:
// deleting
//
//	ALTER TABLE panel_site_members ADD CONSTRAINT
//	    panel_site_members_owner_never_expires
//	    CHECK (expires_at IS NULL OR role <> 'owner');
//
// from internal/panel/schema.sql broke nothing in this package.
//
// That matters because of why the constraint is in the database at all,
// which its own comment says: *four paths write this table.* AddMember
// is one of them. The Go refusal is the good error message; the CHECK is
// the rule. A test that only ever meets the good error message is a test
// that would still pass with the rule gone - and the thing the rule
// prevents is a site whose only owner expires on a timer, which is
// C9.2's entire subject.
//
// So this one writes SQL directly, as panel_user, which is the role the
// panel's own connection uses. No Go guard stands between it and the
// table.
func TestTheDatabaseItselfRefusesAnOwnerWithAnEndDate(t *testing.T) {
	ctx := context.Background()

	// Applied first, as the superuser, because this suite shares the
	// development database: the claim is about this tree's schema, not
	// about the shape the database happens to be in.
	admin := testdb.Admin(t)
	if _, err := admin.Exec(ctx, SchemaSQL); err != nil {
		t.Fatalf("applying the panel schema: %v", err)
	}

	pool := testdb.Pool(t, testdb.Panel)
	const site = "d-sahip-bitis"
	future := time.Now().Add(24 * time.Hour)

	// A user row to hang the membership on, and both cleaned up
	// afterwards - this table is shared with three other panel suites.
	var userID int64
	if err := admin.QueryRow(ctx, `
		INSERT INTO panel_users (email, password_hash) VALUES ($1, 'not-a-hash')
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id`, "sahip-bitis@example.invalid").Scan(&userID); err != nil {
		t.Fatalf("seeding a user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		if _, err := admin.Exec(c,
			`DELETE FROM panel_site_members WHERE site_id = $1`, site); err != nil {
			t.Errorf("clearing the membership: %v", err)
		}
		if _, err := admin.Exec(c, `DELETE FROM panel_users WHERE id = $1`, userID); err != nil {
			t.Errorf("clearing the user: %v", err)
		}
	})

	// Straight in, as an owner with an expiry.
	_, err := pool.Exec(ctx, `
		INSERT INTO panel_site_members (site_id, user_id, role, expires_at)
		VALUES ($1, $2, 'owner', $3)`, site, userID, future)
	if err == nil {
		t.Error("the database accepted an owner with an end date.\n" +
			"AddMember refuses this in Go, and that refusal is the message a caller " +
			"sees - but four paths write this table and the CHECK is what covers the " +
			"other three. Without it a site can be left with an owner that expires, " +
			"which is a site with no owner at a time nobody chose.")
	} else if !strings.Contains(err.Error(), "owner_never_expires") {
		t.Errorf("the insert failed for some reason other than the constraint, so this "+
			"test is not measuring the constraint: %v", err)
	}

	// The two halves that must still work, or the constraint would be
	// refusing more than the rule: an owner with no end date, and a
	// non-owner with one.
	if _, err := pool.Exec(ctx, `
		INSERT INTO panel_site_members (site_id, user_id, role, expires_at)
		VALUES ($1, $2, 'owner', NULL)`, site, userID); err != nil {
		t.Fatalf("an owner without an end date was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE panel_site_members SET role = 'admin', expires_at = $3
		WHERE site_id = $1 AND user_id = $2`, site, userID, future); err != nil {
		t.Errorf("an admin with an end date was refused: %v.\n"+
			"The constraint must bound owners only; C9.2's whole feature is a "+
			"membership that ends.", err)
	}

	// And the update direction, which is the one AddMember's Go guard
	// does not stand in front of at all: raising an existing timed
	// member to owner while the end date stays.
	_, err = pool.Exec(ctx, `
		UPDATE panel_site_members SET role = 'owner'
		WHERE site_id = $1 AND user_id = $2`, site, userID)
	if err == nil {
		t.Error("an admin with an end date was raised to owner and kept the end date.\n" +
			"This is the path a UPDATE takes with no Go call in front of it, and it is " +
			"the one the constraint exists for.")
	} else if !strings.Contains(err.Error(), "owner_never_expires") {
		t.Errorf("the update failed for some reason other than the constraint: %v", err)
	}
}

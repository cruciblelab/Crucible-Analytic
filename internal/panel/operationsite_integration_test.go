//go:build integration

package panel

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAnOperationsSiteMustBeASite.
//
// The writer's half of the release form's fault (internal/panel/web,
// operationsite_integration_test.go): a value in site_id that is not a
// site identifier is refused by name, before PostgreSQL refuses it as an
// over-long index entry without one. The web tests prove the forms no
// longer pass one; this proves the next caller that does gets a sentence.
func TestAnOperationsSiteMustBeASite(t *testing.T) {
	store := newTestStore(t, "opsite")
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(ctx, `DELETE FROM panel_operations WHERE action = 'test.opsite'`)
	})

	_, err := store.BeginOperation(ctx, Access{}, "test.opsite", "", strings.Repeat("a", 3000))
	if err == nil {
		t.Fatal("a 3000-byte site was accepted")
	}
	if !strings.Contains(err.Error(), "not a site id") {
		t.Errorf("the refusal does not say what was wrong: %v", err)
	}

	// The boundary, both sides: a site at the longest a key may be is
	// one, and a site with a NUL in it is not.
	if _, err := store.BeginOperation(ctx, Access{}, "test.opsite", "", strings.Repeat("a", 256)); err != nil {
		t.Errorf("a 256-byte site was refused: %v", err)
	}
	if _, err := store.BeginOperation(ctx, Access{}, "test.opsite", "", "a\x00b"); err == nil {
		t.Error("a site with a NUL in it was accepted")
	}
}

// TestAnOperationsTargetIsCleanedAndCut.
//
// target is where a detail goes, typed or not, so the writer makes it
// storable: control characters out, then at most 256 bytes, never inside
// a character.
func TestAnOperationsTargetIsCleanedAndCut(t *testing.T) {
	store := newTestStore(t, "optarget")
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(ctx, `DELETE FROM panel_operations WHERE action = 'test.optarget'`)
	})

	// Three-byte characters, so 256 falls inside one: 85 fit, 86 do not.
	long := strings.Repeat("ş€", 200)
	for _, c := range []struct {
		in   string
		want func(string) bool
		say  string
	}{
		{"v1\x00.2\x07", func(s string) bool { return s == "v1.2" }, "the control characters removed"},
		{long, func(s string) bool {
			return len(s) <= 256 && len(s) > 250 && utf8.ValidString(s) && strings.HasPrefix(long, s)
		}, "the start of it, at most 256 bytes and whole characters"},
	} {
		op, err := store.BeginOperation(ctx, Access{}, "test.optarget", c.in, "")
		if err != nil {
			t.Fatalf("BeginOperation: %v", err)
		}
		var got string
		if err := store.Pool().QueryRow(ctx,
			`SELECT target FROM panel_operations WHERE id = $1`, op.ID()).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !c.want(got) {
			t.Errorf("the target %q (%d bytes) was stored as %q (%d bytes); want %s",
				c.in, len(c.in), got, len(got), c.say)
		}
	}
}

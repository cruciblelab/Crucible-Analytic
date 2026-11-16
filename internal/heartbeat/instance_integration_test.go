//go:build integration

package heartbeat

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// keysOf is the row keys Read returns for role, sorted.
func keysOf(t *testing.T, beats []Beat, role string) []string {
	t.Helper()
	var out []string
	for _, b := range beats {
		if b.Service == role {
			if b.Key() != RowKey(b.Service, b.Instance) || strings.Contains(b.Service, InstanceSeparator) {
				t.Fatalf("Read returned %+v: the key was not split into role and instance", b)
			}
			out = append(out, b.Key())
		}
	}
	sort.Strings(out)
	return out
}

// Two collectors on one host write two rows, and once an instance has
// started after the role's own row last beat - the row a build before
// instances left behind - Read stops returning that row. An older build
// still running writes it again, and then it is back: it is that
// process's row (W3). The row itself stays in the table throughout: no
// service may delete one.
func TestInstancesOfOneRoleWriteTheirOwnRows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	role := testdb.Collector

	New(Options{Pool: pool, Version: "eski"}).beat(ctx)
	time.Sleep(20 * time.Millisecond) // the instances start after that beat
	a := New(Options{Pool: pool, Version: "yeni", Instance: "w3-a"})
	a.beat(ctx)
	New(Options{Pool: pool, Version: "yeni", Instance: "w3-b"}).beat(ctx)

	beats, err := Read(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keysOf(t, beats, role), []string{role + "@w3-a", role + "@w3-b"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows %v, want %v - the role's own row should be left out once an instance started after it", got, want)
	}
	var stored int
	if err := testdb.Admin(t).QueryRow(ctx, `SELECT count(*) FROM service_heartbeat WHERE service = $1`, role).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("the role's own row: %d in the table (%v), want it still there - readers leave it out, nobody deletes it", stored, err)
	}
	for _, b := range beats {
		if b.Service == role && b.Version != "yeni" {
			t.Errorf("%s carries version %q", b.Key(), b.Version)
		}
	}

	New(Options{Pool: pool, Version: "eski"}).beat(ctx)
	a.beat(ctx)
	beats, err = Read(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(t, beats, role); len(got) != 3 || got[0] != role {
		t.Errorf("rows %v, want the older build's row back beside the two instances", got)
	}
}

// An instance that is not a site id is dropped, and the process writes
// the role's own row: a separator inside it would be read back as a
// different role than the policy checked.
func TestAnInstanceThatIsNotASiteIDIsDropped(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	New(Options{Pool: pool, Instance: "a@b"}).beat(ctx)
	beats, err := Read(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(t, beats, testdb.Collector); len(got) != 1 || got[0] != testdb.Collector {
		t.Errorf("rows %v, want only the role's own row", got)
	}
}

// The write policy, asked as the collector role: its own row and its
// instances', never another role's - and not a role that merely begins
// with its name, which a LIKE pattern on current_user would let through.
func TestThePolicyKeepsInstancesInsideTheirRole(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	admin := testdb.Admin(t)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(),
			`DELETE FROM service_heartbeat WHERE service IN ($1, $2)`,
			testdb.Beacon+"@w3-yabanci", testdb.Collector+"x@w3")
	})
	insert := func(key string) error {
		_, err := pool.Exec(ctx, `INSERT INTO service_heartbeat (service, started_at, beat_at)
			VALUES ($1, now(), now())`, key)
		return err
	}
	if err := insert(testdb.Collector + "@w3-kendi"); err != nil {
		t.Errorf("the collector could not write its own instance's row: %v", err)
	}
	for _, key := range []string{testdb.Beacon + "@w3-yabanci", testdb.Collector + "x@w3"} {
		err := insert(key)
		if err == nil {
			t.Errorf("the collector wrote %q", key)
			continue
		}
		if !strings.Contains(err.Error(), "row-level security") {
			t.Errorf("%q refused for the wrong reason: %v", key, err)
		}
	}
}

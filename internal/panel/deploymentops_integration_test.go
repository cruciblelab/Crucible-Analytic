//go:build integration

package panel

import (
	"context"
	"errors"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// TestNoOperationOnTheMachineIsOneCustomersToRun (W1b).
//
// Measured on the live rig, with two customers on one deployment: the
// owner of one site started a range refresh, a backup check that took
// the backup queue's only slot - the restore that followed was refused
// as "a backup is already running" - and could have started a backup of
// everybody's data and prepared a restore whose report counts everybody's
// rows. Each operation asked only CapManageSettings, and the health page
// gave every site owner that.
//
// Asked of every operation the health page offers, at the store, with
// the Access a per-site page would build for the owner of one site among
// others. Then the same Access, as the owner of every site, passes the
// entitlement: its context is already cancelled, so what it runs into
// next is the database refusing a dead context - proof the call got past
// the check, and nothing written to a queue other suites read.
func TestNoOperationOnTheMachineIsOneCustomersToRun(t *testing.T) {
	store := &Store{pool: testdb.Pool(t, testdb.Panel)}
	tenant := Access{
		Principal: Principal{Kind: PrincipalUser, UserID: 1, Label: "kiraci@example.invalid"},
		Role:      RoleOwner, Member: true, SiteID: "w1b-kendi",
	}
	every := tenant
	every.OwnsEverySite = true
	// A viewer is refused as a viewer: their reason is the role, not
	// the other customers, and on a one-customer deployment the "it
	// reaches sites that are not yours" sentence would be false.
	viewer := tenant
	viewer.Role = RoleViewer

	var none devgate.Authorization
	operations := map[string]func(context.Context, Access) error{
		"backup": func(ctx context.Context, a Access) error {
			_, err := store.RequestBackup(ctx, a, none, "", []string{"veri"})
			return err
		},
		"backup check": func(ctx context.Context, a Access) error {
			_, err := store.VerifyBackup(ctx, a, "", 1)
			return err
		},
		"restore": func(ctx context.Context, a Access) error {
			_, err := store.RestoreBackup(ctx, a, "", 1)
			return err
		},
		"range refresh": func(ctx context.Context, a Access) error {
			_, err := store.RequestRangeRefresh(ctx, a, "")
			return err
		},
		"release": func(ctx context.Context, a Access) error {
			_, err := store.RequestRelease(ctx, a, none, "", "v0.0.0", "v0.0.1")
			return err
		},
		"restart": func(ctx context.Context, a Access) error {
			_, err := store.RequestRestart(ctx, a, none, "collector")
			return err
		},
		"upgrade": func(ctx context.Context, a Access) error {
			_, err := store.RequestUpgrade(ctx, a, none, "")
			return err
		},
	}

	for name, run := range operations {
		if err := run(context.Background(), tenant); !errors.Is(err, ErrSettingShared) {
			t.Errorf("%s by the owner of one site among others = %v, want %v",
				name, err, ErrSettingShared)
		}

		if err := run(context.Background(), viewer); !errors.Is(err, ErrSettingNotWritable) ||
			errors.Is(err, ErrSettingShared) {
			t.Errorf("%s by a viewer = %v, want %v and not the shared-setting reason",
				name, err, ErrSettingNotWritable)
		}

		dead, cancel := context.WithCancel(context.Background())
		cancel()
		if err := run(dead, every); errors.Is(err, ErrSettingNotWritable) {
			t.Errorf("%s by the owner of every site was refused its entitlement: %v", name, err)
		}
	}
}

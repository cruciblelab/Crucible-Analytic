package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/collector"
)

// TestTheStartupLineSaysWhichKindOfMissingItIs.
//
// # The bug this is about
//
// Two situations reach this line looking identical, and the advice for
// them is opposite:
//
//   - never fetched. Ordinary. One command away from fixed, and the
//     line names the command.
//   - cannot be written here. A misconfiguration, and that same command
//     will fail. The operator runs it, it fails, the next restart says
//     "never fetched" again, and the known-bot signal stays off - half
//     of the D3 crossover, silently, for good.
//
// The measured case is a systemd install whose unit carries
// ProtectSystem=strict with the state directory spelled one way and
// bot_data.path spelled the other. Nothing about the directory looks
// wrong. The mount underneath it is read-only.
//
// # Why the wording is asserted and not just the level
//
// The whole defect was a message that was true about the file and wrong
// about the cause. A test that only checked "something was logged"
// would pass on the broken version, and so would one that only checked
// the level. So each case names a phrase that must appear and the
// phrases that must not - including, for the unwritable one, the
// command that will not help.
//
// # The fixture, and why one case skips as root
//
// The unwritable case is an unwritable directory, because the branch
// under test is only reached when the file itself is *absent*: a
// present-but-unreadable file goes to Load's error branch instead, one
// case earlier. That rules out the tricks that hold for root - a
// non-directory parent gives ENOTDIR on the read, not ENOENT - and
// leaves a mode that root is simply permitted to ignore.
//
// So it skips there rather than quietly asserting nothing. CI runs the
// suite unprivileged and sees it; this was also run by hand as nobody.
func TestTheStartupLineSaysWhichKindOfMissingItIs(t *testing.T) {
	cases := []struct {
		name              string
		path              func(t *testing.T, root string) string
		want              string
		reject            []string
		wantLevel         slog.Level
		rootCanDoAnything bool
	}{
		{
			name: "a writable directory with nothing in it yet",
			path: func(_ *testing.T, root string) string {
				return filepath.Join(root, "known_bots.json")
			},
			want:      "never been fetched",
			reject:    []string{"cannot be written"},
			wantLevel: slog.LevelInfo,
		},
		{
			name: "a directory this service may not write to",
			path: func(t *testing.T, root string) string {
				locked := filepath.Join(root, "locked")
				if err := os.Mkdir(locked, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
				return filepath.Join(locked, "known_bots.json")
			},
			want: "cannot be written",
			// Both of the old line's halves. "-update-bot-data" is the
			// one that costs an afternoon: it is the command that is
			// guaranteed not to help here.
			reject:            []string{"never been fetched", "-update-bot-data"},
			wantLevel:         slog.LevelWarn,
			rootCanDoAnything: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rootCanDoAnything && os.Geteuid() == 0 {
				t.Skip("running as root, which is permitted to write anywhere; this " +
					"arrangement says nothing here. Run the suite as an unprivileged " +
					"user to see it")
			}

			var out bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))

			cfg := &collector.Config{}
			cfg.BotData.Path = tc.path(t, t.TempDir())

			bots, set := loadBotData(cfg, logger)
			if len(bots) != 0 || set.Len() != 0 {
				t.Fatalf("expected an empty set, got %d fingerprints", set.Len())
			}

			line := out.String()
			if !strings.Contains(line, tc.want) {
				t.Errorf("the startup line does not say %q:\n%s", tc.want, line)
			}
			for _, no := range tc.reject {
				if strings.Contains(line, no) {
					t.Errorf("the startup line says %q, which belongs to the other case:\n%s",
						no, line)
				}
			}
			if want := tc.wantLevel.String(); !strings.Contains(line, `"level":"`+want+`"`) {
				t.Errorf("the startup line is not %s:\n%s", want, line)
			}
		})
	}
}

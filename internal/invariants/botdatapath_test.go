package invariants

import (
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/cruciblelab/crucible-analytic/internal/api"
	"github.com/cruciblelab/crucible-analytic/internal/collector"
	"github.com/cruciblelab/crucible-analytic/internal/panel/web"
)

// The known-bot file has one writer and two readers, and the examples
// name the same file in all three (W4, item 4).
//
// The collector fetches it ([bot_data] path). The panel's setup wizard
// asks whether it has been fetched, and the analytics API labels
// fingerprints with it - both as a top-level bot_data_path. In the panel
// and API examples that key was commented out, so the wizard said "not
// reported to the panel" on every install, and the API looked nowhere.
//
// Decoded into the services' own configuration types rather than grepped:
// a key that sits below a table header belongs to that table and never
// reaches the field - the API example once had it under [[tokens]], and
// a text search would have called that file correct.
//
// release/install_test.go holds the other half: that install.sh moves all
// three into the STATE_DIR it was given.
func TestTheExamplesNameOneBotDataFile(t *testing.T) {
	root := repoRootFromInvariants(t)

	var c collector.Config
	if _, err := toml.DecodeFile(filepath.Join(root, "config.example.toml"), &c); err != nil {
		t.Fatal(err)
	}
	var p web.Config
	if _, err := toml.DecodeFile(filepath.Join(root, "panel.example.toml"), &p); err != nil {
		t.Fatal(err)
	}
	var a api.Config
	if _, err := toml.DecodeFile(filepath.Join(root, "analytics-api.example.toml"), &a); err != nil {
		t.Fatal(err)
	}

	want := c.BotData.Path
	if want == "" {
		t.Fatal("config.example.toml names no [bot_data] path; the collector would have " +
			"nowhere to write the file the other two read")
	}
	for name, got := range map[string]string{
		"panel.example.toml":         p.BotDataPath,
		"analytics-api.example.toml": a.BotDataPath,
	} {
		if got != want {
			t.Errorf("%s: bot_data_path = %q, want %q - the file the collector writes. "+
				"Empty means the key is commented out or sits under a table header",
				name, got, want)
		}
	}
}

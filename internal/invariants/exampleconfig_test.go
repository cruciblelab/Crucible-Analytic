package invariants

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/api"
	"github.com/cruciblelab/crucible-analytic/internal/applier"
	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/collector"
	"github.com/cruciblelab/crucible-analytic/internal/panel/web"
)

// In TOML every key after a table header belongs to that table. So a
// top-level setting written below one is not the setting it looks like -
// it is a member of that table, it decodes into nothing, and the
// decoder says nothing about it.
//
// This has cost this project twice. First panel.example.toml kept
// secret_key under [developer_gate]: release/install.sh generated the
// key, reported "wrote it", the panel started, and the mail account
// could never be saved - one log line, no error. A note was added to
// that file explaining the rule.
//
// The note protected that file and only that file. Found while
// measuring bot detection on a live install:
// analytics-api.example.toml had bot_data_path below [[tokens]], so
// uncommenting the line the file itself tells you to uncomment produced
// a config where the API still logged "bot data not present" while the
// path sat plainly in the file. Moving the same line above the table
// header - changing nothing else - loaded 52 fingerprints.
//
// A rule written as prose in one file protects one file. This is the
// same rule written where a person does not have to remember it.
//
// The two sides the package doc calls for: the field list comes from
// each config struct by reflection, so a new top-level setting is
// covered the day it is added; the file-to-struct pairing is the hand
// list, with a reason each, so a new example config has to be
// registered here before it is trusted.
var exampleConfigs = map[string]struct {
	cfg any
	why string
}{
	"config.example.toml": {collector.Config{},
		"the collector: site_id and mode sit above [network], and everything else is in a table"},
	"beacon.example.toml": {beacon.Config{},
		"the beacon: six top-level settings, including trusted_proxies, which decides whose IP is believed"},
	"analytics-api.example.toml": {api.Config{},
		"the read API: bot_data_path was below [[tokens]] here, which is what this test was written for"},
	"panel.example.toml": {web.Config{},
		"the panel: secret_key and bot_data_path, the pair that failed silently the first time"},
	"upgrader.example.toml": {applier.Config{},
		"the upgrader: schema_admin_dsn and interval_seconds above [logging], [release] and [backup]"},
}

// assignment matches a setting line, commented or not. The commented
// form matters more than the live one: a commented setting is an
// instruction to the reader to uncomment it, so it has to be in a
// position where uncommenting works.
var assignment = regexp.MustCompile(`^\s*#?\s*([A-Za-z_][A-Za-z0-9_]*)\s*=`)

// tableHeader matches [table] and [[array.of.tables]], commented or not.
// A commented header counts: uncommenting a block below it is exactly
// what the file invites, and the keys under it belong to it either way.
var tableHeader = regexp.MustCompile(`^\s*#?\s*\[\[?[A-Za-z0-9_.]+\]\]?\s*$`)

func TestEveryTopLevelSettingComesBeforeTheFirstTable(t *testing.T) {
	root := repoRoot(t)

	// Checked against a list: every example config in the repository has
	// to be registered above, or this fails. A scan that only walked the
	// registered files would pass the day somebody adds a sixth.
	onDisk, err := filepath.Glob(filepath.Join(root, "*.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range onDisk {
		name := filepath.Base(path)
		if _, ok := exampleConfigs[name]; !ok {
			t.Errorf("%s is not registered in exampleConfigs: add it with the struct it decodes "+
				"into and a reason, so its top-level settings are checked too", name)
		}
	}

	for name, spec := range exampleConfigs {
		t.Run(name, func(t *testing.T) {
			top := topLevelKeys(reflect.TypeOf(spec.cfg))
			if len(top) == 0 {
				t.Fatalf("%s decodes into a struct with no top-level settings at all (%s); "+
					"either the pairing is wrong or this test is reading the wrong type",
					name, spec.why)
			}

			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}

			var firstTable string
			var firstTableLine int
			for i, line := range strings.Split(string(data), "\n") {
				if tableHeader.MatchString(line) {
					if firstTable == "" {
						firstTable, firstTableLine = strings.TrimSpace(strings.TrimLeft(line, "# \t")), i+1
					}
					continue
				}
				m := assignment.FindStringSubmatch(line)
				if m == nil || !top[m[1]] || firstTable == "" {
					continue
				}
				t.Errorf("%s:%d: %q is a top-level setting but comes after %s (line %d).\n"+
					"In TOML it decodes as a member of that table and is silently ignored. "+
					"Move it above the first table header.",
					name, i+1, m[1], firstTable, firstTableLine)
			}
		})
	}
}

// topLevelKeys returns the TOML names of the fields that are settings
// rather than tables: anything that is not a struct, and not a slice of
// structs (an array of tables). A []string is a value, so it counts.
func topLevelKeys(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if name == "" || name == "-" {
			continue
		}
		if isTable(f.Type) {
			continue
		}
		out[name] = true
	}
	return out
}

func isTable(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		return true
	case reflect.Slice, reflect.Array:
		return isTable(t.Elem())
	default:
		return false
	}
}

// TestTheRegisteredConfigsCoverEveryExampleFile is the other direction:
// a registered name with no file is a pairing that stopped meaning
// anything, and would leave this test quietly checking four files while
// claiming five.
func TestTheRegisteredConfigsCoverEveryExampleFile(t *testing.T) {
	root := repoRoot(t)
	var missing []string
	for name := range exampleConfigs {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("registered in exampleConfigs but not in the repository: %s", strings.Join(missing, ", "))
	}
}

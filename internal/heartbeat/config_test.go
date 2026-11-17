package heartbeat

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/configreport"
)

// Read - the health page's, the dashboard's and the diagnostic bundle's
// reader, all three open to the owner of one site - never selects the
// config column. Its list is optionalColumns without that one column,
// held here rather than written twice.
func TestReadNeverSelectsConfig(t *testing.T) {
	want := slices.DeleteFunc(slices.Clone(optionalColumns), func(c string) bool { return c == configColumn })
	if !slices.Equal(beatColumns, want) {
		t.Errorf("Read selects %v; it must select every optional column but %s (%v)",
			beatColumns, configColumn, want)
	}
	if !slices.Contains(optionalColumns, configColumn) {
		t.Errorf("%s is not an optional column, so nothing writes it", configColumn)
	}
}

// The column holds the report as JSON; nothing for a process that
// reports nothing, or names a program this build does not know; and every
// value bounded, whatever the process handed over.
func TestConfigJSON(t *testing.T) {
	if got := (&Reporter{}).configJSON(); got != "{}" {
		t.Errorf("no report: %s, want {}", got)
	}
	unknown := &Reporter{config: func() configreport.Report {
		return configreport.Report{Program: "beacon2", Values: map[string]string{"listen_addr": "x"}}
	}}
	if got := unknown.configJSON(); got != "{}" {
		t.Errorf("an unknown program: %s, want {}", got)
	}

	long := strings.Repeat("a", configreport.MaxValueBytes+100)
	r := &Reporter{config: func() configreport.Report {
		return configreport.Report{Program: configreport.Beacon, Values: map[string]string{"sites": long}}
	}}
	var back configreport.Report
	if err := json.Unmarshal([]byte(r.configJSON()), &back); err != nil {
		t.Fatal(err)
	}
	if back.Program != configreport.Beacon {
		t.Errorf("program = %q", back.Program)
	}
	if got := back.Values["sites"]; len(got) > configreport.MaxValueBytes+len("…") {
		t.Errorf("a %d-byte value went into the row unbounded (%d bytes)", len(long), len(got))
	}
}

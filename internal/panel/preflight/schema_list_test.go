package preflight

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// createsPanelTable matches the schema file's table definitions.
var createsPanelTable = regexp.MustCompile(`(?m)^CREATE TABLE IF NOT EXISTS (panel_\w+)`)

// TestPanelTablesMatchesTheSchemaFile.
//
// The wizard's "are the panel tables applied" check works from a list
// written by hand, and a list written by hand drifts. This one had
// drifted by two: panel_owner_claims had been missing since invitations
// were built, and panel_recovery_codes since the day it was added. The
// check reported "all eight present" and meant it.
//
// That is worse than having no check. A deployment missing either table
// passes the wizard, hands over, and then fails at runtime on the one
// page that was supposed to have caught it.
//
// Read from the file rather than from the panel package on purpose:
// preflight does not import panel and must not start - see
// TestPreflightDoesNotImportThePanel. A path is not an import.
func TestPanelTablesMatchesTheSchemaFile(t *testing.T) {
	path := filepath.Join("..", "schema.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var inSchema []string
	for _, m := range createsPanelTable.FindAllStringSubmatch(string(body), -1) {
		inSchema = append(inSchema, m[1])
	}
	// A regexp that matched nothing would agree with an empty list.
	if len(inSchema) < 5 {
		t.Fatalf("only %d panel tables found in %s; the parser is not reading it", len(inSchema), path)
	}

	want := slices.Clone(inSchema)
	got := slices.Clone(panelTables)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the check looks for %v\nthe schema creates %v\n\n"+
			"Add the table to panelTables - and while you are here, add it to KURULUM.md's "+
			"GRANT block too, since a table the panel role cannot reach fails the same way.",
			got, want)
	}
}

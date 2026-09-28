package web

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestTheDiagnosticFileCarriesNoVisitorNumbers is the Health page's rule,
// applied to the file that repeats it.
//
// Stricter than the page's check in two ways, because this leaves the
// machine: every struct type declared in diagnostic.go is read, not a
// list of names that could fall behind, and the JSON names are checked
// as well as the Go ones - the JSON name is the one the reader sees.
// It already did its job once: the first draft of the file carried
// IPTokenKey and Paths, and both are words this check refuses.
func TestTheDiagnosticFileCarriesNoVisitorNumbers(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "diagnostic.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	types := 0
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		structType, ok := spec.Type.(*ast.StructType)
		if !ok {
			return false
		}
		types++
		for _, field := range structType.Fields.List {
			var names []string
			for _, name := range field.Names {
				names = append(names, name.Name)
			}
			if field.Tag != nil {
				tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("json")
				names = append(names, strings.Split(tag, ",")[0])
			}
			for _, name := range names {
				lower := strings.ToLower(name)
				for _, word := range visitorWords {
					if strings.Contains(lower, word) {
						t.Errorf("%s has a field named %q; the diagnostic file leaves this "+
							"machine, and nothing in it may be a number about a visitor",
							spec.Name.Name, name)
					}
				}
			}
		}
		return false
	})
	if types < 10 {
		t.Fatalf("found %d struct types in diagnostic.go; the file has more than that, so "+
			"the scan is reading the wrong file or the wrong nodes", types)
	}
}

// TestTheFileMeasuresTheDiskThePageMeasures: every number the page's
// disk section holds reaches the file, under its own name.
//
// Distinct values in every field, so a field copied into its neighbour
// shows up as a wrong number rather than as a coincidence.
func TestTheFileMeasuresTheDiskThePageMeasures(t *testing.T) {
	page := healthDisk{
		Container:           true,
		DatabaseLocal:       true,
		DatabaseKnown:       true,
		DatabaseBytes:       11,
		UnplacedBackupBytes: 12,
		Error:               "disk-hatasi",
		Filesystems: []healthFilesystem{{
			Paths:         []string{"/var/log/crucible-analytic"},
			Labels:        []string{"etiket, dosyaya girmez"},
			TotalBytes:    21,
			UsedBytes:     22,
			AvailBytes:    23,
			ReservedBytes: 24,
			BackupBytes:   25,
			AtRisk:        true,
			Error:         "fs-hatasi",
		}},
	}
	body, err := json.Marshal(diagnosticDiskFrom(page))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{
		`"container":true`, `"database_local":true`, `"database_known":true`,
		`"database_bytes":11`, `"unplaced_backup_bytes":12`, `"error":"disk-hatasi"`,
		`"directories":["/var/log/crucible-analytic"]`,
		`"total_bytes":21`, `"used_bytes":22`, `"avail_bytes":23`,
		`"reserved_bytes":24`, `"backup_bytes":25`, `"at_risk":true`, `"error":"fs-hatasi"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the file's disk section has no %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "etiket") {
		t.Errorf("a page label reached the file: %s", got)
	}
}

// TestTheAPISectionKeepsItsMeasurement: reachability, the time it took,
// and the error text bounded like any other value leaving the process.
func TestTheAPISectionKeepsItsMeasurement(t *testing.T) {
	got := diagnosticAPIFrom(healthAPI{
		Configured: true,
		Detail:     "dial tcp: connection refused\nsecond line",
		Took:       1500 * time.Millisecond,
	})
	if !got.Configured || got.Reachable {
		t.Errorf("configured/reachable = %v/%v, want true/false", got.Configured, got.Reachable)
	}
	if got.TookMillis != 1500 {
		t.Errorf("took_ms = %d, want 1500", got.TookMillis)
	}
	if strings.Contains(got.Detail, "\n") {
		t.Errorf("the detail kept a newline, so one value could read as two: %q", got.Detail)
	}
}

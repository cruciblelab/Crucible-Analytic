package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What the services say about their configuration (PLAN §W4, item 5).
//
// Two rules, both wired by hand and both invisible to every package test:
// a service whose main leaves out heartbeat.Options.Config reports
// nothing, and the wizard says "not reported" about it for ever; and a
// page that calls heartbeat.ReadConfig puts one customer's collector
// backend on a page another customer's owner can open.

// reportingMains is each service main and the program it must report as.
var reportingMains = map[string]string{
	"collector":     "Collector",
	"beacon":        "Beacon",
	"analytics-api": "API",
	"panel":         "Panel",
}

// TestEveryServiceReportsItsConfig: every heartbeat.Options literal in a
// cmd/ main sets Config, and the function it sets names its own program
// (configreport.Collector in cmd/collector) - a report labelled with
// another program's name would be drawn under that program's keys.
//
// The mains are found from the tree, not from the map: a new service main
// with a heartbeat and no entry here fails rather than going unchecked.
func TestEveryServiceReportsItsConfig(t *testing.T) {
	root := repoRootFromInvariants(t)
	fset := token.NewFileSet()
	dirs, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		names, _ := filepath.Glob(filepath.Join(root, "cmd", dir.Name(), "*.go"))
		var files []*ast.File
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
		}
		for _, file := range files {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isSelectorNode(lit.Type, "heartbeat", "Options") {
					return true
				}
				seen[dir.Name()] = true
				program, want := reportingMains[dir.Name()]
				if !want {
					t.Errorf("%s: cmd/%s writes a heartbeat and is not in reportingMains; say which "+
						"program it reports as", fset.Position(lit.Pos()), dir.Name())
					return true
				}
				var config ast.Expr
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Config" {
							config = kv.Value
						}
					}
				}
				if config == nil {
					t.Errorf("%s: cmd/%s's heartbeat.Options sets no Config - the setup wizard "+
						"would say \"not reported\" about every value it has", fset.Position(lit.Pos()), dir.Name())
					return true
				}
				if !namesProgram(files, config, program) {
					t.Errorf("%s: cmd/%s's Config does not name configreport.%s", fset.Position(config.Pos()),
						dir.Name(), program)
				}
				return true
			})
		}
	}
	for dir := range reportingMains {
		if !seen[dir] {
			t.Errorf("cmd/%s has no heartbeat.Options literal; this check is no longer checking "+
				"what it was written for", dir)
		}
	}
}

// namesProgram reports whether expr - or a function of the same main
// package it calls - mentions configreport.<program>.
func namesProgram(files []*ast.File, expr ast.Expr, program string) bool {
	if mentions(expr, program) {
		return true
	}
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		for _, file := range files {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == id.Name && mentions(fn, program) {
					found = true
				}
			}
		}
		return true
	})
	return found
}

func mentions(n ast.Node, program string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if isSelectorNode(n, "configreport", program) {
			found = true
		}
		return !found
	})
	return found
}

func isSelectorNode(n ast.Node, pkg, name string) bool {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg && sel.Sel.Name == name
}

// configReaders are the files allowed to call heartbeat.ReadConfig, with
// why.
var configReaders = map[string]string{
	"internal/panel/web/configreports.go": "the setup wizard, opened only by somebody who speaks for the whole deployment",
}

// TestOnlyTheWizardReadsConfigReports: no other production file calls
// heartbeat.ReadConfig. The health page, the dashboard and the diagnostic
// bundle are open to the owner of one site and read heartbeat.Read,
// which does not select the column.
func TestOnlyTheWizardReadsConfigReports(t *testing.T) {
	root := repoRootFromInvariants(t)
	fset := token.NewFileSet()
	calls := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.Walk(filepath.Join(root, top), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isSelectorNode(call.Fun, "heartbeat", "ReadConfig") {
					return true
				}
				calls++
				if _, ok := configReaders[rel]; !ok {
					t.Errorf("%s calls heartbeat.ReadConfig. What the services report about their "+
						"configuration names one customer's backend and addresses; only the setup "+
						"wizard reads it (configReaders)", fset.Position(call.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls == 0 {
		t.Error("nothing calls heartbeat.ReadConfig; the wizard is not reading what the services report, " +
			"or this check is not seeing the call")
	}
}

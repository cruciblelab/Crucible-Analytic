package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/logsink"
)

// Every log line that names a site names it with logsink.SiteKey.
//
// # The failure this exists for
//
// The log table has a site column so that a line about one customer's
// site is never shown to another - "a process-level line must never be
// shown to one customer as if it were theirs" is in its schema, and the
// panel's diagnostic file filters on it. The sink moves the attribute
// with the key SiteKey into that column. SiteKey was "site_id", and all
// twenty-four keys in the tree that name a site, in eight files, were
// "site": measured on 2026-09-28, such a line is stored with an empty
// site column and the site in the attribute blob, i.e. as a line about
// nobody's site. The sink's own test wrote the constant, so it passed.
//
// # What is checked
//
// Every key in a log call, or in a slog attribute built anywhere, that
// spells "site" or "site id" in any of the usual ways. Each must be
// SiteKey's own spelling. A key built from the constant itself is not a
// literal and passes on its own. The walk is checked to have found the
// lines it is about, so an extractor that saw nothing cannot pass.
//
// *Bir sütunu dolduran anahtar, yazanların yazdığı anahtar değilse,
// sütun yalnız testlerde doludur.*
func TestEveryLogLineNamesItsSiteTheOneWay(t *testing.T) {
	siteKey := regexp.MustCompile(`(?i)^site[_\-.]?(id)?$`)

	var found, wrong []string
	for _, k := range logKeys(t) {
		if !siteKey.MatchString(k.key) {
			continue
		}
		found = append(found, k.where)
		if k.key != logsink.SiteKey {
			wrong = append(wrong, k.where+" writes "+strconv.Quote(k.key))
		}
	}
	sort.Strings(wrong)
	if len(wrong) > 0 {
		t.Errorf("these log lines name a site with a key the log table does not read, "+
			"so they are stored as lines about no site (want %q):\n  %s",
			logsink.SiteKey, strings.Join(wrong, "\n  "))
	}
	// The walk has to have seen the lines this is about. Ten is well
	// under today's count and far above zero.
	if len(found) < 10 {
		t.Errorf("only %d site-naming log keys found; the extractor is not seeing the log calls", len(found))
	}
}

// Every key a log line is written under is classified for leaving the
// machine, and every classified key is one some line is written under.
//
// # Why both directions
//
// The panel's diagnostic file carries recent log lines to whoever
// supports the deployment, and logging.ExportRuleFor decides what each
// attribute does there: goes, goes masked, or never goes. An unclassified
// key is withheld, which is the safe direction - so one side of this test
// is not about privacy but about the file staying useful: a new key is a
// question for whoever adds it (is this about a person?) rather than a
// field that quietly never arrives. The other side is the list's honesty:
// a classification for a key no code writes any more describes code that
// is gone.
func TestEveryLogKeyIsClassifiedForLeavingTheMachine(t *testing.T) {
	written := map[string]string{}
	for _, k := range logKeys(t) {
		if _, ok := written[k.key]; !ok {
			written[k.key] = k.where
		}
	}
	var unclassified, stale []string
	for key, where := range written {
		if logging.ExportRuleFor(key) == logging.ExportUnknown {
			unclassified = append(unclassified, strconv.Quote(key)+" ("+where+")")
		}
	}
	for _, key := range logging.ExportClassified() {
		if _, ok := written[key]; !ok {
			stale = append(stale, strconv.Quote(key))
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("log keys with no rule for the diagnostic file; add each to logging's exportRules, "+
			"asking whether its value can be about a person:\n  %s", strings.Join(unclassified, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("classified keys no log line is written under any more; remove them:\n  %s",
			strings.Join(stale, "\n  "))
	}
	// The two rules the owner's decision names, as themselves.
	if logging.ExportRuleFor(logging.KeyClaim) != logging.ExportNever {
		t.Errorf("%q must never leave the machine", logging.KeyClaim)
	}
	if logging.ExportRuleFor(logging.KeyPeer) != logging.ExportAddress {
		t.Errorf("%q must leave only masked", logging.KeyPeer)
	}
}

// logKey is one attribute key written by a log call, and where.
type logKey struct {
	key   string
	where string
}

// logMethods are the slog.Logger methods that take attributes, and how
// many leading arguments each has before them.
var logMethods = map[string]int{
	"Debug": 1, "Info": 1, "Warn": 1, "Error": 1,
	"DebugContext": 2, "InfoContext": 2, "WarnContext": 2, "ErrorContext": 2,
	"Log": 3, "LogAttrs": 3, "With": 0,
}

// attrConstructors are the slog functions that build one attribute from a
// key.
var attrConstructors = map[string]bool{
	"String": true, "Int": true, "Int64": true, "Uint64": true, "Float64": true,
	"Bool": true, "Time": true, "Duration": true, "Any": true, "Group": true,
}

// keyConstants are the named keys the tree writes attributes under, by
// the constant's name: a key written as logging.KeyPeer is the key
// "peer", and a walk that saw only literals would miss the one that
// matters most to the diagnostic file.
var keyConstants = map[string]string{
	"KeyClaim":     logging.KeyClaim,
	"KeyPeer":      logging.KeyPeer,
	"KeyVerdict":   logging.KeyVerdict,
	"KeyReason":    logging.KeyReason,
	"KeySource":    logging.KeySource,
	"CategoryKey":  logging.CategoryKey,
	"SiteKey":      logsink.SiteKey,
	"OperationKey": logsink.OperationKey,
}

// attrKey is a plausible attribute key, which a message is not.
var attrKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// logKeys walks every non-test source file for the keys of log
// attributes: key-value pairs in a log call, and the first argument of
// slog's attribute constructors wherever they appear.
//
// A call is a log call by method name. A package-qualified call is
// skipped unless the package is slog itself - http.Error has a method's
// name and a message where a key would be.
func logKeys(t *testing.T) []logKey {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()
	var keys []logKey
	for _, dir := range sourceRoots {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			imported := map[string]bool{}
			for _, imp := range file.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				name := p[strings.LastIndex(p, "/")+1:]
				if imp.Name != nil {
					name = imp.Name.Name
				}
				imported[name] = true
			}
			rel, _ := filepath.Rel(root, path)
			at := func(n ast.Node) string {
				return filepath.ToSlash(rel) + ":" + strconv.Itoa(fset.Position(n.Pos()).Line)
			}
			keyOf := func(e ast.Expr) (string, bool) {
				if k, ok := stringLit(e); ok {
					return k, attrKey.MatchString(k)
				}
				switch v := e.(type) {
				case *ast.Ident:
					k, ok := keyConstants[v.Name]
					return k, ok
				case *ast.SelectorExpr:
					k, ok := keyConstants[v.Sel.Name]
					return k, ok
				}
				return "", false
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, qualified := sel.X.(*ast.Ident)
				qualified = qualified && imported[pkg.Name]
				if qualified && pkg.Name == "slog" && attrConstructors[sel.Sel.Name] {
					if len(call.Args) > 0 {
						if k, ok := keyOf(call.Args[0]); ok {
							keys = append(keys, logKey{k, at(call)})
						}
					}
					return true
				}
				skip, ok := logMethods[sel.Sel.Name]
				if !ok || (qualified && pkg.Name != "slog") || len(call.Args) <= skip {
					return true
				}
				args := call.Args[skip:]
				for i := 0; i < len(args); {
					if k, ok := keyOf(args[i]); ok {
						keys = append(keys, logKey{k, at(args[i])})
						i += 2 // the key and its value
						continue
					}
					i++ // an attribute, or a spread of them
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

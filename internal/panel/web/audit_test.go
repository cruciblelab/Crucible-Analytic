package web

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// Every audit entry says who acted, before the database has to ask.
//
// # What went wrong
//
// panel_audit_log.actor_kind has a check constraint naming the kinds it
// accepts. Four call sites built an AuditEntry without setting the
// field, so the column got the empty string, so the database refused the
// row - and every one of those calls discarded the error.
//
// The four were the failed sign-in and the three rate-limit refusals.
// The audit log therefore recorded 1075 successful sign-ins on the
// development database and not one failure, which is precisely the half
// worth having.
//
// # Why a source check and not only the integration test
//
// There is an integration test now, and it would catch this happening
// again to the sign-in path. It would not catch it happening to the
// fifth call site somebody adds next year, because that call site will
// have no test of its own - the four that broke did not either. The
// mistake is not a behaviour, it is an omission, and an omission is
// visible in the source and invisible everywhere else until a row is
// refused on somebody's server.
//
// *Bir isteğin taşıdığı her alan, onu yazana verilmiş bir yetkidir.*

// auditCalls finds every AuditEntry literal handed to the two helpers.
func auditCalls(t *testing.T) (viaAudit, viaAuditFor []*ast.CompositeLit) {
	t.Helper()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate this package")
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Dir(self), func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, ok := pkgs["web"]
	if !ok {
		t.Fatal("this directory did not parse as package web, so the check read nothing")
	}

	for _, f := range pkg.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.CompositeLit)
				if !ok || !isAuditEntry(lit.Type) {
					continue
				}
				switch sel.Sel.Name {
				case "audit":
					viaAudit = append(viaAudit, lit)
				case "auditFor":
					viaAuditFor = append(viaAuditFor, lit)
				}
			}
			return true
		})
	}
	return viaAudit, viaAuditFor
}

// isAuditEntry reports whether a literal's type is panel.AuditEntry.
func isAuditEntry(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "AuditEntry" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "panel"
}

// hasField reports whether a composite literal sets the named field.
func hasField(lit *ast.CompositeLit, name string) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == name {
			return true
		}
	}
	return false
}

// TestEveryAuditEntryNamesItsActor.
func TestEveryAuditEntryNamesItsActor(t *testing.T) {
	viaAudit, viaAuditFor := auditCalls(t)
	if len(viaAudit) == 0 && len(viaAuditFor) == 0 {
		t.Fatal("no audit entries were found in this package, so the check read nothing. " +
			"If the helpers were renamed, rename them here too")
	}

	for _, lit := range viaAudit {
		if !hasField(lit, "ActorKind") {
			t.Error("an entry passed to s.audit does not set ActorKind.\n" +
				"The column's check constraint refuses the empty string, so the row " +
				"is never written - and the helper's whole job is that the failure is " +
				"logged rather than lost. Set it, or use s.auditFor with a principal.")
		}
	}
}

// TestAnEntryWithAPrincipalDoesNotAlsoSetTheActor is the other
// direction.
//
// auditFor overwrites ActorKind, ActorID and ActorLabel from the
// principal it was given. A literal that sets them anyway reads like it
// decides them, and does not - which is the kind of line somebody later
// changes expecting an effect it cannot have.
func TestAnEntryWithAPrincipalDoesNotAlsoSetTheActor(t *testing.T) {
	_, viaAuditFor := auditCalls(t)
	if len(viaAuditFor) == 0 {
		t.Fatal("no entries go through s.auditFor, so half of this check read nothing")
	}

	for _, lit := range viaAuditFor {
		for _, field := range []string{"ActorKind", "ActorID", "ActorLabel"} {
			if hasField(lit, field) {
				t.Errorf("an entry passed to s.auditFor sets %s, which the principal "+
					"overwrites. The line has no effect and reads as though it does", field)
			}
		}
	}
}

// failingAuditStore refuses every write, which is what a database with a
// constraint the entry violates does.
type failingAuditStore struct{ err error }

func (f failingAuditStore) Record(context.Context, panel.AuditEntry) error { return f.err }
func (f failingAuditStore) RecordFor(context.Context, panel.Principal, panel.AuditEntry) error {
	return f.err
}

// TestARefusedAuditWriteIsLoudRatherThanLost.
//
// # What goes wrong without it
//
// The helper's only job is this. It does not change what the request
// answers and it does not stop anything - all it does is turn a silent
// loss into a line somebody can find. Delete the logging and every test
// in this repository still passes, which is exactly how the original
// defect survived four call sites and several phases.
//
// # Why this can be written at all
//
// Because writeAudit takes the store. Reaching this branch through the
// real one would mean a database that refuses a write - the very
// condition that went unnoticed for months because nobody could
// conveniently produce it.
func TestARefusedAuditWriteIsLoudRatherThanLost(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	db := failingAuditStore{err: errors.New("violates check constraint")}

	writeAudit(context.Background(), db, log, panel.AuditEntry{
		Action: panel.ActionLoginFailed, ActorKind: panel.PrincipalAnonymous,
	})

	got := buf.String()
	if got == "" {
		t.Fatal("a refused audit write produced no log line at all, which is the " +
			"defect this helper exists to end")
	}
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("the refusal was not logged at error level:\n%s", got)
	}
	for _, want := range []string{panel.ActionLoginFailed, "violates check constraint"} {
		if !strings.Contains(got, want) {
			t.Errorf("the log line does not say %q, so nobody reading it knows what "+
				"was lost:\n%s", want, got)
		}
	}
}

// TestASuccessfulAuditWriteSaysNothing.
//
// The other half, and it matters: a helper that logged on every write
// would put one line per audited action into the log, and a log that
// noisy is one the error above disappears into.
func TestASuccessfulAuditWriteSaysNothing(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	writeAudit(context.Background(), failingAuditStore{}, log, panel.AuditEntry{
		Action: panel.ActionLoginSucceeded, ActorKind: panel.PrincipalUser,
	})

	if buf.Len() != 0 {
		t.Errorf("a successful write logged anyway:\n%s", buf.String())
	}
}

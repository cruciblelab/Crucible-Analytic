package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every write to panel_site_members either asks the last-owner question
// or cannot possibly reduce the owner count.
//
// # The jewel this guards
//
// PLAN.md §3.5, J1: *a site is never left with zero owners.* It is the
// first of the five because it is the only one whose violation locks
// everybody out of their own data with nothing left to press.
//
// # Why a write invariant, when the reads already have one
//
// membershipreads_test.go holds the other half of this table: every
// read filters expired memberships or is listed with a reason. That
// test was written after C9.3. The write side never got one - and the
// write side is where C9.1c happened.
//
// C9.1c is worth restating, because it is the exact failure this file
// exists to make impossible a second time. Access.CanAssign had been
// correct for months: nobody may hand out a role above their own. What
// nothing asked was *whose* role was being touched. Three separate,
// individually reasonable paths - adding a member, redeeming an invite,
// an administrator editing a row - each wrote this table, and two of
// them could leave a site with no owner at all. Each step was legal.
// The composition was not.
//
// It was found because somebody asked "who writes to this table" by
// hand, once, while preparing C9.2 - which was about to add a fourth
// writer. Asked by hand, it worked once. That is what this file is for.
//
// # Why there is no list of excused names
//
// PLAN.md's own rule, learned from the shared-row lock invariant: *when
// an exemption can be stated as conditions, do not write it as a list of
// names.* A name on a list is a claim about a function on the day
// somebody typed it. A condition is re-checked on every run.
//
// Two writers here genuinely do not need the check, and both for the
// same structural reason: their statements cannot lower the number of
// owners. Redeeming an invite inserts and, on conflict, does nothing -
// so it can never overwrite the role of somebody who is already there.
// Redeeming an owner claim inserts and, on conflict, sets the role to
// owner - so the count can only go up. Neither reason is a promise about
// intent; both are readable off the SQL, and both stop being true the
// moment the SQL changes, which is precisely when the check should come
// back.
//
// The discriminator is real rather than convenient: AddMember's own
// conflict clause is
//
//	ON CONFLICT (site_id, user_id) DO UPDATE
//	   SET role = EXCLUDED.role, expires_at = EXCLUDED.expires_at
//
// which can demote an owner, so AddMember is not monotone and does ask
// the question. The rule separates the three guarded writers from the
// two safe ones without being told which is which.
//
// *Bir kuralın verilen yarısını sınayan bir süit, alınan yarısını
// sınamıyordur.*

// ownerGuardMarker is what a guarded write must contain.
//
// The call, not the condition: a writer that reimplements the owner
// census inline is exactly the drift this guards against, so only going
// through the shared helper counts.
const ownerGuardMarker = "lastOwnerIs("

// membershipWrite finds a statement that writes the membership table,
// capturing the verb so an INSERT can be told from an UPDATE or DELETE.
var membershipWrite = regexp.MustCompile(
	`(?is)\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+` + membershipTable + `\b`)

// conflictClause finds every ON CONFLICT in a function.
var conflictClause = regexp.MustCompile(`(?is)ON\s+CONFLICT\b`)

// safeConflictAction finds the conflict actions that cannot lower the
// owner count: doing nothing, or setting the role to the owner literal.
//
// Deliberately literal about the role. A clause that assigns from
// EXCLUDED takes whatever the caller passed, which is how an
// administrator once demoted the only owner.
var safeConflictAction = regexp.MustCompile(
	`(?is)ON\s+CONFLICT\s*(?:\([^)]*\))?\s*DO\s+(?:NOTHING|UPDATE\s+SET\s+role\s*=\s*'owner')`)

// writeVerbs returns the verbs of every membership write in src.
func writeVerbs(src string) []string {
	var out []string
	for _, m := range membershipWrite.FindAllStringSubmatch(src, -1) {
		out = append(out, strings.ToUpper(strings.Fields(m[1])[0]))
	}
	return out
}

// ownerMonotone reports whether every write in src is one the owner
// count cannot survive less of.
//
// Two conditions, both required:
//
//   - every write is an INSERT (an UPDATE can change a role, a DELETE
//     can take the last owner away), and
//   - every ON CONFLICT clause present is one of the safe actions.
//
// The second condition counts rather than merely finding one, because a
// function holding two inserts - one safe, one not - would otherwise
// pass on the strength of the safe one.
func ownerMonotone(src string) bool {
	for _, verb := range writeVerbs(src) {
		if verb != "INSERT" {
			return false
		}
	}
	return len(conflictClause.FindAllString(src, -1)) ==
		len(safeConflictAction.FindAllString(src, -1))
}

func TestEveryMembershipWriteAsksTheLastOwnerQuestionOrCannotLowerTheCount(t *testing.T) {
	dir := filepath.Join(repoRootFromInvariants(t), "internal", "panel")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var guarded, monotone, bare []string

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// The whole function, because the guard and the statement
			// are separate syntax nodes and what matters is whether one
			// runs before the other reaches the database.
			src := nodeSource(t, fset, dir, fn)
			if len(writeVerbs(src)) == 0 {
				continue
			}
			switch {
			case strings.Contains(src, ownerGuardMarker):
				guarded = append(guarded, fn.Name.Name)
			case ownerMonotone(src):
				monotone = append(monotone, fn.Name.Name)
			default:
				bare = append(bare, fn.Name.Name)
			}
		}
	}

	// Vacuity, in both directions. A detector that finds nothing reports
	// the same silence as a codebase with nothing wrong, and the whole
	// point of this file is a path nobody looked at.
	if len(guarded) == 0 {
		t.Fatalf("no write of %s calls %s at all.\nEither the guard was renamed "+
			"or this test is looking in the wrong place - and if it is, it would have "+
			"passed whatever the code did.", membershipTable, ownerGuardMarker)
	}
	if len(monotone) == 0 {
		t.Fatalf("no write of %s was classified as unable to lower the owner count.\n"+
			"Two used to be (redeeming an invite, redeeming an owner claim). If their "+
			"SQL changed, the exemption this test grants them is describing code that "+
			"no longer exists; if the pattern stopped matching, every future writer "+
			"gets excused by accident.", membershipTable)
	}

	for _, name := range bare {
		t.Errorf("%s writes %s without calling %s, and its statements can lower the "+
			"number of owners.\n"+
			"Either take the guard - the shared helper, inside the same transaction "+
			"that writes - or change the statement so it cannot demote or remove an "+
			"owner (insert with a conflict action that does nothing, or one that sets "+
			"the role to the owner literal).\n"+
			"A site with no owners cannot be repaired from the panel by anybody: "+
			"the page that hands out ownership is the one nobody can reach.",
			name, membershipTable, ownerGuardMarker)
	}
}

//go:build integration

// Every setting change is an operation, and the record has to be usable
// an hour later by somebody who was not there.
//
// The plan's own emphasis, and the field it singles out: *son alan en
// önemlisi. "Bir şeyi ayarlarken hata olmuş" ancak yarım uygulanmış bir
// değişiklik yarım uygulanmış olarak kaydedilirse cevaplanabilir.*
//
// So the assertions here are about what somebody can find out from the
// row afterwards, not about the row existing.
package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

type opRow struct {
	id         string
	action     string
	target     string
	outcome    string
	errorChain string
	steps      string
	before     *string
	after      *string
	rolledBack *bool
	actorLabel string
	auditID    *int64
	finished   bool
}

// lastOperation reads the most recent operation for a target.
func lastOperation(t *testing.T, store *panel.Store, target string) opRow {
	t.Helper()
	var r opRow
	var finishedAt *string
	err := store.Pool().QueryRow(context.Background(), `
		SELECT id, action, target, outcome, error_chain, steps::text,
		       before_value::text, after_value::text, rolled_back, actor_label,
		       audit_id, finished_at::text
		FROM panel_operations WHERE target = $1
		ORDER BY started_at DESC LIMIT 1`, target).
		Scan(&r.id, &r.action, &r.target, &r.outcome, &r.errorChain, &r.steps,
			&r.before, &r.after, &r.rolledBack, &r.actorLabel, &r.auditID, &finishedAt)
	if err != nil {
		t.Fatalf("reading the operation for %s: %v", target, err)
	}
	r.finished = finishedAt != nil
	return r
}

func cleanOperations(t *testing.T, store *panel.Store) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(), `DELETE FROM panel_operations`)
	})
}

// TestASuccessfulChangeIsRecordedWithBothValues.
//
// "Kim saklama süresini 3650 yaptı" is asked months later, by which
// point the only thing anybody remembers is that it used to be something
// else. A record with only the new value cannot answer it.
func TestASuccessfulChangeIsRecordedWithBothValues(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleOwner)
	cleanOperations(t, store)

	status, body := postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(unguardedKey)},
		"deger":   {"Operasyon Testi"},
	})
	if status != http.StatusOK {
		t.Fatalf("saving answered %d: %s", status, noticeOf(body))
	}

	op := lastOperation(t, store, string(unguardedKey))

	if !strings.HasPrefix(op.id, "op-") {
		t.Errorf("the operation id is %q, which is not the minted shape", op.id)
	}
	if op.outcome != string(panel.OutcomeSucceeded) {
		t.Errorf("outcome = %q", op.outcome)
	}
	if !op.finished {
		t.Error("the operation was never closed; an operation with no end is the interesting case and this was not one")
	}
	if op.after == nil || !strings.Contains(*op.after, "Operasyon Testi") {
		t.Errorf("after_value does not carry what was written: %v", op.after)
	}
	if op.before == nil {
		t.Error("before_value was not recorded; without it nobody can tell what changed")
	}
	if op.rolledBack != nil {
		t.Errorf("rolled_back = %v on a success; it applies to nothing here and must stay null", *op.rolledBack)
	}
	if !strings.Contains(op.steps, "uygula") {
		t.Errorf("the steps do not record the work: %s", op.steps)
	}
	if op.actorLabel == "" {
		t.Error("the operation records no actor")
	}
}

// TestARefusalIsRecordedAsARefusalAndNotAsAFault.
//
// Both end without the change being made, and they need different
// reactions. A refusal is the system working as designed; burying those
// among genuine faults is how a real one gets missed in a list of them.
func TestARefusalIsRecordedAsARefusalAndNotAsAFault(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleViewer)
	cleanOperations(t, store)

	status, _ := postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(unguardedKey)},
		"deger":   {"izleyici yazamaz"},
	})
	if status == http.StatusOK {
		t.Fatal("a viewer's write was accepted")
	}

	op := lastOperation(t, store, string(unguardedKey))
	if op.outcome != string(panel.OutcomeRefused) {
		t.Errorf("outcome = %q, want %q — a refusal is not a fault",
			op.outcome, panel.OutcomeRefused)
	}
	if op.errorChain == "" {
		t.Error("no error chain was recorded, so nobody can tell what refused it")
	}
}

// TestAFailureRecordsWhetherAnythingWasLeftBehind.
//
// The field the plan singles out. "Something went wrong while setting
// it" is only answerable if a half-applied change is recorded as
// half-applied - and the honest answer here is "nothing was applied",
// which has to be *recorded* rather than inferred by whoever reads it.
func TestAFailureRecordsWhetherAnythingWasLeftBehind(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleOwner)
	cleanOperations(t, store)
	restoreGlobal(t, store, panel.KeyLogArchiveAfterDays)

	status, _ := postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {"logs.archive_after_days"},
		"deger":   {"999999"},
	})
	if status == http.StatusOK {
		t.Fatal("a value far outside its bounds was accepted")
	}

	op := lastOperation(t, store, "logs.archive_after_days")
	if op.rolledBack == nil {
		t.Fatal("rolled_back is null on a failure; the reader is left to guess whether the change survived")
	}
	if *op.rolledBack {
		t.Error("rolled_back = true, but nothing was ever applied to undo")
	}
	// The whole chain, not its last link: the innermost cause is the one
	// that names the fix.
	if !strings.Contains(op.errorChain, "3650") {
		t.Errorf("the error chain does not carry the bound that was crossed: %q", op.errorChain)
	}
}

// TestTheLogLinesCarryTheOperationId.
//
// The coupling that made B1 and B2 one phase, end to end: an operation
// runs, and the lines it produced can be found by its id alone.
//
// Without this the streaming window D4b needs can only be "everything
// that happened while you waited", which is noise - and noise is what
// teaches people to click through without reading.
func TestTheLogLinesCarryTheOperationId(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleOwner)
	cleanOperations(t, store)

	status, _ := postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(unguardedKey)},
		"deger":   {"korelasyon testi"},
	})
	if status != http.StatusOK {
		t.Fatalf("saving answered %d", status)
	}

	op := lastOperation(t, store, string(unguardedKey))
	if op.id == "" {
		t.Fatal("no operation id")
	}

	// The panel's own logger in these tests writes to a discard handler,
	// so no row is expected here - what is asserted is that the id is
	// well-formed and unique, which is what a log line would carry.
	//
	// The line-to-column path itself is measured in
	// logsink.TestTheOperationIdAndSiteBecomeColumns, against a real
	// table. Splitting it that way keeps each test able to fail for one
	// reason.
	if !strings.HasPrefix(op.id, "op-") || len(op.id) != 19 {
		t.Errorf("the id is %q; the minted shape is op- plus 16 hex characters", op.id)
	}

	// A second operation gets a different id. An id that repeated would
	// merge two operations' lines into one window.
	status, _ = postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(unguardedKey)},
		"deger":   {"ikinci"},
	})
	if status != http.StatusOK {
		t.Fatalf("the second save answered %d", status)
	}
	second := lastOperation(t, store, string(unguardedKey))
	if second.id == op.id {
		t.Error("two operations share an id")
	}
}

// TestASuccessfulChangeIsTiedToItsAuditEntry.
//
// panel_operations.audit_id had been null on every row since the column
// was created. Operation.LinkAudit existed, was correct, and could not
// be called: the audit write returned only an error, so the id it would
// need did not exist anywhere a caller could reach.
//
// Nothing failed. The column was simply always empty, and the only way
// to notice was to ask who calls LinkAudit - which is what a deadcode
// scan does, and what no test here had been doing.
//
// The two records answer different questions on purpose (see
// internal/panel/operations.go). This link is what lets somebody holding
// one find the other: from "a setting was changed at 14:02" to "and here
// is every step of what happened while it was".
func TestASuccessfulChangeIsTiedToItsAuditEntry(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleOwner)
	cleanOperations(t, store)

	status, body := postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(unguardedKey)},
		"deger":   {"denetim bagi"},
	})
	if status != http.StatusOK {
		t.Fatalf("saving answered %d: %s", status, noticeOf(body))
	}

	op := lastOperation(t, store, string(unguardedKey))
	if op.auditID == nil {
		t.Fatal("audit_id is null on a successful change; the operation record cannot be " +
			"tied back to the audit entry, which is the whole reason the column is there")
	}

	// And it points at the right row, not merely at a row.
	var action, target string
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT action, target FROM panel_audit_log WHERE id = $1`, *op.auditID).
		Scan(&action, &target); err != nil {
		t.Fatalf("the audit entry %d does not exist: %v", *op.auditID, err)
	}
	if target != string(unguardedKey) {
		t.Errorf("the operation is linked to an audit entry for %q, not %q",
			target, unguardedKey)
	}
	if action != panel.ActionSettingChanged {
		t.Errorf("the linked audit entry records %q", action)
	}
}

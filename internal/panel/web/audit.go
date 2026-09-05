package web

import (
	"context"
	"log/slog"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// Writing an audit entry without throwing the failure away.
//
// # The defect this exists because of
//
// Twenty call sites wrote `_ = s.Store.Record(...)`, and the discard was
// deliberate and correct in intent: a failed audit write must not fail
// the request somebody made. What it also did was hide the write
// failing at all.
//
// Four of those twenty passed no ActorKind. The column has a check
// constraint naming the kinds, an empty string is not one of them, and
// every one of those rows was refused by the database. Measured on a
// development database: 1075 `login.succeeded` entries, and not a single
// `login.failed` or `login.throttled` from the sign-in path - which is
// to say the audit log had never recorded a failed sign-in in its life,
// with every test green throughout.
//
// The entries missing were exactly the ones an audit log is opened for.
// Somebody trying addresses against the door leaves no trace; somebody
// getting in leaves 1075.
//
// So the discard is gone and the failure is logged instead. Same
// behaviour for the request - it still does not fail - and the silence
// is what changed.
//
// # Why the writing is a function rather than only a method
//
// Because "it logs the failure" is the entire point, and a method
// reaching s.Store cannot be shown to do it: the test would need a
// database that refuses a write. writeAudit takes the store, so a fake
// that fails is three lines. See stores.go - this is the same seam the
// five sections use, and the first defect after it landed is the one it
// paid for.

// audit writes an entry and logs a failure rather than discarding it.
//
// It returns nothing on purpose: no caller should branch on whether the
// log was written, because there is nothing useful for a handler to do
// about it, and a returned error is one somebody will discard again.
func (s *Server) audit(ctx context.Context, e panel.AuditEntry) {
	writeAudit(ctx, s.auditLog(), s.logger(), e)
}

// auditFor is audit with the actor filled in from a principal.
func (s *Server) auditFor(ctx context.Context, p panel.Principal, e panel.AuditEntry) {
	writeAuditFor(ctx, s.auditLog(), s.logger(), p, e)
}

func writeAudit(ctx context.Context, db auditStore, log *slog.Logger, e panel.AuditEntry) {
	if err := db.Record(ctx, e); err != nil {
		// Error, not Warn. A missing audit entry is a missing answer to
		// "what happened", asked later by somebody who has no other
		// source for it.
		log.Error("panel: audit entry not written",
			"err", err, "action", e.Action, "actor_kind", string(e.ActorKind))
	}
}

func writeAuditFor(ctx context.Context, db auditStore, log *slog.Logger,
	p panel.Principal, e panel.AuditEntry) {

	if err := db.RecordFor(ctx, p, e); err != nil {
		log.Error("panel: audit entry not written",
			"err", err, "action", e.Action, "actor_kind", string(p.Kind))
	}
}

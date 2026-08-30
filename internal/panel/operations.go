package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// An operation is what happened while somebody was changing something.
//
// # Why this is not the audit log
//
// panel_audit_log answers *who did what*: short, legally meaningful,
// kept for a long time, and nothing in this schema may delete a row of
// it. This answers *what happened while they did it* - a different
// question, asked by a different person, usually within the hour,
// usually because a customer said "bir şeyi ayarlarken hata olmuş".
//
// Keeping them apart is what stops diagnostic detail from crowding out
// the record that has to survive.
//
// # The id exists before the work does
//
// Minted in Go rather than by the database, because the whole point is
// to attach it to log lines the operation is *about to* produce. A
// database-assigned id would not exist until the first write, by which
// time the interesting lines have already been emitted without it.

// Operation is a handle on one recorded change.
//
// Not safe for concurrent use by design: an operation is one person
// doing one thing, and a handle two goroutines shared would be recording
// two operations as one.
type Operation struct {
	store *Store
	id    string
	steps []operationStep
}

type operationStep struct {
	At   time.Time `json:"at"`
	Step string    `json:"step"`
	OK   bool      `json:"ok"`
	Note string    `json:"note,omitempty"`
}

// Outcome is how an operation ended.
type Outcome string

const (
	// OutcomeSucceeded means every step did what it was asked.
	OutcomeSucceeded Outcome = "succeeded"
	// OutcomeFailed means it did not, and rolled_back then says whether
	// anything was left behind.
	OutcomeFailed Outcome = "failed"
	// OutcomeRefused means the system declined before doing anything -
	// a missing capability, a wrong password, a value out of bounds.
	//
	// Its own outcome rather than a failure, because the two need
	// different reactions: a refusal is the system working, and burying
	// it among failures is how a genuine fault gets missed in a list of
	// them.
	OutcomeRefused Outcome = "refused"
)

// BeginOperation opens a record and returns it with its id.
//
// The id is usable immediately - that is the point - so a caller can
// attach it to every log line the work produces before the work starts.
func (s *Store) BeginOperation(ctx context.Context, a Access, action, target, site string) (*Operation, error) {
	id, err := newOperationID()
	if err != nil {
		return nil, err
	}

	var actorID *int64
	if a.Principal.UserID != 0 {
		uid := a.Principal.UserID
		actorID = &uid
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO panel_operations (id, action, target, site_id, actor_kind, actor_id, actor_label)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, action, target, site,
		string(a.Principal.Kind), actorID, a.Principal.Label)
	if err != nil {
		return nil, fmt.Errorf("panel: begin operation: %w", err)
	}
	return &Operation{store: s, id: id}, nil
}

// ID is the correlation id to attach to log lines.
func (o *Operation) ID() string {
	if o == nil {
		return ""
	}
	return o.id
}

// Step records that something was attempted and how it went.
//
// Buffered in memory and written when the operation finishes, rather
// than one UPDATE per step. An operation that fails half way is exactly
// the case this table exists for, so Finish is called on every path -
// including the failing ones - and that is where the steps land.
func (o *Operation) Step(step string, ok bool, note string) {
	if o == nil {
		return
	}
	o.steps = append(o.steps, operationStep{At: time.Now().UTC(), Step: step, OK: ok, Note: note})
}

// Values records what the setting was and what it became.
func (o *Operation) Values(ctx context.Context, before, after any) {
	if o == nil {
		return
	}
	b, errB := json.Marshal(before)
	a, errA := json.Marshal(after)
	if errB != nil || errA != nil {
		o.Step("degerler kaydedilemedi", false, "")
		return
	}
	if _, err := o.store.pool.Exec(ctx,
		`UPDATE panel_operations SET before_value = $2, after_value = $3 WHERE id = $1`,
		o.id, b, a); err != nil {
		o.Step("degerler kaydedilemedi", false, err.Error())
	}
}

// LinkAudit ties this operation to the audit entry it belongs to.
func (o *Operation) LinkAudit(ctx context.Context, auditID int64) {
	if o == nil {
		return
	}
	if _, err := o.store.pool.Exec(ctx,
		`UPDATE panel_operations SET audit_id = $2 WHERE id = $1`, o.id, auditID); err != nil {
		o.Step("denetim kaydina baglanamadi", false, err.Error())
	}
}

// Finish closes the record.
//
// rolledBack is three-state on purpose: true undone, false left
// standing, nil not applicable because nothing had been applied yet.
// Collapsing nil into false would claim a change was left behind when
// none was ever made, which is the wrong answer to the one question this
// table exists to answer.
//
// The error chain is stored whole rather than as its last link. This
// project's store errors are wrapped deliberately, and the innermost
// cause is usually the one that names the fix - "permission denied for
// table X" tells somebody what to do; "could not save the setting" does
// not.
func (o *Operation) Finish(ctx context.Context, outcome Outcome, err error, rolledBack *bool) error {
	if o == nil {
		return nil
	}
	steps, marshalErr := json.Marshal(o.steps)
	if marshalErr != nil {
		steps = []byte("[]")
	}

	chain := ""
	if err != nil {
		chain = errorChain(err)
	}

	if _, execErr := o.store.pool.Exec(ctx, `
		UPDATE panel_operations
		SET finished_at = now(), outcome = $2, error_chain = $3, steps = $4, rolled_back = $5
		WHERE id = $1`,
		o.id, string(outcome), chain, steps, rolledBack); execErr != nil {
		return fmt.Errorf("panel: finish operation %s: %w", o.id, execErr)
	}
	return nil
}

// errorChain unwraps an error into one line per link.
//
// Whole rather than last, because the links say different things: the
// outer one names what the panel was doing and the inner one names why
// the database said no.
func errorChain(err error) string {
	var out []byte
	for depth := 0; err != nil && depth < 16; depth++ {
		if depth > 0 {
			out = append(out, '\n')
		}
		out = append(out, err.Error()...)
		unwrapped := unwrap(err)
		if unwrapped == nil || unwrapped.Error() == err.Error() {
			break
		}
		err = unwrapped
	}
	return string(out)
}

func unwrap(err error) error {
	u, ok := err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	return u.Unwrap()
}

// newOperationID mints an id that is unguessable and short enough to
// read out loud.
//
// Random rather than sequential: the id reaches a log line the customer
// can see, and a counter would tell them how many operations the
// deployment has ever run - a small leak, and one there is no reason to
// accept for a value that only has to be unique.
func newOperationID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("panel: mint operation id: %w", err)
	}
	return "op-" + hex.EncodeToString(b[:]), nil
}

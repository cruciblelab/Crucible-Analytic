// Package devgate is a second password in front of the few settings
// that carry legal weight.
//
// # What it is for
//
// The panel password answers "who are you". This one answers a
// different question: "are you entitled to make *this* change". They
// have to be separate keys, because otherwise everyone who can reach the
// panel - the customer, the customer's intern, whoever picked up a
// stolen session - can quietly change how much personal data this
// deployment stores and how long it keeps it. Those are not operational
// decisions. They are the ones somebody has to answer for later.
//
// So a small, named set of settings requires a password that does not
// come from the database at all. It comes from the config file, which
// means changing it needs access to the server, which means the customer
// cannot change it and neither can anything that compromised the panel
// process without also getting a shell.
//
// # The rules, and how each one is actually enforced
//
// These are not conventions. Each is enforced by something that fails
// loudly rather than by everyone remembering:
//
//   - The password is never stored in plaintext. The config carries
//     only the argon2id hash. A plaintext field exists in Config solely
//     so that putting a password there is a startup error with a useful
//     message, instead of an unknown key nobody notices.
//
//   - It is asked every single time. Verify does not create a session
//     or a grant that outlives the operation it was asked for: it
//     returns an Authorization that expires in seconds and names one
//     action. Stashing one for later does not work, and there is a test
//     that proves it does not work.
//
//   - An Authorization cannot be forged. Its validity flag is
//     unexported and nothing outside this package can set it, so no
//     other package can construct a valid one. That is a compiler
//     guarantee, not a review checklist item.
//
//   - An Authorization cannot be moved sideways. It authorizes the one
//     action it was minted for. Verifying in order to change the log
//     level does not authorize turning IP masking off.
//
//   - With no hash configured, the gate is shut. Guarded settings keep
//     their defaults and cannot be changed at all. Since the defaults
//     are the privacy-preserving values, failing closed costs nothing
//     that should be free.
//
// # Why the throttling is not optional
//
// Verification costs one argon2id computation - about 19 MiB and tens of
// milliseconds, on purpose. An endpoint that runs one per request is a
// denial-of-service amplifier pointed at the machine the collector runs
// on. So verifications are serialised, the queue behind them is bounded,
// and repeated failures stop being answered at all for a while. A gate
// that took the whole machine down when somebody leaned on it would not
// be protecting anything.
package devgate

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
)

// GrantLifetime is how long an Authorization stays usable.
//
// It exists to carry a verification from the handler that performed it
// to the store that needs it, which are microseconds apart. Thirty
// seconds is far more than that needs and far less than anything anyone
// could mistake for a session - which is the point: the user asked for
// the password to be required every time, and a grant that cannot
// survive the request cannot become a login.
const GrantLifetime = 30 * time.Second

// MinPasswordLen for the developer password is longer than for an
// account password. It is one shared secret, typed rarely, protecting
// the settings with legal consequences - so there is no usability
// argument for keeping it short, and no lockout risk in making it long.
const MinPasswordLen = 16

// Throttling. Two limits, because they stop different things: the
// failure counter stops guessing, and the queue bound stops somebody
// making the machine do argon2 work in parallel.
const (
	// throttleWindow is how far back failures are counted.
	throttleWindow = 15 * time.Minute
	// maxFailures is small because this password is not typed daily by
	// somebody who might have forgotten it. Five wrong answers in a
	// quarter of an hour is not a typo.
	maxFailures = 5
	// maxQueued is how many verifications may wait for the one in
	// flight. Beyond it, requests are refused without hashing, so the
	// memory cost stays bounded no matter how hard the endpoint is
	// pushed.
	maxQueued = 4
)

// Config is the [developer] section of a service's config file.
type Config struct {
	// PasswordHash is the argon2id PHC string, as produced by
	// cmd/devpass. Empty means no developer password is configured,
	// which shuts the gate rather than opening it.
	PasswordHash string `toml:"password_hash"`
	// Password exists only to be refused.
	//
	// Sooner or later somebody writes the password here, because it is
	// the obvious thing to write in a field called password. Silently
	// ignoring the key would leave them believing the gate is
	// configured when it is shut; leaving the field out entirely turns
	// it into an unrecognised key, which most TOML setups drop without
	// a word. Naming it and refusing it is the only version that
	// produces an error the person can act on.
	Password string `toml:"password"`
}

// Decision is the outcome of one verification attempt. A closed set, so
// "how many refusals yesterday" is a count rather than a text search.
type Decision string

const (
	// DecisionGranted means the password matched.
	DecisionGranted Decision = "granted"
	// DecisionWrongPassword means it did not.
	DecisionWrongPassword Decision = "wrong_password"
	// DecisionNoPassword means the field arrived empty.
	DecisionNoPassword Decision = "no_password_supplied"
	// DecisionNotConfigured means this deployment has no developer
	// password, so the guarded settings cannot be changed at all.
	DecisionNotConfigured Decision = "gate_not_configured"
	// DecisionThrottled means too many recent failures.
	DecisionThrottled Decision = "throttled"
	// DecisionBusy means too many verifications were already queued.
	DecisionBusy Decision = "busy"
	// DecisionNoAction means the caller named nothing to authorize,
	// which is a programming error rather than a user one.
	DecisionNoAction Decision = "no_action_requested"
)

// Request is one attempt to pass the gate.
type Request struct {
	// Actions names every guarded operation this one password entry
	// should authorize.
	//
	// The caller works this list out *itself*, from what it is about to
	// do - never from anything the client sent. A request that could
	// name its own authorized actions would be a request that authorizes
	// itself.
	//
	// One entry is the normal case. Several exist so that a form
	// changing three guarded settings asks for the password once and
	// pays for one argon2 computation, rather than three.
	Actions []string
	// Password is what the person typed.
	Password string
	// Actor labels who typed it, for the record.
	Actor string
	// ActorKind and ActorID identify the caller in whatever scheme the
	// caller uses. This package does not interpret either - it carries
	// them to the audit hook and nothing else - which is what keeps it
	// importable by something with a different notion of who is acting.
	ActorKind string
	ActorID   int64
	// Peer is the address it came from. Unlike anything in the payload,
	// this cannot be forged by the client.
	Peer string
}

// Attempt is what the Gate reports to its Audit hook, successful or not.
//
// Both outcomes are recorded. A record of failures alone cannot show
// that the successful entry at 03:00 came from an address that had been
// failing for an hour, which is the shape an actual compromise has.
type Attempt struct {
	At        time.Time
	Decision  Decision
	Actions   []string
	Actor     string
	ActorKind string
	ActorID   int64
	Peer      string
}

// OK reports whether the attempt was granted.
func (a Attempt) OK() bool { return a.Decision == DecisionGranted }

// Options configure a Gate beyond its config file section.
type Options struct {
	// Logger receives one line per attempt in the auth category. Nil
	// uses the default logger.
	Logger *slog.Logger
	// Audit, when set, is called once per attempt. This is where a
	// caller writes the append-only audit row; it is a callback rather
	// than a database handle so that this package can be imported by
	// anything without dragging a data layer along with it.
	//
	// Its error is logged and otherwise ignored: failing to record an
	// attempt must not decide the attempt's outcome, in either
	// direction.
	Audit func(context.Context, Attempt) error
	// Now supplies the clock, for tests.
	Now func() time.Time
}

// Gate verifies the developer password.
//
// Safe for concurrent use.
type Gate struct {
	hash   string
	logger *slog.Logger
	audit  func(context.Context, Attempt) error
	now    func() time.Time

	// slots serialises verification so concurrent attempts cannot
	// multiply argon2's memory cost. Capacity one: the legitimate use
	// is one person clicking save, for which serialising costs nothing
	// measurable.
	slots chan struct{}
	// queued counts attempts waiting for slots, so the wait itself can
	// be bounded.
	queued chan struct{}

	mu       sync.Mutex
	failures []time.Time
}

// New builds a Gate from a config section.
//
// It returns an error for a configuration that cannot work, rather than
// a Gate that refuses everything: a mistyped hash and a wrong password
// are indistinguishable at verification time, and an operator hunting
// for that difference would have nothing to go on. Failing at startup
// costs one restart and saves that hunt.
func New(cfg Config, opts Options) (*Gate, error) {
	if cfg.Password != "" {
		return nil, fmt.Errorf(
			"devgate: [developer] password must not hold a plaintext password - " +
				"put the hash in password_hash instead, and generate it with: go run ./cmd/devpass")
	}
	if cfg.PasswordHash != "" {
		if err := argon2id.CheckEncoding(cfg.PasswordHash); err != nil {
			return nil, fmt.Errorf("devgate: [developer] password_hash is not a valid argon2id hash - regenerate it with: go run ./cmd/devpass")
		}
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Gate{
		hash:   cfg.PasswordHash,
		logger: opts.Logger,
		audit:  opts.Audit,
		now:    opts.Now,
		slots:  make(chan struct{}, 1),
		queued: make(chan struct{}, maxQueued),
	}, nil
}

// Configured reports whether this deployment has a developer password at
// all. False means every guarded setting is frozen at its default, which
// the panel should say plainly rather than leaving the operator to
// discover it by being refused.
func (g *Gate) Configured() bool { return g.hash != "" }

// Verify checks a password and, on success, mints one Authorization per
// requested action.
func (g *Gate) Verify(ctx context.Context, req Request) Result {
	result := g.decide(ctx, req)

	attempt := Attempt{
		At:        g.now(),
		Decision:  result.Decision,
		Actions:   req.Actions,
		Actor:     req.Actor,
		ActorKind: req.ActorKind,
		ActorID:   req.ActorID,
		Peer:      req.Peer,
	}

	// Logged before audited, because the log write cannot fail in a way
	// that loses the record: the tree falls back to stderr, while the
	// audit hook talks to a database that can be down.
	verdict := logging.VerdictRejected
	switch result.Decision {
	case DecisionGranted:
		verdict = logging.VerdictAccepted
	case DecisionThrottled, DecisionBusy:
		verdict = logging.VerdictThrottled
	}
	g.logger.Info("developer password attempt",
		append(
			logging.Attempt(req.Actor, verdict, string(result.Decision), req.Peer),
			slog.String("actions", joinActions(req.Actions)),
		)...)

	if g.audit != nil {
		if err := g.audit(ctx, attempt); err != nil {
			g.logger.Warn("developer password attempt could not be audited",
				logging.In(logging.CategoryError), "err", err)
		}
	}
	return result
}

// decide is Verify without the reporting, so the reporting happens on
// exactly one path and cannot be skipped by an early return.
func (g *Gate) decide(ctx context.Context, req Request) Result {
	if len(req.Actions) == 0 {
		return Result{Decision: DecisionNoAction}
	}
	if !g.Configured() {
		// Distinguishable from a wrong password on purpose. This is a
		// property of the deployment, not a secret, and the operator
		// needs to be told which of the two situations they are in.
		return Result{Decision: DecisionNotConfigured}
	}
	if req.Password == "" {
		// No argon2 work, and not counted as a failure: an empty field
		// is a person who has not typed yet, and a form bug that sent
		// empty repeatedly should not lock the operator out.
		return Result{Decision: DecisionNoPassword}
	}

	if blocked, retry := g.throttled(); blocked {
		return Result{Decision: DecisionThrottled, RetryAfter: retry}
	}

	// Bound the queue before joining it. Refusing here costs nothing;
	// refusing after allocating 19 MiB would be too late to matter.
	select {
	case g.queued <- struct{}{}:
		defer func() { <-g.queued }()
	default:
		return Result{Decision: DecisionBusy, RetryAfter: time.Second}
	}

	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	case <-ctx.Done():
		return Result{Decision: DecisionBusy, RetryAfter: time.Second}
	}

	// Re-checked after the wait: failures may have accumulated while
	// this attempt sat in the queue, and the whole point of the limit is
	// that it applies to the attempt about to be made, not the one that
	// arrived.
	if blocked, retry := g.throttled(); blocked {
		return Result{Decision: DecisionThrottled, RetryAfter: retry}
	}

	ok, _ := argon2id.Verify(g.hash, req.Password)
	if !ok {
		g.recordFailure()
		return Result{Decision: DecisionWrongPassword}
	}
	// Deliberately no rehash-on-success path. The hash lives in a file
	// on the server, not in a table this process may write; upgrading it
	// is an edit somebody makes with the tool, and silently rewriting a
	// customer's config from a request handler would be worse than the
	// stale parameters it fixed.

	g.clearFailures()

	expires := g.now().Add(GrantLifetime)
	auths := make(map[string]Authorization, len(req.Actions))
	for _, action := range req.Actions {
		if action == "" {
			continue
		}
		auths[action] = Authorization{
			action:    action,
			expiresAt: expires,
			granted:   true,
			now:       g.now,
		}
	}
	return Result{Decision: DecisionGranted, auths: auths}
}

// throttled reports whether recent failures have closed the gate.
func (g *Gate) throttled() (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	cutoff := g.now().Add(-throttleWindow)
	kept := g.failures[:0]
	for _, at := range g.failures {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	g.failures = kept

	if len(g.failures) < maxFailures {
		return false, 0
	}
	// Until the oldest counted failure falls out of the window, rounded
	// up to the second. Approximate on purpose: an exact figure would
	// let somebody time their retries perfectly.
	retry := g.failures[0].Add(throttleWindow).Sub(g.now()).Round(time.Second)
	if retry < time.Second {
		retry = time.Second
	}
	return true, retry
}

func (g *Gate) recordFailure() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = append(g.failures, g.now())
}

// clearFailures forgets recent failures after a correct password, so
// somebody who mistyped twice and then got it right does not start the
// next change with two strikes against them.
func (g *Gate) clearFailures() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = nil
}

// Result is the outcome of a verification.
type Result struct {
	// Decision says what happened.
	Decision Decision
	// RetryAfter is roughly how long until it is worth trying again.
	RetryAfter time.Duration

	// auths is unexported so that a Result cannot be assembled by
	// another package with authorizations it did not earn.
	auths map[string]Authorization
}

// OK reports whether the password was accepted.
func (r Result) OK() bool { return r.Decision == DecisionGranted }

// For returns the authorization for one action.
//
// For an action that was not requested, or an attempt that failed, it
// returns the zero Authorization - which authorizes nothing. There is no
// error to check and no way to get a usable value by accident.
func (r Result) For(action string) Authorization { return r.auths[action] }

// Authorization is proof that the developer password was verified, just
// now, for one specific action.
//
// It is not a token, a session, or a capability that can be stored. It
// carries no secret, expires in seconds, and names exactly one thing it
// permits. A zero Authorization - which is what any other package can
// construct - authorizes nothing.
type Authorization struct {
	action    string
	expiresAt time.Time
	// granted is the field that makes this type unforgeable: it is
	// unexported, has no setter, and is set only by a successful
	// verification in this package.
	granted bool
	// now is carried from the Gate so that expiry is judged by the same
	// clock the grant was issued on, which is what lets a test move time
	// forward without sleeping.
	now func() time.Time
}

// Authorizes reports whether this authorization permits action, right
// now.
//
// Both halves matter. The action check stops an authorization earned for
// a harmless change from being spent on a harmful one; the expiry check
// is what makes "asked every time" true rather than merely intended,
// because an authorization kept in a variable to save the user a second
// prompt is worthless a moment later.
func (a Authorization) Authorizes(action string) bool {
	if !a.granted || a.action == "" || action == "" || a.action != action {
		return false
	}
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	return now().Before(a.expiresAt)
}

// Action is what this authorization was minted for, for error messages.
func (a Authorization) Action() string { return a.action }

func joinActions(actions []string) string {
	out := ""
	for i, action := range actions {
		if i > 0 {
			out += ","
		}
		out += action
	}
	return out
}

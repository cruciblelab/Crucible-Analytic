// Package heartbeat is how a service tells the panel it is alive, what
// build it is, and what has been going wrong.
//
// # The question this answers that /healthz cannot
//
// The beacon and the read API already have /healthz. It says "this
// process is up right now", which a load balancer needs and an operator
// does not: the failure that costs a customer a week of data is a
// collector that is up, answering, and has failed every write since
// Tuesday. Liveness cannot see that.
//
// The collector has no HTTP server at all, and should not get one: it is
// the process that touches attacker bytes, and a listening socket on it
// is surface bought for nothing. So the channel is a row.
//
// # Never at the service's expense
//
// Nothing here may take a service down, slow it down, or make it stop.
// A missing table, a refused connection, a database in recovery - all of
// them log once and are otherwise ignored, because a monitoring feature
// that can break the thing it monitors is worse than no monitoring at
// all. Run returns only when its context ends.
//
// # Never analytics
//
// Nothing written from here is derived from a visitor. No addresses, no
// site ids, no paths, no user agents. The panel's role can read this
// table, and the whole point of that role is that it cannot read
// traffic - so a column here that described traffic would be a second
// route around the isolation, and one no GRANT would reveal.
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
)

// DefaultInterval is how often a service writes its row.
//
// A minute: fast enough that a dead service is obvious on a page
// somebody is looking at, slow enough that four services updating one
// row each is not a workload. The panel decides what counts as stale -
// see Stale - rather than this package deciding for it.
const DefaultInterval = time.Minute

// Counter names.
//
// A closed set rather than free strings, for the reason every closed set
// in this project exists: these become keys the panel looks a label up
// by, and a key nobody has words for renders as a raw identifier on
// somebody's screen. A test in the panel holds the two sides together.
const (
	// CounterWritten is rows successfully written.
	CounterWritten = "yazilan"
	// CounterDropped is rows the service threw away because it could
	// not keep up - the number that matters most, and the first one the
	// health page draws.
	CounterDropped = "dusurulen"
	// CounterRejected is requests refused before any work was done:
	// wrong site, bad payload, over a limit.
	CounterRejected = "reddedilen"
	// CounterAccepted is requests taken in.
	CounterAccepted = "kabul"
	// CounterErrors is failures since start: requests the service could
	// not answer for a reason of its own. The read API reports it; until
	// Z6 nothing did, and the label sat on the page with no producer.
	CounterErrors = "hata"
	// CounterDeadline is requests answered 503 because they ran past the
	// handler deadline (internal/deadline). Not an error: the service
	// did what it promised, and the two together say whether it is
	// broken or slow.
	CounterDeadline = "suresi_dolan"
	// CounterLogLost is log lines that never reached panel_logs. Added by
	// the reporter itself from Options.Log, so no service can forget it.
	CounterLogLost = "gunluk_kaybi"
)

// LogReport is what a service's panel log copy knows about itself:
// internal/logsink's Sink, seen from here.
//
// An interface rather than the type so this package does not import the
// sink, and so a test can hand the reporter a fixed answer.
type LogReport interface {
	// Lost is lines that never reached the table.
	Lost() uint64
	// LastError is the newest ERROR line the service logged, and when.
	LastError() (string, time.Time)
}

// Count converts an unsigned counter into the signed number a row
// carries, saturating instead of wrapping.
//
// Every counter in this product is an atomic.Uint64, because a counter
// only goes up; the row and its JSON are signed, because that is what
// JSON numbers and Go's json package are. So there is a conversion, and
// the choice is where it lives.
//
// Here, once, rather than at each of the six call sites - which is not
// only tidier: a conversion written out six times is six places for
// somebody to write the seventh without the bound. Saturating rather
// than asserting the overflow cannot happen: it needs 9.2 quintillion
// rows and will not, but "a wrong number, silently negative" is a worse
// failure than "a number stuck at the maximum", and the check costs a
// comparison on a path that runs once a minute.
func Count(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// TokenKeyState is what a service says about its IP token key.
//
// Three values because there are three answers, and collapsing the
// first into the third would be a lie about a build that never spoke:
// a boolean column defaulting to false would report "this service has
// no key" for a service whose binary predates the column.
//
// The refusing side is shared by TokenKeyUnknown and TokenKeyAbsent -
// the panel will not offer full mode for either - so the distinction
// buys nothing in the decision and everything in the sentence: "upgrade
// the collector" and "put a key in collector.toml" send an operator to
// different places.
type TokenKeyState string

const (
	// TokenKeyUnknown is the zero value and what an old build, or a
	// service with no addresses to write, leaves in the column.
	TokenKeyUnknown TokenKeyState = ""
	// TokenKeyPresent means this service holds a key TokenIP would
	// actually use - privacy.CanTokenise, not merely a non-empty
	// string. See that function for why the threshold has one home.
	TokenKeyPresent TokenKeyState = "present"
	// TokenKeyAbsent means this service looked and has none.
	TokenKeyAbsent TokenKeyState = "absent"
)

// ParseTokenKeyState reads the column, treating anything unrecognised as
// unknown.
//
// The permissive direction is the safe one here, which is worth saying
// because it usually is not: an unknown word means the panel refuses
// full mode, so a future writer that invents a fourth value costs a
// deployment the fast path and can never open the gate by accident.
func ParseTokenKeyState(value string) TokenKeyState {
	switch TokenKeyState(value) {
	case TokenKeyPresent:
		return TokenKeyPresent
	case TokenKeyAbsent:
		return TokenKeyAbsent
	default:
		return TokenKeyUnknown
	}
}

// TokenKeyStateOf turns a key into what to report about it.
//
// Takes the key rather than a boolean so that no caller has to decide
// what "usable" means - two services report this and the rule is
// privacy.CanTokenise for both.
func TokenKeyStateOf(key []byte) TokenKeyState {
	if privacy.CanTokenise(key) {
		return TokenKeyPresent
	}
	return TokenKeyAbsent
}

// optionalColumns are the service_heartbeat columns a database may not
// have yet, in the order the writer passes their values and the reader
// scans them.
//
// One list, read by both halves. The writer pairs each name with a value
// in Reporter.reported, and TestTheOptionalColumnsAreOneList holds the
// two in the same order - because the failure of getting it wrong is
// silent: profile and ip_token_key_state are both text, so a swapped
// pair writes a profile name into the token-key column and PostgreSQL
// accepts it.
var optionalColumns = []string{"profile", "ip_token_key_state"}

// Beat is one service's row.
type Beat struct {
	// Service is the database role the service connects as. It is also
	// what the row-level policy checks, so this field is the identity
	// rather than a label for one.
	Service   string
	Version   string
	StartedAt time.Time
	BeatAt    time.Time
	Counters  map[string]int64
	// Profile is the resource profile the service is running, empty for
	// the services that have none and for a build older than the column.
	// See internal/profile and the schema's comment on it.
	Profile string
	// IPTokenKey is whether this service could tokenise an address if
	// the panel asked it to. Unknown for the read API, which writes
	// none, and for a build older than the column.
	IPTokenKey TokenKeyState
	LastError  string
	// LastErrorAt is the zero time when nothing has failed, rather than
	// a nil pointer: a template cannot hand a *time.Time to a formatter,
	// and finding that out at render time is a defect that reaches
	// production on the page nobody looks at until something is wrong.
	LastErrorAt time.Time
}

// Age is how long ago this service last said anything.
func (b Beat) Age(now time.Time) time.Duration { return now.Sub(b.BeatAt) }

// Stale reports whether the beat is old enough to mean something.
//
// Three intervals rather than one. A service that missed a single write
// - a slow query, a moment of database contention, a restart - is not a
// service that is down, and a page that says "DOWN" about one is a page
// somebody stops believing. Three misses is a pattern.
func (b Beat) Stale(now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return b.Age(now) > 3*interval
}

// Uptime is how long the process has been running.
func (b Beat) Uptime() time.Duration { return b.BeatAt.Sub(b.StartedAt) }

// Reporter writes one service's row on a timer.
type Reporter struct {
	pool    *pgxpool.Pool
	service string
	version string
	started time.Time
	// counters is called on every beat. Supplied as a function rather
	// than a value so the service keeps owning its own numbers - this
	// package never holds a reference to a counter it did not create.
	counters func() map[string]int64
	// log supplies the last error and the log-loss count; nil for a
	// service with no panel log copy.
	log LogReport
	// profile is fixed for the life of the process: it is derived from
	// configuration that is read once at startup, and changing it needs
	// a restart because the datasets it names are loaded at startup too.
	profile string
	// ipTokenKey is fixed for the life of the process for the same
	// reason profile is: the key is read once, at startup, and a key
	// added to the file afterwards is not a key this process holds.
	ipTokenKey TokenKeyState
	interval   time.Duration
	logger     *slog.Logger
	now        func() time.Time

	// columnsOnce guards the one-time check for the optional columns;
	// see write. present is only written inside it, and only read
	// after it, so it needs no lock of its own.
	columnsOnce sync.Once
	present     map[string]bool

	mu sync.Mutex
	// warned stops a database that is down from filling the log with one
	// line a minute, forever. The first failure is worth a line; the
	// four hundredth is noise that buries whatever else happened.
	warned bool
}

// Options configures a Reporter. Only Pool, Service and Started are
// required.
type Options struct {
	Pool *pgxpool.Pool
	// Service is the database role this service connects as.
	//
	// Leave it empty, which is the intended use: the reporter asks the
	// connection for current_user on its first beat. That is the value
	// the row-level policy compares against, so taking it from anywhere
	// else - a config key, a constant - creates a second source for one
	// fact and a way to configure a service into silently writing
	// nothing. There is no correct value a caller could supply that the
	// database does not already know.
	//
	// Set only by tests, which need to name a role they are not
	// connected as.
	Service string
	Version string
	// Profile is what internal/profile calls this service's resource
	// configuration. Only the collector has one; everything else leaves
	// it empty, which the panel renders as nothing.
	Profile string
	// IPTokenKey is what this service can say about its IP token key.
	// The two services that write addresses set it from
	// TokenKeyStateOf(their configured key); everything else leaves it
	// unknown, which is what the read API means and what the panel
	// reads as "do not offer full mode on my account".
	IPTokenKey TokenKeyState
	Started    time.Time
	Counters   func() map[string]int64
	// Log is the service's panel log copy - the sink logsink.Attach
	// returned. The row's last error and its log-loss counter come from
	// it; every service main passes it, and internal/invariants holds
	// them to that.
	Log      LogReport
	Interval time.Duration
	Logger   *slog.Logger
	// Now supplies the clock, for tests.
	Now func() time.Time
}

// New returns a Reporter. A nil pool yields a Reporter whose Run returns
// immediately, so a service configured without one needs no branch at
// its call site.
func New(o Options) *Reporter {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Started.IsZero() {
		o.Started = o.Now()
	}
	if o.Counters == nil {
		o.Counters = func() map[string]int64 { return nil }
	}
	return &Reporter{
		pool: o.Pool, service: o.Service, version: o.Version,
		profile: o.Profile, ipTokenKey: o.IPTokenKey,
		started: o.Started, counters: o.Counters, log: o.Log,
		interval: o.Interval, logger: o.Logger, now: o.Now,
	}
}

// Run writes a row now and then on every tick, until ctx ends.
func (r *Reporter) Run(ctx context.Context) {
	if r == nil || r.pool == nil {
		return
	}

	// Once immediately. A service that starts and then waits a minute
	// before saying anything is a service the panel calls dead for the
	// first minute of every restart.
	r.beat(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.beat(ctx)
		}
	}
}

// resolveService asks the connection who it is.
//
// Retried on every beat rather than once at startup, and that is a fix
// rather than a preference. The first version resolved once in Run and
// returned when it failed - so a database that was not ready during the
// thirty seconds after boot left the service unmonitored for the rest of
// its life. systemd starts these processes in parallel with PostgreSQL;
// that window is the normal case, not an edge one.
func (r *Reporter) resolveService(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var name string
	if err := r.pool.QueryRow(ctx, `SELECT current_user`).Scan(&name); err != nil {
		r.mu.Lock()
		first := !r.warned
		r.warned = true
		r.mu.Unlock()
		if first {
			// Once, for the same reason beat only warns once: a database
			// that is down would otherwise write a line a minute until
			// it comes back, burying whatever caused it to go down.
			r.logger.Warn("heartbeat: could not ask the database which role this service is; "+
				"this service will not appear on the health page until it can", "err", err)
		}
		return false
	}
	r.service = name
	return true
}

// snapshot is what this beat reports: the last error and the counters,
// the service's own and the log-loss count added to them.
//
// # The last error stays after the service recovers
//
// Which is the point of the field: "it worked when I looked" is how an
// intermittent fault survives for months. The sink keeps the newest one
// for the life of the process, and nothing clears it.
//
// # Why this replaced a Note method
//
// The reporter had one, "called by the service on a failure it wants an
// operator to see". No service ever called it. Two tests did, and they
// passed because they were its only callers - the 5b shape again, this
// time on the page's last-error line, which was empty on every
// installation there has been. Asking the log copy for it on every beat
// needs one field set in each main instead of one call at every failure
// site, and internal/invariants can see a field.
func (r *Reporter) snapshot() (lastError string, at time.Time, counters map[string]int64) {
	// Copied rather than written into: the map is the service's, and the
	// service may hand back the same one every time.
	counters = map[string]int64{}
	for k, v := range r.counters() {
		counters[k] = v
	}
	if r.log != nil {
		counters[CounterLogLost] = Count(r.log.Lost())
		lastError, at = r.log.LastError()
		lastError = truncate(lastError, 500)
	}
	return lastError, at, counters
}

// beat writes the row. It never returns an error, by design - see the
// package comment.
func (r *Reporter) beat(ctx context.Context) {
	if r.service == "" && !r.resolveService(ctx) {
		return
	}

	lastError, lastErrorAt, counted := r.snapshot()
	counters, err := json.Marshal(counted)
	if err != nil {
		// Cannot happen for map[string]int64, and if it somehow did,
		// an empty object is a better row than no row.
		counters = []byte(`{}`)
	}

	var errAt *time.Time
	if !lastErrorAt.IsZero() {
		errAt = &lastErrorAt
	}

	// A short deadline of its own. The service's context may be the
	// process lifetime, and a heartbeat that blocks for ten minutes on a
	// wedged database is a goroutine leak with a timer attached.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	execErr := r.write(ctx, counters, lastError, errAt)

	r.mu.Lock()
	defer r.mu.Unlock()
	if execErr != nil {
		if !r.warned {
			// Once. A database that is down would otherwise write one
			// line a minute for as long as it stays down, and bury the
			// reason it went down in the process.
			r.logger.Warn("heartbeat: could not write the service row; monitoring will be blind to this service",
				"service", r.service, "err", execErr)
			r.warned = true
		}
		return
	}
	if r.warned {
		r.logger.Info("heartbeat: the service row is being written again", "service", r.service)
		r.warned = false
	}
}

// Read returns every service's row, oldest beat last.
//
// Used by the panel. A service with no row has never started under a
// build that reports one, which is a different fact from "it is down" -
// so this returns what is there and the caller says which services it
// expected.
func Read(ctx context.Context, pool *pgxpool.Pool) ([]Beat, error) {
	// The same accommodation the writer makes, for the same window and
	// the same reason: a panel binary carrying schema 8 against a
	// database still on 7 must show the health page rather than an
	// error. It is the page an operator opens to find out what state the
	// upgrade is in.
	// One statement whose *result* shape is fixed and whose column list
	// follows the database: a column this database has is selected, and
	// one it does not have is replaced by an empty literal. So the scan
	// below never changes, which is the half that would otherwise have
	// to be written once per combination.
	//
	// Nothing variable reaches the string - the names come from the same
	// literal list the writer uses. See Reporter.write for the trade
	// this replaced and why.
	selected := make([]string, 0, len(optionalColumns))
	for _, column := range optionalColumns {
		has, err := schemaver.HasColumn(ctx, pool, "service_heartbeat", column)
		if err == nil && has {
			selected = append(selected, column)
			continue
		}
		selected = append(selected, `''::text`)
	}

	query := `
		SELECT service, version, started_at, beat_at, counters, last_error, last_error_at,
		       ` + strings.Join(selected, ", ") + `
		FROM service_heartbeat
		ORDER BY beat_at DESC`

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Beat
	for rows.Next() {
		var (
			b       Beat
			raw     []byte
			errAt   *time.Time
			version string
		)
		var tokenKey string
		if err := rows.Scan(&b.Service, &version, &b.StartedAt, &b.BeatAt,
			&raw, &b.LastError, &errAt, &b.Profile, &tokenKey); err != nil {
			return nil, err
		}
		b.IPTokenKey = ParseTokenKeyState(tokenKey)
		b.Version = version
		if errAt != nil {
			b.LastErrorAt = *errAt
		}
		if len(raw) > 0 {
			// A row whose counters will not parse is still a row worth
			// showing: the beat time and the version are the two facts
			// an operator needs first, and losing them over a malformed
			// JSON blob would be the monitoring failing exactly when
			// something is already wrong.
			_ = json.Unmarshal(raw, &b.Counters)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// truncate bounds a stored error message.
//
// An error can be arbitrarily long - a driver failure carrying a whole
// query, say - and this column is read by a page that shows it in a
// sentence. Cut by runes rather than bytes so a Turkish message never
// ends in half a character.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// write inserts or replaces this service's row.
//
// # The one column it will do without
//
// profile arrived with schema 8. A binary carrying it, run against a
// database that has not been upgraded yet, would fail every write - and
// the reporter's failure is quiet by design (one warning, then silence),
// so the visible symptom would be the health page showing every service
// as down.
//
// That window is not hypothetical: this project's upgrade order is
// schema first, binaries second, and an operator who does it the other
// way round, or who is halfway through, is exactly the person looking at
// the health page. Blinding the monitoring during an upgrade is worse
// than losing one label, so the label is what gives way.
//
// Checked once rather than per beat. The answer only changes when
// somebody applies a schema, which restarts nothing - so a process that
// started before the upgrade keeps writing without the column until it
// is restarted, and that is both correct and invisible: the column is
// empty for a minute or a day and then it is not.
func (r *Reporter) write(ctx context.Context, counters []byte, lastError string, errAt *time.Time) error {
	r.columnsOnce.Do(func() { r.detectColumns(ctx) })

	// The fixed half. now() rather than a parameter: the beat time is
	// the database's opinion, so two services on two machines with two
	// clock skews still sort against each other correctly on the health
	// page.
	columns := `service, version, started_at, beat_at, counters, last_error, last_error_at`
	values := `$1, $2, $3, now(), $4, $5, $6`
	sets := `version = EXCLUDED.version,
		    started_at = EXCLUDED.started_at,
		    beat_at = now(),
		    counters = EXCLUDED.counters,
		    last_error = EXCLUDED.last_error,
		    last_error_at = EXCLUDED.last_error_at`
	args := []any{r.service, r.version, r.started, counters, lastError, errAt}

	// And the optional half, assembled from the columns this database
	// turned out to have.
	//
	// # Why this is assembled and the previous version was not
	//
	// It used to be two whole statements, one naming profile and one
	// not, and the comment beside them argued against exactly what this
	// loop does: a query built by concatenation is one a future edit
	// can make take a value from somewhere else.
	//
	// That argument was right about the risk and is answered rather than
	// ignored. Every fragment below comes from reported(), which is a
	// literal list in this file; nothing a caller, a config file or a
	// database row can influence reaches the string. What changed is the
	// other side of the trade: 5b adds a second optional column, and
	// hand-written statements for every combination is four literals
	// now and eight at the next one, differing in one clause each. A set
	// of eight near-identical statements nobody compares is a defect
	// this project has already paid for more than once.
	for _, opt := range r.reported() {
		if !r.present[opt.column] {
			continue
		}
		columns += ", " + opt.column
		values += fmt.Sprintf(", $%d", len(args)+1)
		sets += fmt.Sprintf(",\n\t\t    %s = EXCLUDED.%s", opt.column, opt.column)
		args = append(args, opt.value)
	}

	_, err := r.pool.Exec(ctx, `
		INSERT INTO service_heartbeat (`+columns+`)
		VALUES (`+values+`)
		ON CONFLICT (service) DO UPDATE SET
		    `+sets, args...)
	return err
}

// reported pairs each optional column with the value this process would
// write into it.
//
// One list rather than a column list and a value list side by side: two
// parallel slices are two things to keep in the same order, and getting
// that wrong would write the profile into the token-key column with no
// error anywhere - both are text.
func (r *Reporter) reported() []struct {
	column string
	value  any
} {
	return []struct {
		column string
		value  any
	}{
		{"profile", r.profile},
		{"ip_token_key_state", string(r.ipTokenKey)},
	}
}

// detectColumns asks the catalog which optional columns exist.
//
// Once per process, because the answer changes only when somebody
// applies a schema, and a schema upgrade restarts nothing - so the cost
// of being wrong for the rest of this process's life is one column
// missing from a page until the next restart, against a catalog query on
// every beat forever.
//
// A failed question assumes the column is there, for the reason the
// profile check gave when it was alone: the far more common reason to be
// here is a database blip rather than an old schema, and the write
// reports its own failure anyway.
func (r *Reporter) detectColumns(ctx context.Context) {
	r.present = make(map[string]bool, 2)
	for _, opt := range r.reported() {
		has, err := schemaver.HasColumn(ctx, r.pool, "service_heartbeat", opt.column)
		if err != nil {
			r.present[opt.column] = true
			continue
		}
		r.present[opt.column] = has
		if !has {
			r.logger.Info("heartbeat: this database has no "+opt.column+" column yet, so "+
				"that detail will not appear in the panel until the schema is "+
				"upgraded; everything else is being reported normally",
				"column", opt.column)
		}
	}
}

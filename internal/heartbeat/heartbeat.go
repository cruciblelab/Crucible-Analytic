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
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	// not keep up - the number that matters most and the one nothing
	// currently surfaces.
	CounterDropped = "dusurulen"
	// CounterRejected is requests refused before any work was done:
	// wrong site, bad payload, over a limit.
	CounterRejected = "reddedilen"
	// CounterAccepted is requests taken in.
	CounterAccepted = "kabul"
	// CounterErrors is failures since start.
	CounterErrors = "hata"
)

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
	LastError string
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
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time

	mu          sync.Mutex
	lastError   string
	lastErrorAt time.Time
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
	Service  string
	Version  string
	Started  time.Time
	Counters func() map[string]int64
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
		started: o.Started, counters: o.Counters,
		interval: o.Interval, logger: o.Logger, now: o.Now,
	}
}

// Note records the last thing that went wrong.
//
// Called by the service on a failure it wants an operator to see. A nil
// error clears nothing: the point of this field is that a failure stays
// visible after the service recovers, because "it worked when I looked"
// is how an intermittent fault survives for months.
func (r *Reporter) Note(err error) {
	if err == nil || r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastError = truncate(err.Error(), 500)
	r.lastErrorAt = r.now()
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

// beat writes the row. It never returns an error, by design - see the
// package comment.
func (r *Reporter) beat(ctx context.Context) {
	if r.service == "" && !r.resolveService(ctx) {
		return
	}

	r.mu.Lock()
	lastError, lastErrorAt := r.lastError, r.lastErrorAt
	r.mu.Unlock()

	counters, err := json.Marshal(nonNil(r.counters()))
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

	_, execErr := r.pool.Exec(ctx, `
		INSERT INTO service_heartbeat
		    (service, version, started_at, beat_at, counters, last_error, last_error_at)
		VALUES ($1, $2, $3, now(), $4, $5, $6)
		ON CONFLICT (service) DO UPDATE SET
		    version = EXCLUDED.version,
		    started_at = EXCLUDED.started_at,
		    beat_at = now(),
		    counters = EXCLUDED.counters,
		    last_error = EXCLUDED.last_error,
		    last_error_at = EXCLUDED.last_error_at`,
		r.service, r.version, r.started, counters, lastError, errAt)

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
	rows, err := pool.Query(ctx, `
		SELECT service, version, started_at, beat_at, counters, last_error, last_error_at
		FROM service_heartbeat
		ORDER BY beat_at DESC`)
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
		if err := rows.Scan(&b.Service, &version, &b.StartedAt, &b.BeatAt,
			&raw, &b.LastError, &errAt); err != nil {
			return nil, err
		}
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

func nonNil(m map[string]int64) map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return m
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

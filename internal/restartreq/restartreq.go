// Package restartreq carries a restart request from the panel to a
// running service (PLAN §4, #38): the panel writes when it wants the
// service to restart, and the service, seeing a request newer than its
// own start, exits cleanly and lets its supervisor start it again.
//
// # Why exiting, and not something that restarts
//
// The catalogue wrote the mechanism down before anything was built: "exit
// cleanly, let systemd restart it - the panel spawns nothing. That is the
// important property: a repair surface that can only stop cannot be
// turned into one that starts arbitrary things, whatever is done to it."
//
// The other design on the table was V's doorbell - a file the panel
// creates and a root-owned unit that runs systemctl restart. It was
// weighed and set aside (NOTES, B3e): it would give an internet-facing
// process a way to trigger something root runs, it needs a step on every
// systemd install and does not exist under Docker at all, and it can
// start a service an administrator stopped on purpose. A stopped process
// cannot read a request, so this design cannot.
//
// Both supervisors this project ships restart a service that exits on
// its own: every unit says Restart=always, and every compose service says
// restart: unless-stopped.
//
// # One rule, two readers
//
// The request is the database's clock at the moment the panel wrote it;
// the service's start is the database's clock too (heartbeat.Reporter
// measures it once, from the monotonic time since the process began).
// Satisfied is the only comparison: the service exits when it is false,
// and the health page says "restarted" when it is true. Two copies of
// that sentence would be two answers the day one of them changed. The
// value is read the one way as well, by ParseRequest, so the page and the
// service cannot disagree about which rows hold a request at all.
//
// # A request is acted on once it is due, and never before
//
// A request later than the database's own clock is not acted on until
// the clock reaches it. The panel cannot write one - it writes now() -
// but a row is a value anybody holding panel_user can set, and the
// database's clock can step backwards. Without this, a request an hour
// ahead is a restart loop: the new process starts before the request,
// so it is not satisfied, so it exits, every few seconds for an hour.
// With it, the service exits only when requested <= now, and the process
// that replaces it starts after that now - so it starts after the
// request, and the request is satisfied. That is the whole argument for
// "no loop", and it needs no slack: a slack of s would let a request up
// to s ahead through, and a restart faster than s would loop on it.
package restartreq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// KeyPrefix is the settings row a request lives under, followed by the
// service's database role - the identity the heartbeat reports under.
//
// A row of panel_settings rather than a table of its own, because that
// is the table the panel already writes and every service able to hear
// a request already reads. It is not a setting: the settings page lists
// the registry, not the table, and this key is in no registry.
//
// The row is the deployment-wide one: scope global, site_id empty. A
// row under the same key with any other scope is not a request.
const KeyPrefix = "process.restart."

// Key is the settings key for one service's requests.
func Key(service string) string { return KeyPrefix + service }

// Satisfied reports whether a process that started at started has
// honoured a request made at requested: it started at the request or
// after it.
func Satisfied(requested, started time.Time) bool { return !requested.After(started) }

// ParseRequest reads a request row's value: a JSON string holding a
// timestamp, which is what to_jsonb(now()) writes - RFC 3339 with the
// session's offset, to the microsecond.
//
// Anything else is not a request, and the error says what it was. The
// watcher does not exit on one and does not fail on one either: the
// value is reported and ignored until it changes.
func ParseRequest(value []byte) (time.Time, error) {
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return time.Time{}, fmt.Errorf("restartreq: the value is not a JSON string: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("restartreq: the value is not a timestamp: %w", err)
	}
	return at, nil
}

// ErrRequested is the cause a service's main gives the context it
// cancels when Exit is called, so the shutdown line can say why the
// process is stopping: Exit: func() { cancel(restartreq.ErrRequested) }.
var ErrRequested = errors.New("restart requested from the panel")

// LogStopping arranges for the line saying why the process is stopping
// to be written the moment ctx ends: ErrRequested for a restart request,
// the signal for a signal (signal.NotifyContext gives its context that
// cause). A main calls it on the context its services run under, and
// calls the function it returns before it writes "shutdown complete".
//
// Written when the shutdown begins rather than when it ends, so a
// shutdown that hangs - the case the watcher's backstop is for - still
// has its reason in the log. Before this, no service said why it was
// stopping at all; "shutdown complete" was the only line, and it came
// at the end.
//
// The returned function is what keeps the two lines in that order. The
// first version was a goroutine waiting on ctx, and the real binaries
// showed what that is worth: the read API's shutdown takes microseconds,
// and its log said "shutdown complete" and then "stopping" - and a main
// that returns first is a process that exits before the goroutine runs,
// with no line at all. The function waits until the line is written if
// ctx has ended, and returns at once if it has not (a server that
// failed rather than one that was stopped), so it can never hang.
func LogStopping(ctx context.Context, logger *slog.Logger) (written func()) {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		// A context that is done always has a cause: its own error when
		// nobody gave one.
		logger.Info("stopping", "cause", context.Cause(ctx).Error())
	})
	return func() {
		if stop() {
			// ctx had not ended: the line is not going to be written,
			// and nothing is waited for. done is closed here, where the
			// write would have closed it, so a second call returns too.
			close(done)
			return
		}
		<-done
	}
}

// DefaultInterval is how often a service asks. Five seconds against a
// minute for the settings: somebody pressed a button and is waiting.
const DefaultInterval = 5 * time.Second

// privilegeRecheck is how often a service whose role cannot read the
// settings table asks again - so a grant added by a schema upgrade, which
// restarts nothing, is noticed without a restart.
const privilegeRecheck = 5 * time.Minute

// DefaultExitWithin is how long a process may take to finish after it
// has been asked to exit before the watcher ends it. Its graceful paths
// are each bounded - the servers' shutdowns at ten seconds, the
// collector's drain likewise (Z7), the buffered writers' last flush at
// five and ten - and the longest chain of them, the beacon's server and
// then its writer, is twenty. Reaching this means one of them stopped
// being bounded, and a service that has closed its listener and never
// exits is worse than one that exits abruptly: the supervisor restarts
// the second.
const DefaultExitWithin = 30 * time.Second

// queryTimeout bounds each question, as the heartbeat bounds its write:
// the loop's context is the process lifetime, and a question that waits
// for ever on a wedged database is a loop that has stopped.
const queryTimeout = 5 * time.Second

// Watcher asks the database, on its own connection pool, whether this
// service has been asked to restart.
type Watcher struct {
	// Pool is the service's monitoring pool (resources.OpenMonitor):
	// the question is asked on the connections that stay free when the
	// work's pool is exhausted, which is one of the states a restart is
	// for. Nil means no database, and Run returns at once.
	Pool *pgxpool.Pool
	// Started is when this process started in the database's clock,
	// and whether that is known yet - heartbeat.Reporter.Started.
	// Until it is known the watcher reads nothing and does nothing: a
	// request cannot be compared with a start nobody has measured.
	Started func() (time.Time, bool)
	// Exit begins a clean shutdown: the same path SIGTERM takes, which
	// in every main is cancelling the context the service runs under
	// (with ErrRequested as the cause). Called at most once.
	Exit func()
	// Interval is how often to ask. Zero means DefaultInterval.
	Interval time.Duration
	// ExitWithin bounds the clean shutdown. Zero means
	// DefaultExitWithin.
	ExitWithin time.Duration
	Logger     *slog.Logger

	// The rest are for this package's tests, which can set them and
	// nothing else can.
	//
	// hardExit is os.Exit.
	hardExit func(int)
	// recheck is privilegeRecheck.
	recheck time.Duration
	// key replaces Key(current_user), so a test can run the watcher as a
	// real role without reading the key the panel's own tests write for
	// that role, and under the "test." namespace the panel's suite
	// leaves alone when it clears the table. The privilege question is
	// still asked of the real role.
	key string
	// db is the database; nil means Pool.
	db database
}

// database is the two questions a watcher asks.
type database interface {
	// whoAndMay is the connection's role and whether it may read
	// panel_settings, asked without reading it.
	whoAndMay(ctx context.Context) (role string, may bool, err error)
	// request is the raw value of the request row under key, whether
	// there is one, and the database's clock in the same statement.
	request(ctx context.Context, key string) (value []byte, found bool, now time.Time, err error)
}

// state is what a watcher has learned so far. Each failure it can report
// has its own flag, so one recovering does not silence the other.
type state struct {
	role      string
	permitted bool
	askedAt   time.Time

	whoWarned  bool
	readWarned bool
	// reported is the last unusable value warned about, so one bad row
	// is one line however long it stays, and a different bad row is a
	// new one.
	reported string
}

// Run asks until ctx ends or a request arrives.
//
// The first question is asked at once rather than after an interval, as
// the heartbeat's first beat is: the start is usually not known yet at
// that moment, so the read waits, but the privilege answer - and its
// one line, for a role that cannot read - comes when the service starts
// rather than five seconds later.
func (w *Watcher) Run(ctx context.Context) {
	if w == nil || w.Pool == nil && w.db == nil {
		return
	}
	logger := w.logger()
	if w.Started == nil || w.Exit == nil {
		// A main that built a watcher and left out either has a watcher
		// that can never act. Said once, at ERROR, because it is a defect
		// in this product, not a state of the deployment.
		logger.Error("restart requests: the watcher was built without Started or Exit; " +
			"the panel cannot restart this service")
		return
	}
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	var st state
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if w.step(ctx, &st, logger) {
			w.exit(logger)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// step asks once and reports whether the service should exit.
func (w *Watcher) step(ctx context.Context, st *state, logger *slog.Logger) bool {
	db := w.database()

	recheck := w.recheck
	if recheck <= 0 {
		recheck = privilegeRecheck
	}
	if st.role == "" || (!st.permitted && time.Since(st.askedAt) >= recheck) {
		role, may, err := db.whoAndMay(ctx)
		st.askedAt = time.Now()
		if err != nil {
			if ctx.Err() != nil {
				// The service is stopping; the question was cut short by
				// that, and a warning about it would be a false one.
				return false
			}
			if !st.whoWarned {
				logger.Warn("restart requests: could not ask the database whether this service "+
					"may read requests; asking again", "err", err)
				st.whoWarned = true
			}
			return false
		}
		st.whoWarned = false
		switch {
		case !may && (st.role == "" || st.permitted):
			logger.Info("restart requests: this service's database role cannot read "+
				"panel_settings, so the panel cannot ask it to restart", "service", role)
		case may && st.role != "" && !st.permitted:
			logger.Info("restart requests: this service's database role can now read "+
				"panel_settings; the panel can ask it to restart", "service", role)
		}
		st.role, st.permitted = role, may
	}
	if !st.permitted {
		return false
	}

	started, known := w.Started()
	if !known {
		return false
	}

	service, key := st.role, Key(st.role)
	if w.key != "" {
		key = w.key
	}
	value, found, now, err := db.request(ctx, key)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		if mayNotRead(err) {
			// The grant went away under a running service, or the table
			// did. One ERROR in the server's log, and then the silent
			// question again, on its own interval.
			logger.Info("restart requests: this service's database role cannot read "+
				"panel_settings any more, so the panel cannot ask it to restart",
				"service", service, "err", err)
			st.permitted = false
			st.askedAt = time.Now()
			return false
		}
		if !st.readWarned {
			logger.Warn("restart requests: could not read the request; asking again",
				"service", service, "err", err)
			st.readWarned = true
		}
		return false
	}
	if st.readWarned {
		logger.Info("restart requests: the request is being read again", "service", service)
		st.readWarned = false
	}
	if !found {
		st.reported = ""
		return false
	}

	requested, err := ParseRequest(value)
	if err != nil {
		if st.reported != string(value) {
			logger.Warn("restart requests: the request row does not hold a time; ignoring it "+
				"until it changes", "service", service, "value", clip(value), "err", err)
			st.reported = string(value)
		}
		return false
	}
	if requested.After(now) {
		if st.reported != string(value) {
			logger.Warn("restart requests: the request is later than the database's clock; "+
				"it will be acted on when the clock reaches it", "service", service,
				"requested", requested.UTC().Format(time.RFC3339Nano),
				"database_now", now.UTC().Format(time.RFC3339Nano))
			st.reported = string(value)
		}
		return false
	}
	st.reported = ""
	if Satisfied(requested, started) {
		return false
	}
	logger.Info("restart requests: restart requested from the panel; exiting for the "+
		"supervisor to start this service again", "service", service,
		"requested", requested.UTC().Format(time.RFC3339Nano),
		"started", started.UTC().Format(time.RFC3339Nano))
	return true
}

// exit arms the backstop and begins the clean shutdown, in that order:
// an Exit that blocks must not keep the backstop from being armed.
//
// The backstop is never disarmed. A process that finishes its clean
// shutdown is gone before it fires; one that is still here after
// ExitWithin is the case it is for.
func (w *Watcher) exit(logger *slog.Logger) {
	within := w.ExitWithin
	if within <= 0 {
		within = DefaultExitWithin
	}
	hard := w.hardExit
	if hard == nil {
		hard = os.Exit
	}
	time.AfterFunc(within, func() {
		logger.Error("restart requests: the service had not exited after a restart request; "+
			"exiting now so the supervisor can start it again", "within", within)
		hard(1)
	})
	w.Exit()
}

func (w *Watcher) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

func (w *Watcher) database() database {
	if w.db != nil {
		return w.db
	}
	return poolDB{w.Pool}
}

// mayNotRead reports whether err is the database refusing this role the
// table, or not having it: the two answers whoAndMay gives as "no".
func mayNotRead(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "42501" || pgErr.Code == "42P01")
}

// clip keeps a reported value to a size a log line can carry. The value
// is whatever somebody wrote, and the line is about its shape.
func clip(value []byte) string {
	const max = 64
	if len(value) <= max {
		return string(value)
	}
	return string(value[:max]) + "..."
}

// poolDB asks through a pool.
type poolDB struct{ pool *pgxpool.Pool }

// whoAndMay asks the connection which role it is and whether that role
// may read panel_settings - without reading it, so a role that may not
// leaves no permission error in the database's log every few seconds.
func (p poolDB) whoAndMay(ctx context.Context) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var role string
	var may bool
	// to_regclass rather than the name: on a database where the panel's
	// schema was never applied, has_table_privilege('panel_settings', ...)
	// is an error in the server's log every few minutes, and NULL is a
	// plain "no". The name resolves on the search path, as the read
	// below does, so the two ask about the same table.
	err := p.pool.QueryRow(ctx, `
		SELECT current_user,
		       coalesce(has_table_privilege(to_regclass('panel_settings'), 'SELECT'), false)`).Scan(&role, &may)
	return role, may, err
}

// request reads the request row under key, and the database's clock in
// the same statement: the future check compares the request with the
// clock that will stamp the next process's start.
//
// The value comes back raw and is parsed in Go, by ParseRequest. A cast
// in the query would turn a row nobody can parse into an ERROR in the
// server's log on every interval.
func (p poolDB) request(ctx context.Context, key string) ([]byte, bool, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var value []byte
	var now time.Time
	err := p.pool.QueryRow(ctx, `
		SELECT value, now() FROM panel_settings
		 WHERE scope = 'global' AND site_id = '' AND key = $1`, key).Scan(&value, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, time.Time{}, nil
	}
	if err != nil {
		return nil, false, time.Time{}, fmt.Errorf("restartreq: read: %w", err)
	}
	return value, true, now, nil
}

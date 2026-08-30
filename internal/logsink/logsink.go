// Package logsink is the second place a service's log lines go: a table
// the panel can read, for a customer who has no shell and is not going
// to get one.
//
// # What it is not
//
// Not a replacement for internal/logging's tree. The tree is the
// operator's record and it stays the better one - complete, cheap, and
// readable with grep on a machine somebody is already logged into. This
// carries the subset a customer needs to see, and it is allowed to lose
// lines that the tree keeps.
//
// # The rule that shapes everything here
//
// **The database is never in the request path.** internal/proxy touches
// attacker bytes and forwards them to the customer's own website; a log
// write that waited on PostgreSQL would put a database round-trip
// between a visitor and the site they are visiting, and a database that
// got slow would become a website that got slow.
//
// So: a buffered channel, one background writer, and lines dropped when
// the buffer is full. Dropping is the correct behaviour and the reason
// is that the alternative is worse in exactly the situation that
// matters - a service under load, producing more log lines than usual,
// is the service you least want to block.
//
// What must not happen is dropping *silently*. Dropped is counted and
// the count is reported like every other counter, so "the panel's log is
// missing an hour" is a question with an answer.
//
// # Why WARN and above by default
//
// A log table becomes the largest table in the database, and that is the
// disk-full failure arriving by a second road. The level is a
// *slog.LevelVar shared with internal/logging, so the per-site verbose
// switch - which expires on its own; see logging.Controls.Apply - raises
// this sink at the same moment it raises the tree.
package logsink

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
)

// OperationKey is the attribute a log line carries to say which
// operation it belongs to.
//
// A string key rather than a context value, because slog records already
// travel with their attributes and a context would have to be threaded
// through every call site that logs. The panel's streaming window reads
// this column; without it the only implementable window is "everything
// that happened while you waited", which is noise.
const OperationKey = "operation_id"

// SiteKey is the attribute naming the site a line is about. Absent means
// the line is about the process, which is a different fact from "unknown
// site" and must never be shown to one customer as if it were theirs.
const SiteKey = "site_id"

// maxAttrs bounds how many attributes reach the database from one
// record.
//
// A log line is text somebody else chose, and a caller that attached two
// thousand attributes would otherwise turn one record into a large JSON
// document. The tree keeps them all; this is the copy that shares a
// database with the customer's analytics.
const maxAttrs = 32

// Config is what a service supplies.
type Config struct {
	// Service must be the database role this pool connects as.
	//
	// The row-level policy compares it against current_user, so a
	// mismatch is refused by PostgreSQL rather than written under the
	// wrong name. Passed explicitly rather than read from the connection
	// so the value that is written and the value that is checked come
	// from the same place in the code.
	Service string
	// Level is shared with internal/logging, so the verbose switch moves
	// both at once.
	Level *slog.LevelVar
	// Buffer is how many records may wait. Zero takes the default.
	Buffer int
}

// Sink writes records to panel_logs.
type Sink struct {
	pool    *pgxpool.Pool
	service string
	level   *slog.LevelVar

	records chan record
	done    chan struct{}
	once    sync.Once

	written atomic.Uint64
	dropped atomic.Uint64
	failed  atomic.Uint64
}

type record struct {
	at        time.Time
	level     string
	category  string
	message   string
	attrs     map[string]any
	siteID    string
	operation string
}

// New starts a sink and its writer goroutine.
func New(pool *pgxpool.Pool, cfg Config) *Sink {
	buffer := cfg.Buffer
	if buffer <= 0 {
		buffer = 512
	}
	s := &Sink{
		pool:    pool,
		service: cfg.Service,
		level:   cfg.Level,
		records: make(chan record, buffer),
		done:    make(chan struct{}),
	}
	go s.run()
	return s
}

// Close stops the writer, draining what is already buffered.
//
// Safe to call twice, because a service shutting down has more than one
// path that wants to.
func (s *Sink) Close() {
	s.once.Do(func() {
		close(s.records)
		<-s.done
	})
}

// Counters reports what the sink did, for the health page.
//
// dropped is the one that matters: it is the difference between "the
// panel's log is complete" and "the panel's log is complete except for
// the busiest hour", and a sink that dropped in silence would make the
// second look like the first.
func (s *Sink) Counters() (written, dropped, failed uint64) {
	return s.written.Load(), s.dropped.Load(), s.failed.Load()
}

func (s *Sink) run() {
	defer close(s.done)
	for r := range s.records {
		s.write(r)
	}
}

func (s *Sink) write(r record) {
	// Its own timeout, not the caller's context. The caller has gone by
	// now - this runs on the writer goroutine - and a log write that
	// inherited a request context would be cancelled precisely when the
	// request failed, which is when its log line matters most.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	attrs, err := json.Marshal(r.attrs)
	if err != nil {
		attrs = []byte(`{}`)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO panel_logs (at, service, level, category, message, attrs, site_id, operation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.at, s.service, r.level, r.category, r.message, attrs, r.siteID, r.operation)
	if err != nil {
		// Not logged. A sink that logged its own failures through the
		// logger it is attached to would answer a database outage with
		// an unbounded loop of records about the database outage.
		// Counted instead, and the health page reads the counter.
		s.failed.Add(1)
		return
	}
	s.written.Add(1)
}

// Handler returns the slog.Handler that feeds this sink.
func (s *Sink) Handler() slog.Handler { return &handler{sink: s} }

type handler struct {
	sink  *Sink
	attrs []slog.Attr
	group string
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	if h.sink.level == nil {
		return level >= slog.LevelWarn
	}
	return level >= h.sink.level.Level()
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *handler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.group = name
	return &clone
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	rec := record{
		at: r.Time,
		// Level().String() rather than the number: a panel showing "8"
		// where it means "ERROR" is a panel somebody has to decode.
		level: r.Level.String(),
		// Sanitized here and not only at the database. Every value that
		// reaches this table went through the same pass the log tree
		// uses: invalid UTF-8 removed, control characters dropped,
		// length capped. A log line contains text somebody else chose.
		message: logging.SanitizeValue(r.Message),
		attrs:   make(map[string]any, 8),
	}

	collect := func(a slog.Attr) bool {
		switch a.Key {
		case SiteKey:
			rec.siteID = logging.SanitizeValue(a.Value.String())
			return true
		case OperationKey:
			rec.operation = logging.SanitizeValue(a.Value.String())
			return true
		case logging.CategoryKey:
			rec.category = logging.SanitizeValue(a.Value.String())
			return true
		}
		if len(rec.attrs) >= maxAttrs {
			return true
		}
		rec.attrs[logging.SanitizeValue(a.Key)] = logging.SanitizeValue(a.Value.String())
		return true
	}

	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(func(a slog.Attr) bool { return collect(a) })

	// The one place this package refuses to wait. A full buffer means
	// the database is slower than the service is talkative, and the
	// service wins.
	select {
	case h.sink.records <- rec:
	default:
		h.sink.dropped.Add(1)
	}
	return nil
}

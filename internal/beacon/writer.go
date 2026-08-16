package beacon

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tableName = "beacon_events"

// columns must stay in sync with the field order Writer.flush encodes in
// and with schema.sql's column list.
var columns = []string{
	"time", "site_id", "visitor_id", "event_type", "event_name",
	"path", "query", "title", "referrer_host", "referrer_path",
	"ip", "browser", "os", "device", "is_bot_ua",
	"screen_w", "screen_h", "language", "country", "asn", "asn_org",
	"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
	"ref", "click_source", "click_id",
}

// Writer buffers rows in memory and batch-writes them with COPY.
//
// Buffering is not an optimization here, it is what keeps the HTTP
// handler off the database entirely. Writing each event inline would
// put a network round trip on the critical path of a request a browser
// made while rendering a page, and would tie the beacon's availability
// to the database's - a database hiccup would turn into visible latency
// on customers' sites. Enqueue instead does a single non-blocking
// channel send and returns.
//
// The consequence, stated plainly: events that are buffered when the
// process dies are lost. That is the right trade for analytics - a
// dropped pageview is a rounding error, whereas a page that renders
// slowly because a stats database is busy is a real problem - but it is
// a trade, not a free lunch. Run drains the buffer on shutdown, so only
// an actual crash loses anything.
type Writer struct {
	pool *pgxpool.Pool

	events chan Row
	// FlushInterval bounds how long a row waits in the buffer.
	flushInterval time.Duration
	// batchSize is how many rows force a flush before the interval is
	// up, so a burst does not sit in memory waiting on a timer.
	batchSize int

	dropped atomic.Uint64
	written atomic.Uint64
	logger  *slog.Logger
}

// WriterConfig configures a Writer. Zero values get sensible defaults.
type WriterConfig struct {
	// BufferSize is how many rows may be waiting at once. Past this,
	// Enqueue drops - see Enqueue.
	BufferSize int
	// BatchSize is the number of buffered rows that triggers an
	// immediate flush.
	BatchSize int
	// FlushInterval is the longest a row waits before being written.
	FlushInterval time.Duration
	Logger        *slog.Logger
}

func (c WriterConfig) withDefaults() WriterConfig {
	if c.BufferSize <= 0 {
		// ~10k rows of a few hundred bytes each is a couple of
		// megabytes: enough to ride out a multi-second database stall
		// at a healthy event rate, small enough that a wedged database
		// cannot turn into unbounded memory growth.
		c.BufferSize = 10_000
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 500
	}
	if c.FlushInterval <= 0 {
		// Short, because the buffer is the only thing between an event
		// and durability; a long interval widens the window in which a
		// crash loses data for no real gain.
		c.FlushInterval = 2 * time.Second
	}
	if c.BatchSize > c.BufferSize {
		c.BatchSize = c.BufferSize
	}
	return c
}

// NewWriter opens a connection pool to databaseURL and verifies it is
// reachable. It does not create or migrate the schema - apply
// schema.sql once, separately, exactly as with the collector's writer.
func NewWriter(ctx context.Context, databaseURL string, cfg WriterConfig) (*Writer, error) {
	cfg = cfg.withDefaults()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("beacon: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("beacon: ping database: %w", err)
	}

	return &Writer{
		pool:          pool,
		events:        make(chan Row, cfg.BufferSize),
		flushInterval: cfg.FlushInterval,
		batchSize:     cfg.BatchSize,
		logger:        cfg.Logger,
	}, nil
}

func (w *Writer) log() *slog.Logger {
	if w.logger != nil {
		return w.logger
	}
	return slog.Default()
}

// Enqueue buffers a row, reporting false if the buffer is full.
//
// It never blocks. Blocking would push backpressure from the database
// all the way out to a visitor's browser, which is exactly backwards:
// the site is the thing that matters and the analytics event is the
// thing that does not. When the buffer is full, something is already
// badly wrong (the database is down or hopelessly behind), and the
// least harmful response is to lose events and say so in the logs.
func (w *Writer) Enqueue(row Row) bool {
	select {
	case w.events <- row:
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

// Counters reports rows successfully written and rows dropped for lack
// of buffer space.
func (w *Writer) Counters() (written, dropped uint64) {
	return w.written.Load(), w.dropped.Load()
}

// Run consumes the buffer until ctx is cancelled, then drains whatever
// is left and performs one final write, so a clean shutdown loses
// nothing. It returns when that drain is done.
func (w *Writer) Run(ctx context.Context) {
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]Row, 0, w.batchSize)
	lastDropped := uint64(0)

	for {
		select {
		case <-ctx.Done():
			// Drain without blocking: everything already buffered gets
			// written, but no new arrival can keep shutdown going
			// forever.
			for {
				select {
				case row := <-w.events:
					batch = append(batch, row)
					if len(batch) >= w.batchSize {
						batch = w.flush(context.WithoutCancel(ctx), batch)
					}
					continue
				default:
				}
				break
			}
			// A fresh, bounded context: ctx is already cancelled, so
			// reusing it would abort the very write this drain exists
			// to perform.
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			w.flush(flushCtx, batch)
			cancel()
			return

		case row := <-w.events:
			batch = append(batch, row)
			if len(batch) >= w.batchSize {
				batch = w.flush(ctx, batch)
			}

		case <-ticker.C:
			batch = w.flush(ctx, batch)

			// Drops are reported here rather than per-event so a
			// sustained outage produces one line per interval instead
			// of one per lost event - a log that floods during an
			// incident is a log nobody can read during an incident.
			if dropped := w.dropped.Load(); dropped != lastDropped {
				w.log().Warn("beacon: events dropped, buffer full",
					"dropped_total", dropped, "since_last_report", dropped-lastDropped)
				lastDropped = dropped
			}
		}
	}
}

// flush writes batch and returns an empty batch to continue with. The
// returned slice reuses batch's array, so steady-state flushing does no
// further allocation.
func (w *Writer) flush(ctx context.Context, batch []Row) []Row {
	if len(batch) == 0 {
		return batch
	}
	n, err := w.WriteRows(ctx, batch)
	if err != nil {
		// The batch is discarded rather than retried. Retrying in place
		// would stall the drain loop while the buffer behind it keeps
		// filling, converting a transient database problem into total
		// event loss instead of partial - and there is no durable queue
		// here to retry from anyway.
		w.log().Error("beacon: write failed, batch dropped", "err", err, "rows", len(batch))
		w.dropped.Add(uint64(len(batch)))
		return batch[:0]
	}
	w.written.Add(uint64(n))
	return batch[:0]
}

// WriteRows batch-inserts rows via COPY and returns how many were
// written. Exported so integration tests can write directly without
// going through the buffer.
func (w *Writer) WriteRows(ctx context.Context, rows []Row) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	n, err := w.pool.CopyFrom(ctx, pgx.Identifier{tableName}, columns, pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		return []any{
			r.Time, r.SiteID, r.VisitorID, r.EventType, r.EventName,
			r.Path, r.Query, r.Title, r.ReferrerHost, r.ReferrerPath,
			r.IP, r.Browser, r.OS, r.Device, r.IsBotUA,
			r.ScreenW, r.ScreenH, r.Language, r.Country, r.ASN, r.ASNOrg,
			r.Campaign.Source, r.Campaign.Medium, r.Campaign.Name,
			r.Campaign.Term, r.Campaign.Content, r.Campaign.Ref,
			r.Campaign.ClickIDSource, r.Campaign.ClickID,
		}, nil
	}))
	if err != nil {
		return n, fmt.Errorf("beacon: copy rows: %w", err)
	}
	return n, nil
}

// Pool exposes the connection pool so a service can read settings
// through the same one rather than opening a second.
//
// Sharing is deliberate: a second pool would double this process's
// connection footprint on a database that is also serving the collector,
// and settings are read once a minute - there is nothing to isolate.
func (w *Writer) Pool() *pgxpool.Pool { return w.pool }

// Close releases the connection pool. Call after Run has returned, so
// the shutdown drain still has a working connection.
func (w *Writer) Close() {
	w.pool.Close()
}

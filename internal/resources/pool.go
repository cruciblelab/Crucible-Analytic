package resources

import (
	"context"
	"runtime"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The one number on the CPU side that was wrong.
//
// pgxpool sizes a pool it is not told the size of as the greater of
// four and runtime.NumCPU() (pgx v5.9.2, pgxpool/pool.go:384). NumCPU is
// the host's affinity mask, not the container's budget - measured: it
// reports 4 inside a 0.5-CPU quota on this 4-core machine.
//
// On a four-core machine the two agree, because max(4, 4) is 4 whatever
// the quota. They stop agreeing on exactly the deployment the product
// ships an image for: a container given one CPU on a thirty-two core
// host gets thirty-two connections per pool, four services open four
// such pools, and PostgreSQL's default ceiling is a hundred. The queue
// the pool existed to keep in Go moves into the database, where nothing
// times it out. That arrangement cannot be reproduced on this machine;
// the arithmetic is read off pgx's source and the runtime's behaviour is
// measured, and the rule is held by a test with the numbers spelled out.
//
// GOMAXPROCS is the container-aware number (Go 1.25 reads the CPU quota
// for it), so the fix keeps pgx's own formula and swaps the input. On
// any host without a CPU limit the two are equal and nothing changes.

// minPoolConns is pgx's own floor, kept rather than re-argued.
const minPoolConns = 4

// DefaultPoolMaxConns is the size a pool gets when its DSN does not say.
//
// The narrowing to int32 carries no upper bound, on purpose: the runtime
// keeps GOMAXPROCS as an int32 itself (gomaxprocs in runtime2.go), so the
// value it hands back already fits. A bound here could never be reached
// and no test could show it doing anything. gosec's G115 flags the
// one-expression form, int32(runtime.GOMAXPROCS(0)), and not this one -
// measured - but the runtime's own type is why neither needs a guard.
func DefaultPoolMaxConns() int32 {
	n := runtime.GOMAXPROCS(0)
	if n < minPoolConns {
		n = minPoolConns
	}
	return int32(n)
}

// PoolConfig is a pool configuration whose size has been decided here.
//
// A distinct type rather than a bare *pgxpool.Config, so NewPool cannot
// be handed one that skipped the sizing - the compiler holds that, not a
// comment. It embeds the pgx config, so callers adjust everything else
// (the API turns JIT off, for instance) exactly as before.
type PoolConfig struct {
	*pgxpool.Config
}

// ParseConfig parses a DSN and sizes the pool from this process's CPU
// budget, unless the DSN names pool_max_conns itself.
//
// The operator's value is recognised the way pgxpool recognises it: pgx
// parses the DSN and leaves unknown keys in RuntimeParams, and that is
// where pgxpool looks. Asking the same place means both spellings - URL
// query and key=value - are honoured without this package parsing
// either.
func ParseConfig(dsn string) (*PoolConfig, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if !namesPoolMaxConns(dsn) {
		cfg.MaxConns = DefaultPoolMaxConns()
	}
	// An operator who asked for a floor above the size this package
	// chose meant the floor. Raising the ceiling to meet it is the
	// reading that keeps their number; refusing to start would punish
	// them for a default they never saw.
	if cfg.MinConns > cfg.MaxConns {
		cfg.MaxConns = cfg.MinConns
	}
	return &PoolConfig{Config: cfg}, nil
}

// namesPoolMaxConns reports whether the DSN sets the pool size itself.
func namesPoolMaxConns(dsn string) bool {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		// pgxpool.ParseConfig has already accepted this DSN, so this
		// cannot happen; if it somehow did, "the operator did not say"
		// is the answer that changes least.
		return false
	}
	_, ok := cc.RuntimeParams["pool_max_conns"]
	return ok
}

// NewPool opens a pool from a configuration ParseConfig sized.
//
// The only place in the product that calls pgxpool.NewWithConfig or
// pgxpool.New. internal/invariants holds every other file to that, so a
// pool that skips the sizing has to be written on purpose and fails a
// test when it is.
func NewPool(ctx context.Context, cfg *PoolConfig) (*pgxpool.Pool, error) {
	return pgxpool.NewWithConfig(ctx, cfg.Config)
}

// Monitoring pool sizes. Two at most, because the heartbeat and the log
// sink write from their own goroutines and neither should queue behind
// the other; one kept open, so a service whose main pools have filled
// PostgreSQL's connection ceiling still has the connection it reports
// that on.
const (
	monitorMaxConns = 2
	monitorMinConns = 1
)

// OpenMonitor opens the pool a service's own monitoring writes through:
// its heartbeat row and the panel's copy of its log.
//
// Separate from the pool the service does its work on, and that is the
// whole point - measured (PLAN §Z4): with the read API's pool held by
// queries for 150 seconds, the heartbeat row did not advance once, and
// the one line saying the heartbeat was blind reached the panel's log
// view zero times out of one. Both waited for a connection behind the
// work they exist to report on, for their five second deadline, and
// dropped. A service that is busy was shown as a service that had gone
// stale, with nothing saying why.
//
// The DSN's pool_max_conns is the operator's size for the work pool and
// is not applied here: this pool's size is not a capacity decision.
func OpenMonitor(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = monitorMaxConns
	cfg.MinConns = monitorMinConns
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Open is ParseConfig followed by NewPool, for the callers that change
// nothing in between - which is all but one of them.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return NewPool(ctx, cfg)
}

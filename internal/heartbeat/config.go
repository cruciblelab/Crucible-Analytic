package heartbeat

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/configreport"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
)

// ConfigReport is one process's row as the setup wizard reads it: whose
// row, when it last beat, and what it said about its configuration.
type ConfigReport struct {
	Service  string
	Instance string
	BeatAt   time.Time
	// Report is empty - Program "" - for a row written by a build older
	// than the column, or by a process that reports nothing.
	Report configreport.Report
}

// Key is the row's key.
func (c ConfigReport) Key() string { return RowKey(c.Service, c.Instance) }

// ReadConfig returns what each running process says about the
// configuration it loaded (PLAN §W4, item 5).
//
// Kept apart from Read on purpose, and only the setup wizard calls it:
// see beatColumns. The rows are the ones Read would return - the same
// forgetReplaced and forgetSilent rules decide which processes are still
// there - so the wizard and the health page agree about which collectors
// exist, and only the wizard learns what they are configured with.
//
// A database without the column - the binary is newer than the schema -
// returns the rows with empty reports rather than an error: the wizard
// then says "not reported", which is true.
func ReadConfig(ctx context.Context, pool *pgxpool.Pool) ([]ConfigReport, error) {
	column := `'{}'::jsonb`
	if has, err := schemaver.HasColumn(ctx, pool, "service_heartbeat", configColumn); err == nil && has {
		column = configColumn
	}
	rows, err := pool.Query(ctx, `
		SELECT service, started_at, beat_at, `+column+`
		FROM service_heartbeat
		ORDER BY beat_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		beats   []Beat
		reports = map[string]configreport.Report{}
	)
	for rows.Next() {
		var (
			b   Beat
			raw []byte
		)
		if err := rows.Scan(&b.Service, &b.StartedAt, &b.BeatAt, &raw); err != nil {
			return nil, err
		}
		key := b.Service
		b.Service, b.Instance = SplitKey(b.Service)
		var report configreport.Report
		// A row whose report will not parse, or names a program this
		// build does not know, reads as one that said nothing: a value
		// the wizard cannot attribute is not one it should show.
		if json.Unmarshal(raw, &report) != nil || !configreport.Known(report.Program) {
			report = configreport.Report{}
		}
		reports[key] = report
		beats = append(beats, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []ConfigReport
	for _, b := range forgetSilent(forgetReplaced(beats)) {
		out = append(out, ConfigReport{
			Service: b.Service, Instance: b.Instance, BeatAt: b.BeatAt,
			Report: reports[b.Key()],
		})
	}
	return out, nil
}

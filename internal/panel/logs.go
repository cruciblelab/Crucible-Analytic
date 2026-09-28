package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// LogLine is one line a service logged, as panel_logs holds it.
type LogLine struct {
	At       time.Time
	Service  string
	Level    string
	Category string
	Message  string
	// Site is the site the line is about, empty for a line about a
	// process.
	Site  string
	Attrs map[string]string
}

// LogFilter chooses what RecentWarnings reads.
type LogFilter struct {
	// Since is how far back.
	Since time.Duration
	// Limit bounds how many lines come back.
	Limit int
	// Sites, when non-nil, is the only sites whose lines may come back;
	// lines about no site always may. Nil is every site - the operator's
	// view. An empty non-nil slice is lines about no site only.
	Sites []string
}

// RecentWarnings reads the WARN and ERROR lines of the recent past,
// newest first, and whether there were more than Limit.
//
// # Whose lines
//
// A line about one customer's site is never shown to another: the
// table's own schema says so, and one deployment can host several
// customers. A line is about a site when its site column says so, or -
// for lines written before 2026-09-28, when the sink's site key did not
// match the key every caller wrote - when its attributes carry the site
// under that key. Both are asked, so an upgraded deployment's older lines
// are filtered as strictly as its new ones.
func (s *Store) RecentWarnings(ctx context.Context, f LogFilter) ([]LogLine, bool, error) {
	var sites any
	if f.Sites != nil {
		sites = f.Sites
	}
	rows, err := s.pool.Query(ctx, `
		SELECT at, service, level, category, message,
		       CASE WHEN site_id <> '' THEN site_id ELSE coalesce(attrs->>'site', '') END,
		       attrs
		  FROM panel_logs
		 WHERE (level LIKE 'WARN%' OR level LIKE 'ERROR%')
		   AND at > now() - make_interval(secs => $1)
		   AND ($2::text[] IS NULL
		        OR ((site_id = '' OR site_id = ANY($2))
		            AND (NOT attrs ? 'site' OR attrs->>'site' = ANY($2))))
		 ORDER BY at DESC, id DESC
		 LIMIT $3`, f.Since.Seconds(), sites, f.Limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("panel: read recent log lines: %w", err)
	}
	defer rows.Close()

	lines := []LogLine{}
	for rows.Next() {
		var l LogLine
		var attrs []byte
		if err := rows.Scan(&l.At, &l.Service, &l.Level, &l.Category, &l.Message, &l.Site, &attrs); err != nil {
			return nil, false, fmt.Errorf("panel: scan log line: %w", err)
		}
		// Every value the sink stores is a string. A blob that is not
		// that shape is carried as no attributes rather than dropping
		// the line: the message is the part most worth having.
		if err := json.Unmarshal(attrs, &l.Attrs); err != nil {
			l.Attrs = map[string]string{}
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(lines) > f.Limit
	if truncated {
		lines = lines[:f.Limit]
	}
	return lines, truncated, nil
}

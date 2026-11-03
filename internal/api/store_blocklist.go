package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// What a proposed country or ASN rule would have refused (B3f, catalog #12).
//
// collector.blocked_countries and collector.blocked_asns refuse every
// connection that matches, for every site on the deployment, with a 403
// before anything is recorded. A wrong entry - a typo, the wrong country
// code, the ASN of a mobile carrier - throws real traffic away and leaves
// no trace of having done so. So before the panel accepts a new entry it
// shows how many addresses that entry would have matched in the last
// day. This is the read behind that sentence.

// BlocklistPreviewWindow is how far back a preview looks.
const BlocklistPreviewWindow = 24 * time.Hour

// MaxBlocklistPreviewRules is how many rules one preview asks about.
//
// The cost is the window's scan, not the rules: on 3M snapshots in a day
// (B3f's ca_b3f) three rules and two hundred cost the same, 1.5-1.9 s
// cold. The cap keeps one request from carrying a pasted list of ten
// thousand ASNs into a query string.
const MaxBlocklistPreviewRules = 200

// BlocklistPreview is one preview.
type BlocklistPreview struct {
	// From and To are the window, on the database's clock.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Addresses is every distinct address the collector recorded in the
	// window, on every site; WithCountry and WithASN are how many of them
	// the lookup placed. A rule can only match a placed address, so these
	// two say how much of the traffic a rule could see at all - zero
	// means the lookup is off or failing, and no rule will block anything.
	Addresses   int64 `json:"addresses"`
	WithCountry int64 `json:"with_country"`
	WithASN     int64 `json:"with_asn"`
	// Countries and ASNs are one entry per rule asked about, in the order
	// asked, zero included.
	Countries []CountryHits `json:"countries"`
	ASNs      []ASNHits     `json:"asns"`
}

// CountryHits is one country rule's addresses.
type CountryHits struct {
	Country   string `json:"country"`
	Addresses int64  `json:"addresses"`
}

// ASNHits is one ASN rule's addresses.
type ASNHits struct {
	ASN       int   `json:"asn"`
	Addresses int64 `json:"addresses"`
}

// BlocklistPreview counts, for each rule, the distinct addresses in the
// last day that it would have matched.
//
// # What it cannot see
//
// An address a rule already in force refuses never reaches the table: the
// 403 comes before the proxy. So this answers "what more would this rule
// refuse", which is the question a new entry raises - and it answers zero
// for an entry that is already on the list, which is why the panel never
// asks it about one.
//
// # The shape, measured
//
// On ca_b3f (3M snapshots in the last 24 hours, 62,472 distinct masked
// addresses, 182 countries, 7,691 ASNs, 40 sites): the distinct (raw
// columns) rows first and the address key built from those, 1.59-1.66 s
// cold, 1.03-1.12 s warm. Building the key on every row first, the shape
// every other query here would suggest, 2.26-2.56 s cold - inet_send per
// row, the O4b lesson. Per-rule counts are a join against the distinct
// rows, so the rule count does not move the cost: 3+3 rules and 100+100
// fall in the same band.
func (s *Store) BlocklistPreview(ctx context.Context, countries []string, asns []int) (BlocklistPreview, error) {
	if countries == nil {
		countries = []string{}
	}
	if asns == nil {
		asns = []int{}
	}
	out := BlocklistPreview{Countries: []CountryHits{}, ASNs: []ASNHits{}}
	var byCountry, byASN []byte
	err := s.pool.QueryRow(ctx, `
		WITH w AS MATERIALIZED (
			SELECT DISTINCT coalesce(ip_hash, inet_send(ip)) AS k, country, asn FROM (
				SELECT DISTINCT ip_hash, ip, country, asn
				FROM traffic_snapshots
				WHERE time >= now() - make_interval(secs => $3)) raw
		)
		SELECT now() - make_interval(secs => $3), now(),
		       (SELECT count(DISTINCT k) FROM w),
		       (SELECT count(DISTINCT k) FROM w WHERE country <> ''),
		       (SELECT count(DISTINCT k) FROM w WHERE asn <> 0),
		       (SELECT coalesce(json_agg(json_build_object('country', c, 'addresses', n) ORDER BY o), '[]')
		          FROM (SELECT c, o, count(DISTINCT w.k) AS n
		                  FROM unnest($1::text[]) WITH ORDINALITY AS r(c, o)
		                  LEFT JOIN w ON w.country = r.c
		                 GROUP BY c, o) x),
		       (SELECT coalesce(json_agg(json_build_object('asn', a, 'addresses', n) ORDER BY o), '[]')
		          FROM (SELECT a, o, count(DISTINCT w.k) AS n
		                  FROM unnest($2::int[]) WITH ORDINALITY AS r(a, o)
		                  LEFT JOIN w ON w.asn = r.a
		                 GROUP BY a, o) y)`,
		countries, asns, BlocklistPreviewWindow.Seconds()).
		Scan(&out.From, &out.To, &out.Addresses, &out.WithCountry, &out.WithASN, &byCountry, &byASN)
	if err != nil {
		return BlocklistPreview{}, fmt.Errorf("api: blocklist preview: %w", err)
	}
	if err := json.Unmarshal(byCountry, &out.Countries); err != nil {
		return BlocklistPreview{}, fmt.Errorf("api: blocklist preview countries: %w", err)
	}
	if err := json.Unmarshal(byASN, &out.ASNs); err != nil {
		return BlocklistPreview{}, fmt.Errorf("api: blocklist preview asns: %w", err)
	}
	return out, nil
}

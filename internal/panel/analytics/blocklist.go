package analytics

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

// BlocklistPreview is what a set of country and ASN rules would have
// turned away in the last day, on every site (B3f, catalog #12).
//
// The read API's answer, decoded by this package's own type; a test
// marshals the producer's type through it, so a renamed field on either
// side fails there instead of decoding as zero - and zero is the
// dangerous reading here: "this rule matches nobody, go ahead".
type BlocklistPreview struct {
	From        time.Time     `json:"from"`
	To          time.Time     `json:"to"`
	Addresses   int64         `json:"addresses"`
	WithCountry int64         `json:"with_country"`
	WithASN     int64         `json:"with_asn"`
	Countries   []CountryHits `json:"countries"`
	ASNs        []ASNHits     `json:"asns"`
}

// CountryHits is one country rule's addresses in the window.
type CountryHits struct {
	Country   string `json:"country"`
	Addresses int64  `json:"addresses"`
}

// ASNHits is one ASN rule's addresses in the window.
type ASNHits struct {
	ASN       int   `json:"asn"`
	Addresses int64 `json:"addresses"`
}

// MaxPreviewRules is how many rules one preview may ask about: the read
// API refuses more (api.MaxBlocklistPreviewRules). Its own constant so
// the panel does not import the API's package; a test holds the two
// equal.
const MaxPreviewRules = 200

// BlocklistPreview asks the read API what these rules would have matched.
//
// The API looks at the last day on its own clock and ignores from and
// to; get sends them because every call does.
func (c *Client) BlocklistPreview(ctx context.Context, countries []string, asns []int) (BlocklistPreview, error) {
	if !c.Configured() {
		return BlocklistPreview{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, PageTimeout)
	defer cancel()
	q := url.Values{}
	for _, c := range countries {
		q.Add("country", c)
	}
	for _, a := range asns {
		q.Add("asn", strconv.Itoa(a))
	}
	var out BlocklistPreview
	now := time.Now()
	err := c.get(ctx, "/api/v1/blocklist/preview", now.Add(-24*time.Hour), now, q, &out)
	return out, err
}

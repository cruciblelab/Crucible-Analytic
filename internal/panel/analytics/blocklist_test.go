package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/api"
)

// TestTheBlocklistPreviewSurvivesTheApisOwnJson ties the decoder to the
// producer's type: every field set to a value no other field has, so a
// name either side changes decodes as zero and fails here. And the rules
// travel as repeated parameters, in the order given.
func TestTheBlocklistPreviewSurvivesTheApisOwnJson(t *testing.T) {
	from := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	body, err := json.Marshal(api.BlocklistPreview{
		From: from, To: from.Add(24 * time.Hour),
		Addresses: 101, WithCountry: 97, WithASN: 89,
		Countries: []api.CountryHits{{Country: "RU", Addresses: 13}, {Country: "CN", Addresses: 0}},
		ASNs:      []api.ASNHits{{ASN: 64512, Addresses: 7}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		path    string
		country []string
		asn     []string
	}
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.country, got.asn = r.URL.Path, r.URL.Query()["country"], r.URL.Query()["asn"]
		_, _ = w.Write(body)
	}))
	p, err := c.BlocklistPreview(context.Background(), []string{"RU", "CN"}, []int{64512})
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/v1/blocklist/preview" || !reflect.DeepEqual(got.country, []string{"RU", "CN"}) ||
		!reflect.DeepEqual(got.asn, []string{"64512"}) {
		t.Errorf("asked %q with country=%q asn=%q", got.path, got.country, got.asn)
	}
	want := BlocklistPreview{
		From: from, To: from.Add(24 * time.Hour), Addresses: 101, WithCountry: 97, WithASN: 89,
		Countries: []CountryHits{{"RU", 13}, {"CN", 0}}, ASNs: []ASNHits{{64512, 7}},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("decoded %+v\nwant    %+v\nfrom %s", p, want, body)
	}
}

// TestThePanelsRuleCapIsTheApis: a panel cap above the API's would send
// a preview the API refuses; one below would leave rules unasked for no
// reason.
func TestThePanelsRuleCapIsTheApis(t *testing.T) {
	if MaxPreviewRules != api.MaxBlocklistPreviewRules {
		t.Errorf("the panel asks about %d rules at most, the API answers %d", MaxPreviewRules, api.MaxBlocklistPreviewRules)
	}
}

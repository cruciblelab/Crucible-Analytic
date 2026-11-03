package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// The deployment-wide half of fakeStore.
func (f *fakeStore) BlocklistPreview(ctx context.Context, countries []string, asns []int) (BlocklistPreview, error) {
	f.gotCall, f.gotCountries, f.gotASNs = "BlocklistPreview", countries, asns
	if f.err != nil {
		return BlocklistPreview{}, f.err
	}
	out := BlocklistPreview{Addresses: 10, WithCountry: 9, WithASN: 8}
	for _, c := range countries {
		out.Countries = append(out.Countries, CountryHits{Country: c, Addresses: 1})
	}
	for _, a := range asns {
		out.ASNs = append(out.ASNs, ASNHits{ASN: a, Addresses: 2})
	}
	return out, nil
}

// TestTheBlocklistPreviewReachesTheStoreAsAsked.
//
// Codes in any case become upper case, repeats are asked about once, and
// the order asked is the order passed - the panel draws the rows in the
// order the person typed them.
func TestTheBlocklistPreviewReachesTheStoreAsAsked(t *testing.T) {
	store := &fakeStore{}
	h := newTestServer(t, store)

	w := do(h, http.MethodGet, "/api/v1/blocklist/preview?country=ru&country=CN&country=RU&asn=64512&asn=13335&asn=64512",
		"panel-secret")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if store.gotCall != "BlocklistPreview" {
		t.Fatalf("the route reached %q", store.gotCall)
	}
	if !reflect.DeepEqual(store.gotCountries, []string{"RU", "CN"}) {
		t.Errorf("countries = %q, want [RU CN]", store.gotCountries)
	}
	if !reflect.DeepEqual(store.gotASNs, []int{64512, 13335}) {
		t.Errorf("asns = %v, want [64512 13335]", store.gotASNs)
	}
	var got BlocklistPreview
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Addresses != 10 || got.WithCountry != 9 || got.WithASN != 8 || len(got.Countries) != 2 || len(got.ASNs) != 2 {
		t.Errorf("the answer did not carry the store's: %+v", got)
	}
}

// TestAScopedTokenIsRefusedTheBlocklistPreview.
//
// The answer counts every site; a token for some sites gets 403 and the
// store is never asked - not a preview of its own sites under the
// deployment's name.
func TestAScopedTokenIsRefusedTheBlocklistPreview(t *testing.T) {
	store := &fakeStore{}
	h := newTestServer(t, store)

	w := do(h, http.MethodGet, "/api/v1/blocklist/preview?country=RU", "ahmet-secret")
	if w.Code != http.StatusForbidden {
		t.Errorf("a token scoped to site-a got %d, want 403: %s", w.Code, w.Body)
	}
	if store.gotCall != "" {
		t.Errorf("the store was asked (%s) for a token that may not see the answer", store.gotCall)
	}
	if w := do(h, http.MethodGet, "/api/v1/blocklist/preview", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token got %d, want 401", w.Code)
	}
}

// TestTheBlocklistPreviewRefusesWhatNoRuleCouldBe.
//
// Each refusal is a value the panel would never store; and the rule cap
// is held from both sides, at the constant and one past it.
func TestTheBlocklistPreviewRefusesWhatNoRuleCouldBe(t *testing.T) {
	for _, q := range []string{
		"country=R", "country=RUS", "country=R1", "country=", "country=%C3%9CR",
		"asn=0", "asn=-5", "asn=AS13335", "asn=", "asn=2147483648",
	} {
		t.Run(q, func(t *testing.T) {
			store := &fakeStore{}
			w := do(newTestServer(t, store), http.MethodGet, "/api/v1/blocklist/preview?"+q, "panel-secret")
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s answered %d, want 400", q, w.Code)
			}
			if store.gotCall != "" {
				t.Errorf("%s reached the store", q)
			}
		})
	}

	rules := func(n int) string {
		v := url.Values{}
		for i := range n {
			v.Add("asn", fmt.Sprint(i+1))
		}
		return v.Encode()
	}
	h := newTestServer(t, &fakeStore{})
	if w := do(h, http.MethodGet, "/api/v1/blocklist/preview?"+rules(200), "panel-secret"); w.Code != http.StatusOK {
		t.Errorf("200 rules answered %d, want 200: %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodGet, "/api/v1/blocklist/preview?"+rules(201), "panel-secret"); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "200") {
		t.Errorf("201 rules answered %d, want 400 naming the cap: %s", w.Code, w.Body)
	}
	if MaxBlocklistPreviewRules != 200 {
		t.Errorf("MaxBlocklistPreviewRules = %d; the two requests above were written for 200", MaxBlocklistPreviewRules)
	}
}

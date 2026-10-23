package beacon

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The live site list keeps every entry that can be a key, and says which
// it left out (PLAN §H8).
//
// The list comes from the panel's settings table, which took any string
// until H8 - and a site the beacon accepts is a site_id it writes rows
// under, into every index keyed on it. An entry past 2704 bytes would
// fail every batch an event naming it landed in. Asked through the
// handler as well as through the return value: a SetSites that reported
// the refusal and stored the list anyway would pass the first half.
//
// "bir site" stays: the configuration files' pattern refuses a space,
// and a deployment that has been recording under such an entry must not
// stop on the day it upgrades (internal/siteid says why). It is here so
// that a SetSites holding the list to the pattern fails this test.
func TestTheLiveSiteListKeepsOnlySiteIdentifiers(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	long := strings.Repeat("x", 3000)

	refused := s.SetSites([]string{"magaza", "bir site", long, "kontrol\x01", "diger"})
	if want := []string{long, "kontrol\x01"}; !slices.Equal(refused, want) {
		t.Errorf("SetSites refused %d entries, want the two that cannot be a key", len(refused))
	}
	for site, want := range map[string]int{
		"magaza":   http.StatusNoContent,
		"diger":    http.StatusNoContent,
		"bir site": http.StatusNoContent,
		long:       http.StatusForbidden,
	} {
		if w := post(t, s, eventFor(site)); w.Code != want {
			t.Errorf("an event for %.12q... answered %d, want %d", site, w.Code, want)
		}
	}
	if len(sink.rows) != 3 {
		t.Errorf("%d rows reached the sink, want the three keyable sites'", len(sink.rows))
	}
	if refused := s.SetSites([]string{"magaza"}); len(refused) != 0 {
		t.Errorf("a clean list reported %d refusals", len(refused))
	}
}

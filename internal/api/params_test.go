package api

import (
	"net/http"
	"testing"
)

// TestParseTimezone.
//
// The zone reaches SQL, so what it may be is worth stating. It is a
// bound parameter, so injection is not the risk; a name PostgreSQL does
// not know is, because it would surface as a 500 from deep inside a
// query rather than as a sentence naming the parameter.
func TestParseTimezone(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: "", want: DefaultTimezone},
		{raw: "UTC", want: "UTC"},
		{raw: "Europe/Istanbul", want: "Europe/Istanbul"},
		{raw: "America/New_York", want: "America/New_York"},
		{raw: "Mars/Olympus", wantErr: true},
		// "Local" is the one name Go accepts and PostgreSQL cannot, and
		// the one a caller sends by accident: any time.Time built from
		// time.Now() without In() carries it.
		{raw: "Local", wantErr: true},
	} {
		got, err := ParseTimezone(tc.raw)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("ParseTimezone(%q) = %q, want an error", tc.raw, got)
		case !tc.wantErr && err != nil:
			t.Errorf("ParseTimezone(%q): %v", tc.raw, err)
		case !tc.wantErr && got != tc.want:
			t.Errorf("ParseTimezone(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestAnUnknownTimezoneIsRefusedByTheHandlerNotTheDatabase.
//
// A 400 naming the parameter rather than a 500 from a query. The
// difference matters to whoever is reading the log at the time.
func TestAnUnknownTimezoneIsRefusedByTheHandlerNotTheDatabase(t *testing.T) {
	store := &fakeStore{}
	srv := newTestServer(t, store)

	for _, path := range []string{
		"/api/v1/sites/bir/timeseries?tz=Mars/Olympus",
		"/api/v1/sites/bir/beacon/timeseries?tz=Local",
	} {
		rec := do(srv, http.MethodGet, path, "panel-secret")

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", path, rec.Code)
		}
		if store.gotZone != "" {
			t.Errorf("%s reached the store with tz=%q; a zone the handler refused must "+
				"not become a query", path, store.gotZone)
		}
	}
}

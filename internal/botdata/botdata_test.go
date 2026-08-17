package botdata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMissingFileIsAnEmptySetNotAnError is the contract the licence
// decision rests on. This repository ships no copy of the dataset, so a
// deployment that has never fetched is ordinary, not broken - and if
// that were an error, every caller would have a branch it was tempted to
// ignore.
func TestMissingFileIsAnEmptySetNotAnError(t *testing.T) {
	set, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("a missing file returned an error: %v", err)
	}
	if set.Len() != 0 {
		t.Errorf("a missing file produced %d fingerprints", set.Len())
	}
	if set.Fetched() {
		t.Error("a missing file claimed to have been fetched")
	}
	if set.Labels == nil {
		t.Error("Labels is nil; callers should never have to check")
	}
}

func TestUnsetPathIsAlsoAnEmptySet(t *testing.T) {
	set, err := Load("")
	if err != nil || set.Fetched() || set.Len() != 0 {
		t.Fatalf("Load(\"\") = %+v, %v", set, err)
	}
}

// TestUnparsableFileIsAnError, because a file somebody put there and a
// file that does not exist are different facts. Treating a truncated
// download as "no data" would hide it forever.
func TestUnparsableFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.json")
	if err := os.WriteFile(path, []byte(`{"entries": [ truncated`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a truncated file loaded as if it were fine")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the file: %v", err)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "bot.json")
	when := time.Date(2026, time.August, 17, 9, 0, 0, 0, time.UTC)
	want := Set{
		Labels:    map[string]string{"ja4-a": "curl", "ja4-b": "python-requests"},
		Source:    "https://example.test/archive",
		FetchedAt: when,
		Dropped:   7,
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Len() != 2 || got.Labels["ja4-a"] != "curl" {
		t.Errorf("labels = %v", got.Labels)
	}
	if got.Source != want.Source {
		t.Errorf("source = %q", got.Source)
	}
	if !got.FetchedAt.Equal(when) {
		t.Errorf("fetched at %v, want %v", got.FetchedAt, when)
	}
	if got.Dropped != 7 {
		t.Errorf("dropped = %d", got.Dropped)
	}
	if !got.Fetched() {
		t.Error("a saved set does not report itself as fetched")
	}
}

// TestSavedFileExplainsItself: somebody finding this on a server a year
// from now should be able to tell what it is, where it came from, and
// that this project does not claim to own it - without reading any
// source code.
func TestSavedFileExplainsItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.json")
	if err := Save(path, Set{Labels: map[string]string{"a": "b"}, Source: "https://example.test"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("the file we wrote does not parse: %v", err)
	}
	for _, want := range []string{"does not redistribute", "update-bot-data", "source's own terms"} {
		if !strings.Contains(f.Note, want) {
			t.Errorf("the note does not mention %q: %q", want, f.Note)
		}
	}
	if f.Source == "" || f.RetrievedAt == "" {
		t.Error("the file does not record where it came from or when")
	}
}

// TestSaveIsAtomic: a crash mid-write must not leave a half-written file
// that the collector refuses to parse at its next restart.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.json")
	if err := Save(path, Set{Labels: map[string]string{"a": "first"}}); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, Set{Labels: map[string]string{"a": "second"}}); err != nil {
		t.Fatal(err)
	}
	set, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if set.Labels["a"] != "second" {
		t.Errorf("the second save did not replace the first: %v", set.Labels)
	}
	// No temporary files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".botdata-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestSaveRefusesAnEmptyPath(t *testing.T) {
	if err := Save("", Set{Labels: map[string]string{"a": "b"}}); err == nil {
		t.Fatal("Save accepted an empty path")
	}
}

const sampleArchive = `[
  {"ja4":"t13d-curl","bot_type":"http_client","name":"curl","user_agent":"curl/8.5.0","submission_count":9},
  {"ja4":"t13d-curl","bot_type":"scraper","name":"some-scraper","submission_count":3},
  {"ja4":"t13d-chrome","bot_type":"browser","name":"Chrome 120","submission_count":400},
  {"ja4":"","bot_type":"scanner","name":"nameless","submission_count":1},
  {"ja4":"t13d-python","bot_type":"http_client","name":"python-requests","submission_count":5}
]`

func TestFetchFiltersWhatMustNotBeABotSignal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "crucible-analytic") {
			t.Errorf("the fetcher did not identify itself: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleArchive))
	}))
	defer server.Close()

	set, err := Fetch(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// A browser fingerprint in the set would make the panel call every
	// ordinary visitor a known bot. This is the single most important
	// thing this parser does.
	if _, ok := set.Labels["t13d-chrome"]; ok {
		t.Error("a browser-classified entry survived the filter")
	}
	// "" is the sentinel for "no JA4 available"; keeping it would flag
	// all non-TLS traffic.
	if _, ok := set.Labels[""]; ok {
		t.Error("an empty fingerprint survived the filter")
	}
	if set.Len() != 2 {
		t.Errorf("kept %d fingerprints, want 2: %v", set.Len(), set.Labels)
	}
	if set.Dropped != 2 {
		t.Errorf("dropped %d, want 2", set.Dropped)
	}
	if !set.Fetched() {
		t.Error("a fetched set does not report itself as fetched")
	}
	if set.Source != server.URL {
		t.Errorf("source = %q", set.Source)
	}
}

// TestOneFingerprintManyTools: JA4 fingerprints the TLS stack, not the
// application, so many different scripts built on one curl build share a
// fingerprint. Merging keeps that visible rather than letting whichever
// row arrived last win.
func TestOneFingerprintManyTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleArchive))
	}))
	defer server.Close()

	set, err := Fetch(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Labels["t13d-curl"]; got != "curl" {
		t.Errorf("the merged entry is labelled %q, want the first label", got)
	}
}

func TestFetchAcceptsAWrappedPayload(t *testing.T) {
	for name, body := range map[string]string{
		"entries": `{"entries": ` + sampleArchive + `}`,
		"data":    `{"data": ` + sampleArchive + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			set, err := Fetch(context.Background(), server.Client(), server.URL)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if set.Len() != 2 {
				t.Errorf("kept %d fingerprints", set.Len())
			}
		})
	}
}

func TestFetchRefusesWhatItCannotUse(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"not found":       {http.StatusNotFound, "", "HTTP 404"},
		"not json":        {http.StatusOK, "<html>maintenance</html>", "unrecognised response"},
		"empty array":     {http.StatusOK, `[]`, "returned no entries"},
		"all filtered":    {http.StatusOK, `[{"ja4":"x","bot_type":"browser"}]`, "all 1 entries were filtered out"},
		"empty object":    {http.StatusOK, `{}`, "no entries"},
		"wrong shape":     {http.StatusOK, `{"result": "ok"}`, "no entries"},
		"array of number": {http.StatusOK, `[1,2,3]`, "unrecognised response"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
					return
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := Fetch(context.Background(), server.Client(), server.URL)
			if err == nil {
				t.Fatalf("Fetch accepted %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestFetchIsBounded: without a cap, a redirect to something enormous
// turns a cron job into an out-of-memory kill on the machine that also
// runs the collector.
func TestFetchIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat("a", 1<<20)
		for written := 0; written <= maxDownloadBytes; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	_, err := Fetch(context.Background(), server.Client(), server.URL)
	if err == nil {
		t.Fatal("Fetch read an unbounded response")
	}
	if !strings.Contains(err.Error(), "more than") {
		t.Fatalf("error %q does not say what the limit was", err)
	}
}

// TestUpdateWritesNothingWhenTheFetchFails. A failed refresh must leave
// the previous data in place: yesterday's fingerprints are worth far
// more than none.
func TestUpdateWritesNothingWhenTheFetchFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.json")
	if err := Save(path, Set{Labels: map[string]string{"kept": "yesterday"}}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if _, err := Update(context.Background(), server.Client(), server.URL, path); err == nil {
		t.Fatal("Update reported success on a failing source")
	}
	set, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if set.Labels["kept"] != "yesterday" {
		t.Errorf("a failed update destroyed the previous data: %v", set.Labels)
	}
}

func TestUpdateWritesWhatItFetched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleArchive))
	}))
	defer server.Close()

	set, err := Update(context.Background(), server.Client(), server.URL, path)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Len() != set.Len() {
		t.Errorf("wrote %d fingerprints, loaded %d", set.Len(), loaded.Len())
	}
}

// TestTwoRunsOverUnchangedDataProduceTheSameFile, so an operator can
// diff one against the last and see whether anything actually moved.
func TestTwoRunsOverUnchangedDataProduceTheSameFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleArchive))
	}))
	defer server.Close()

	dir := t.TempDir()
	when := time.Date(2026, time.August, 17, 9, 0, 0, 0, time.UTC)
	var bodies []string
	for _, name := range []string{"one.json", "two.json"} {
		set, err := Fetch(context.Background(), server.Client(), server.URL)
		if err != nil {
			t.Fatal(err)
		}
		// Pin the timestamp; it is the one field that legitimately
		// differs between runs.
		set.FetchedAt = when
		path := filepath.Join(dir, name)
		if err := Save(path, set); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(raw))
	}
	if bodies[0] != bodies[1] {
		t.Error("two runs over identical data produced different files; a diff would be useless")
	}
}

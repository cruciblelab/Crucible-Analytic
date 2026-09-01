// Package botdata fetches and stores the known-bot JA4 fingerprint set.
//
// This repository ships no copy of that data, and that is a licence
// decision rather than an oversight. The project's own code is MIT and
// anyone may take it; a third-party dataset carried inside it would come
// with somebody else's terms, and a repository that says "MIT, help
// yourself" while containing data under unstated conditions passes that
// uncertainty to everybody who clones it.
//
// So the data is fetched by the deployment, onto the deployment's own
// machine, under the source's own terms. What ships here is the
// mechanism.
//
// How that mechanism is scheduled is deliberately not this package's
// business: it is a plain function behind a command-line flag, so a
// deployment can put it in cron, run it by hand, or drive it from
// somewhere else later. The only requirement this package imposes is
// that never having run it is a supported state - see Load, where a
// missing file is an empty set rather than an error.
package botdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultSourceURL is where the fingerprint archive is published.
//
// Configurable, because pinning a third party's URL into a binary is how
// a deployment becomes unable to update itself the day that host moves.
const DefaultSourceURL = "https://thebotaquarium.com/api/fingerprint/archive"

// FetchTimeout bounds one update run. Generous for a small JSON file,
// short enough that a cron entry cannot pile up behind a hung request.
const FetchTimeout = 60 * time.Second

// maxDownloadBytes caps what will be read from the source.
//
// The archive is tens of kilobytes. This is not a guess at its size, it
// is a bound on somebody else's server: without it a redirect to
// something enormous turns an update into an out-of-memory kill on the
// machine that also runs the collector.
const maxDownloadBytes = 32 << 20

// Set is the loaded data: JA4 fingerprint to a human-readable label,
// plus where it came from.
//
// The provenance travels with the data rather than beside it, because
// the first question anybody asks about a file like this is "what is
// this and when did it arrive", and the second is "am I allowed to have
// it".
type Set struct {
	// Labels maps a JA4 fingerprint to its label. Never nil.
	Labels map[string]string
	// Source is the URL it was fetched from.
	Source string
	// FetchedAt is when. Zero means never - see Fetched.
	FetchedAt time.Time
	// Dropped counts entries the filter removed, so an operator can tell
	// "the source had 12 rows" from "the source had 60 and we kept 12".
	Dropped int
}

// Fetched reports whether this set came from a real fetch, as opposed to
// being the empty set a deployment has before it ever ran an update.
//
// The distinction matters enough to be a method: "nobody has fetched
// this yet" and "the source returned nothing" look identical in a count
// and mean entirely different things to whoever has to act on it.
func (s Set) Fetched() bool { return !s.FetchedAt.IsZero() }

// Len reports how many fingerprints are known.
func (s Set) Len() int { return len(s.Labels) }

// Empty returns a set with no fingerprints, which is what a deployment
// that has never updated has.
func Empty() Set { return Set{Labels: map[string]string{}} }

// file is the on-disk format. Deliberately self-describing: somebody
// finding this file on a server a year from now should be able to tell
// what it is and where it came from without this source code.
type file struct {
	Source      string  `json:"source"`
	RetrievedAt string  `json:"retrieved_at"`
	Note        string  `json:"note"`
	Dropped     int     `json:"dropped_entries"`
	Entries     []entry `json:"entries"`
}

type entry struct {
	JA4              string   `json:"ja4"`
	BotTypes         []string `json:"bot_types"`
	Label            string   `json:"label"`
	ExampleUserAgent string   `json:"example_user_agent"`
	SubmissionCount  int      `json:"submission_count"`
}

// fileNote is written into every file this package produces.
const fileNote = "Fetched by crucible-analytic (collector -update-bot-data). " +
	"This project does not redistribute this dataset; it is retrieved by " +
	"the deployment from the source named above, under that source's own " +
	"terms. Entries classified as browsers are dropped: including them " +
	"would flag real browsers as known bots. Entries sharing one JA4 are " +
	"merged, because JA4 fingerprints the TLS stack rather than the " +
	"application."

// Load reads a set from path.
//
// A missing file returns the empty set and no error. That is the whole
// point of this package's contract: a deployment that has never run an
// update is a supported, ordinary state, not a broken one. The caller
// finds out by asking Set.Fetched, not by handling an error it would be
// tempted to ignore.
//
// A file that exists but cannot be parsed *is* an error. That is a
// different fact - somebody put something there - and quietly treating
// it as "no data" would hide a truncated download forever.
func Load(path string) (Set, error) {
	if path == "" {
		return Empty(), nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Empty(), nil
	}
	if err != nil {
		return Empty(), fmt.Errorf("botdata: read %s: %w", path, err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return Empty(), fmt.Errorf("botdata: parse %s: %w (delete it and run the update again)", path, err)
	}
	set := Set{
		Labels:  labelsOf(f.Entries),
		Source:  f.Source,
		Dropped: f.Dropped,
	}
	if f.RetrievedAt != "" {
		when, err := time.Parse(time.RFC3339, f.RetrievedAt)
		if err != nil {
			return Empty(), fmt.Errorf("botdata: %s has an unreadable retrieved_at %q: %w", path, f.RetrievedAt, err)
		}
		set.FetchedAt = when
	}
	return set, nil
}

// labelsOf turns entries into the lookup map, dropping the ones that
// must never be in it.
func labelsOf(entries []entry) map[string]string {
	labels := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.JA4 == "" {
			// "" is the sentinel every caller uses for "no JA4 was
			// available". An empty-string key here would label all
			// non-TLS and unparseable traffic as a known bot.
			continue
		}
		labels[e.JA4] = e.Label
	}
	return labels
}

// Fetch downloads the archive and returns the filtered set. It does not
// write anything; Save does that.
func Fetch(ctx context.Context, client *http.Client, sourceURL string) (Set, error) {
	if sourceURL == "" {
		sourceURL = DefaultSourceURL
	}
	if client == nil {
		client = &http.Client{Timeout: FetchTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return Empty(), fmt.Errorf("botdata: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Identifying the client is basic manners toward a free service, and
	// it is what lets them tell us apart if this ever misbehaves.
	req.Header.Set("User-Agent", "crucible-analytic/botdata (+https://github.com/cruciblelab/Crucible-Analytic)")

	resp, err := client.Do(req)
	if err != nil {
		return Empty(), fmt.Errorf("botdata: fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Empty(), fmt.Errorf("botdata: fetch %s: HTTP %d", sourceURL, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return Empty(), fmt.Errorf("botdata: read %s: %w", sourceURL, err)
	}
	if len(raw) > maxDownloadBytes {
		return Empty(), fmt.Errorf("botdata: %s returned more than %d bytes", sourceURL, maxDownloadBytes)
	}

	entries, dropped, err := parseArchive(raw)
	if err != nil {
		return Empty(), fmt.Errorf("botdata: %s: %w", sourceURL, err)
	}
	return Set{
		Labels:    labelsOf(entries),
		Source:    sourceURL,
		FetchedAt: time.Now().UTC(),
		Dropped:   dropped,
	}, nil
}

// archiveEntry is the source's shape, which is not ours: it is somebody
// else's API and may carry fields we do not use or gain ones we do not
// know about. Decoding into a narrow struct rather than a map means a
// field appearing upstream cannot change how this behaves.
type archiveEntry struct {
	JA4              string `json:"ja4"`
	BotType          string `json:"bot_type"`
	Label            string `json:"label"`
	Name             string `json:"name"`
	ExampleUserAgent string `json:"user_agent"`
	SubmissionCount  int    `json:"submission_count"`
}

// parseArchive reads the source's response, filters it, and merges
// entries that share a fingerprint.
//
// Accepts either a bare array or an object with an "entries" or "data"
// array, because a source we do not control is free to wrap its payload
// and a rewrite of this parser should not be the cost of that.
func parseArchive(raw []byte) ([]entry, int, error) {
	var direct []archiveEntry
	if err := json.Unmarshal(raw, &direct); err == nil {
		return filterArchive(direct)
	}
	var wrapped struct {
		Entries []archiveEntry `json:"entries"`
		Data    []archiveEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, 0, fmt.Errorf("unrecognised response: %w", err)
	}
	if len(wrapped.Entries) > 0 {
		return filterArchive(wrapped.Entries)
	}
	if len(wrapped.Data) > 0 {
		return filterArchive(wrapped.Data)
	}
	return nil, 0, errors.New("response contained no entries")
}

// browserType is the classification that must never become a bot
// signal. The archive is community-submitted and includes reference
// fingerprints for real browsers; keeping those would make the panel
// call every ordinary visitor a known bot.
const browserType = "browser"

func filterArchive(entries []archiveEntry) ([]entry, int, error) {
	merged := map[string]*entry{}
	dropped := 0
	for _, e := range entries {
		if e.JA4 == "" || e.BotType == browserType {
			dropped++
			continue
		}
		label := e.Label
		if label == "" {
			label = e.Name
		}
		if label == "" {
			label = e.BotType
		}
		existing, seen := merged[e.JA4]
		if !seen {
			merged[e.JA4] = &entry{
				JA4:              e.JA4,
				BotTypes:         compactTypes(e.BotType),
				Label:            label,
				ExampleUserAgent: e.ExampleUserAgent,
				SubmissionCount:  e.SubmissionCount,
			}
			continue
		}
		// One JA4, several submitted tools: JA4 fingerprints the TLS
		// stack, not the application, so many scripts built on the same
		// curl build share one. Merging keeps that visible instead of
		// letting whichever row came last win.
		existing.BotTypes = mergeTypes(existing.BotTypes, e.BotType)
		existing.SubmissionCount += e.SubmissionCount
		if existing.ExampleUserAgent == "" {
			existing.ExampleUserAgent = e.ExampleUserAgent
		}
	}
	if len(merged) == 0 {
		// Two different failures, two different next actions, so two
		// different sentences. "The source sent nothing" is somebody
		// else's outage; "we filtered everything out" is our parser
		// meeting a shape it did not expect, and only the second is
		// worth reading this code over.
		if len(entries) == 0 {
			return nil, 0, errors.New("the source returned no entries")
		}
		return nil, dropped, fmt.Errorf(
			"all %d entries were filtered out; the source's shape may have changed", len(entries))
	}

	out := make([]entry, 0, len(merged))
	for _, e := range merged {
		sort.Strings(e.BotTypes)
		out = append(out, *e)
	}
	// Sorted so two runs over unchanged data produce an identical file,
	// which is what lets an operator diff one against the last.
	sort.Slice(out, func(i, j int) bool { return out[i].JA4 < out[j].JA4 })
	return out, dropped, nil
}

func compactTypes(botType string) []string {
	if botType == "" {
		return nil
	}
	return []string{botType}
}

func mergeTypes(existing []string, botType string) []string {
	if botType == "" {
		return existing
	}
	for _, have := range existing {
		if have == botType {
			return existing
		}
	}
	return append(existing, botType)
}

// Writable reports whether Save could write to path, by doing what Save
// does short of writing.
//
// # Why a probe and not a permission check
//
// The failure this exists to name is systemd's ProtectSystem=strict,
// which turns a mount read-only underneath a directory whose mode still
// says 0755 and whose owner is still this very user. Every check short
// of attempting the write agrees the write will succeed. So it is
// attempted, and the temporary file removed again.
//
// The directory rather than the file, which is not a shortcut but what
// Save's rename actually needs: replacing a file takes write permission
// on its directory, not on the file.
//
// # Why it lives here
//
// Beside Save, sharing its first two steps. A copy of them in cmd/
// would be answering a question about a mechanism it does not own, and
// would go on answering it confidently after that mechanism changed.
// TestWritableAgreesWithSave holds the two together.
func Writable(path string) error {
	if path == "" {
		return errors.New("botdata: no path to save to")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("botdata: create %s: %w", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".botdata-probe-*")
	if err != nil {
		return fmt.Errorf("botdata: temp file in %s: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// Save writes a set to path.
//
// Written to a temporary file in the same directory and renamed, so a
// crash or a full disk mid-write cannot leave a half-written file that
// the collector would refuse to parse at its next restart. Same
// directory because rename is only atomic within one file system.
func Save(path string, set Set) error {
	if path == "" {
		return errors.New("botdata: no path to save to")
	}
	entries := make([]entry, 0, len(set.Labels))
	for ja4, label := range set.Labels {
		entries = append(entries, entry{JA4: ja4, Label: label})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].JA4 < entries[j].JA4 })

	when := set.FetchedAt
	if when.IsZero() {
		when = time.Now().UTC()
	}
	body, err := json.MarshalIndent(file{
		Source:      set.Source,
		RetrievedAt: when.UTC().Format(time.RFC3339),
		Note:        fileNote,
		Dropped:     set.Dropped,
		Entries:     entries,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("botdata: encode: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("botdata: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".botdata-*.json")
	if err != nil {
		return fmt.Errorf("botdata: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("botdata: write %s: %w", tmpName, err)
	}
	// Sync before rename: without it the rename can land while the
	// contents are still in the page cache, and a power loss leaves an
	// empty file where a valid one used to be.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("botdata: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("botdata: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("botdata: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("botdata: rename into %s: %w", path, err)
	}
	return nil
}

// Update fetches and saves in one call, returning what it stored. This
// is what the command-line flag runs, and what a scheduled job calls.
func Update(ctx context.Context, client *http.Client, sourceURL, path string) (Set, error) {
	set, err := Fetch(ctx, client, sourceURL)
	if err != nil {
		return Empty(), err
	}
	if err := Save(path, set); err != nil {
		return Empty(), err
	}
	return set, nil
}

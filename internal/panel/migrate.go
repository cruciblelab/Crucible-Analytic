package panel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/BurntSushi/toml"
	"github.com/jackc/pgx/v5"
)

// Moving a setting out of a config file without losing what the file
// said.
//
// The problem this solves is small and easy to get badly wrong. A
// deployment has been running for a year with max_concurrent = 4000 in
// its file because the default of 1000 was not enough. The setting moves
// to the panel. On the next upgrade the value has to still be 4000 -
// not the default, not silently, not "until somebody notices the graphs
// changed shape".
//
// # Three layers, each narrower than the last
//
// A service resolves a value as: the stored row if there is one, else
// the config file's value, else the built-in default. The file never
// stops being the fallback, which is what makes an unreachable settings
// table a non-event rather than a silent reset - the rule internal/settings
// states in its own package comment.
//
// The migration writes the row once. After that the row wins, so editing
// the file changes nothing: the plan's phrase for this is "ignored from
// the file thereafter", and it is true in effect rather than by deleting
// anything. Nothing is deleted, and that is deliberate - a migration
// that edits somebody's config file is a migration that can corrupt it.
//
// # Why this is a shell command and not something a service does
//
// The collector's database role may only SELECT on panel_settings, and
// widening that is not a trade worth making: a service that could write
// this table could change the retention period and the IP storage mode,
// which are the two settings sitting behind the developer password
// precisely because they carry legal weight.
//
// So the migration runs as the panel, from a shell, once - the same
// shape as applying the schema, minting a developer link, or minting an
// owner invitation. Work that needs authority the service does not have
// is work a person does at a prompt.

// migratable is one value the migration knows how to move: where it
// lives in a TOML file, and which setting it becomes.
type migratable struct {
	// path is the TOML location, outermost first. Two elements means a
	// key inside a table.
	path []string
	key  Key
	// convert reshapes the file's value into the one the setting takes,
	// for the entries where the two genuinely differ.
	//
	// Only one shape does so far: a config file writes ASNs as numbers
	// (blocked_asns = [64512]) and the settings column is text, so the
	// registry's kind is a string list. Loosening the validator to
	// accept numbers into any string list would have been the smaller
	// diff and the wrong one - it would let a number become a string
	// silently for every setting, everywhere, to spare one conversion
	// here.
	//
	// Nil means the file's value goes straight to Validate.
	convert func(any) any
}

// asnsAsText turns a TOML list of numbers into the list of text the
// settings column holds.
//
// Anything that is not a whole number is passed through untouched, so
// the registry's own validator is what refuses it and the operator gets
// that message rather than a silently dropped entry.
func asnsAsText(value any) any {
	list, ok := value.([]any)
	if !ok {
		return value
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		switch n := item.(type) {
		case int64:
			out = append(out, strconv.FormatInt(n, 10))
		case int:
			out = append(out, strconv.Itoa(n))
		case string:
			out = append(out, n)
		default:
			return value
		}
	}
	return out
}

// migrations maps each service's config file to the settings that have
// moved out of it.
//
// Written per service rather than as one table because the same setting
// has different names in different files: the collector's limit counts
// connections and the beacon's counts requests, and they are genuinely
// different numbers rather than one number spelled two ways.
var migrations = map[string][]migratable{
	"collector": {
		{path: []string{"limits", "max_concurrent_connections"}, key: KeyCollectorMaxConcurrent},
		{path: []string{"limits", "max_requests_per_second"}, key: KeyCollectorMaxPerSecond},
		{path: []string{"limits", "overload_policy"}, key: KeyCollectorOverloadPolicy},
		{path: []string{"limits", "throttle_queue_size"}, key: KeyCollectorThrottleQueue},

		// A5.2. The two blocklists and the scoring signal.
		{path: []string{"asn_lookup", "blocked_countries"}, key: KeyBlockedCountries},
		{path: []string{"asn_lookup", "blocked_asns"}, key: KeyBlockedASNs, convert: asnsAsText},
		{path: []string{"asn_lookup", "known_bot_asns"}, key: KeyKnownBotASNs, convert: asnsAsText},
		{path: []string{"asn_lookup", "apply_to_scoring"}, key: KeyApplyASNToScoring},
	},
	"beacon": {
		{path: []string{"trusted_proxies"}, key: KeyTrustedProxies},
		{path: []string{"limits", "max_concurrent_requests"}, key: KeyBeaconMaxConcurrent},
		{path: []string{"limits", "max_requests_per_second"}, key: KeyBeaconMaxPerSecond},
		{path: []string{"limits", "overload_policy"}, key: KeyBeaconOverloadPolicy},
		{path: []string{"limits", "throttle_queue_size"}, key: KeyBeaconThrottleQueue},
	},
}

// MigratableServices lists the services this command understands, for
// the usage message.
func MigratableServices() []string {
	out := make([]string, 0, len(migrations))
	for name := range migrations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ErrUnknownService is returned for a service with no migration table.
var ErrUnknownService = errors.New("panel: no settings have moved out of that service's config file")

// MigrationOutcome is what happened to one key.
type MigrationOutcome struct {
	Key Key
	// From is the TOML path the value was read from, for the report and
	// the audit entry. A person reading either should be able to go and
	// look at the line.
	From string
	// Value is what was read, when there was something to read.
	Value any
	// Written is true when a row was created.
	Written bool
	// Skipped says why nothing was written, in English, for the report.
	// Empty when Written.
	Skipped string
}

// MigrateSettings copies the values a config file still carries into the
// settings table.
//
// Three rules, in this order, and the first is the one that matters:
//
//   - **An existing row is never overwritten.** Undoing a value somebody
//     set in the panel, using a line they forgot was in a file, is the
//     worst thing this command could do - and it would be invisible,
//     because the panel would go on showing the setting as configurable
//     while showing a number nobody chose.
//   - A value the registry refuses is reported and skipped, not written.
//     The migration is not a way around validation.
//   - Every row it does write produces an audit entry naming the file.
//
// The file is parsed generically rather than through each service's own
// config loader. That keeps the panel's binary from linking the beacon's
// HTTP server, and it keeps this command honest about what it does: it
// reads the keys it knows, it does not validate somebody's whole
// deployment.
func (s *Store) MigrateSettings(ctx context.Context, service, path string) ([]MigrationOutcome, error) {
	entries, ok := migrations[service]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownService, service)
	}

	var file map[string]any
	if _, err := toml.DecodeFile(path, &file); err != nil {
		return nil, fmt.Errorf("panel: reading %s: %w", path, err)
	}

	outcomes := make([]MigrationOutcome, 0, len(entries))
	for _, entry := range entries {
		outcome := MigrationOutcome{Key: entry.key, From: tomlPath(entry.path)}

		raw, found := lookupTOML(file, entry.path)
		if !found {
			outcome.Skipped = "the file does not set it"
			outcomes = append(outcomes, outcome)
			continue
		}
		outcome.Value = raw
		if entry.convert != nil {
			raw = entry.convert(raw)
		}

		// Through the registry's own validator, which also canonicalises
		// - so an int arriving from TOML as int64 is stored in the shape
		// every reader expects.
		value, err := Validate(entry.key, raw)
		if err != nil {
			outcome.Skipped = "the value is not one this setting accepts: " + err.Error()
			outcomes = append(outcomes, outcome)
			continue
		}
		outcome.Value = value

		exists, err := s.settingRowExists(ctx, entry.key)
		if err != nil {
			return outcomes, err
		}
		if exists {
			outcome.Skipped = "the panel already has a value for it, which is left alone"
			outcomes = append(outcomes, outcome)
			continue
		}

		if err := s.writeMigrated(ctx, entry.key, value, path, outcome.From); err != nil {
			return outcomes, err
		}
		outcome.Written = true
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// settingRowExists reports whether the deployment-wide row is already
// there.
//
// Deployment-wide only: every key this command moves is ScopeGlobal, and
// a site-scoped row for one of them would be a different setting rather
// than a reason to skip.
func (s *Store) settingRowExists(ctx context.Context, key Key) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx,
		`SELECT 1 FROM panel_settings WHERE key = $1 AND site_id = ''`, string(key)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("panel: checking for an existing %s: %w", key, err)
	}
	return true, nil
}

// writeMigrated stores one value and files it.
//
// It goes around SetSetting deliberately, and the reason is worth
// stating: several of the keys that will move in A5.2 carry legal weight
// and are refused by SetSetting without a developer-password
// authorisation. That guard is about somebody changing a value from a
// browser. This is a person at a shell on the server copying a value
// that is *already in force* on that same server - asking them for the
// password from the file they are standing next to would be ceremony,
// and the audit entry records exactly what happened either way.
func (s *Store) writeMigrated(ctx context.Context, key Key, value any, file, from string) error {
	if err := s.setSetting(ctx, key, "", value, nil); err != nil {
		return err
	}
	entry := AuditEntry{
		ActorKind:  PrincipalDeveloper,
		ActorLabel: DeveloperLabel,
		Action:     ActionSettingMigrated,
		Target:     string(key),
		Detail: map[string]any{
			"value": value,
			"file":  file,
			"from":  from,
		},
	}
	if err := s.Record(ctx, entry); err != nil {
		// The row is written. Failing now would make the operator run
		// the command again, which would then report the key as already
		// present - a confusing answer to a problem that is not theirs.
		s.logAuditFailure("setting migration", err)
	}
	return nil
}

// lookupTOML walks a decoded file to a path.
func lookupTOML(file map[string]any, path []string) (any, bool) {
	var current any = file
	for _, step := range path {
		table, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = table[step]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func tomlPath(path []string) string {
	out := ""
	for i, step := range path {
		if i > 0 {
			out += "."
		}
		out += step
	}
	return out
}

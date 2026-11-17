// Package configreport is what a running service says about the
// configuration it loaded, for the setup wizard (PLAN §W4, items 5 and 6).
//
// # Why the services say it
//
// The wizard's "Yapılandırma dosyaları" step listed thirteen settings and
// said "panele bildirilmemiş" beside ten of them, on every deployment: the
// panel's role cannot read collector.toml or beacon.toml, deliberately -
// those files carry database passwords, and five roles exist so that no
// service holds another's credentials. Measured on the live rig before
// this package: 13 rows, 10 unreported, 3 secret, 0 known.
//
// The process that loaded a file is the one place its values are known,
// and it already writes a row the panel reads (internal/heartbeat). So it
// says them there - and what it says is what it is running on, not what
// the file says now: a file edited and never restarted into shows the old
// value, which is the value in force. The same argument the heartbeat's
// profile and ip_token_key_state columns make.
//
// # What may be said
//
// Only the keys in Entries, and of those only the ones not marked Secret.
// That is structural: Values resolves the listed non-secret keys and
// nothing else, so a field added to a config type is not reported until
// somebody lists it here and decides whether it is a secret - and
// TestASecretLookingKeyIsMarkedSecret refuses a listed key whose name
// says password, dsn, token, key or hash without the mark.
//
// The values are not visitor data and not credentials, but they are a
// deployment's plumbing: which address the collector forwards a
// customer's site to, which proxies the beacon trusts. On a deployment
// with two customers each one's collector says its own site and backend.
// So the panel reads them in one place, the setup wizard, which only
// somebody who speaks for the whole deployment opens; the health page and
// the diagnostic bundle - both open to a site's owner - do not read them
// (internal/invariants holds the readers to that).
package configreport

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Programs that report. The name is the program's, not its database
// role's: roles are configurable ([roles] in panel.toml) and the panel
// knows only two of them, so a row says which program wrote it.
const (
	Collector = "collector"
	Beacon    = "beacon"
	API       = "api"
	Panel     = "panel"
)

// Programs is every program that reports, in the order the wizard lists
// them.
var Programs = []string{Collector, Beacon, API, Panel}

// The two keys the beacon reports about its site list as it is now,
// rather than as the file had it. Not file keys: the list is a live
// setting (beacon.sites) that falls back to the file's sites, and which
// of the two is in force is exactly what the wizard did not know.
const (
	// KeyAcceptedSites is the sites the beacon accepts events for, after
	// the ones that cannot be a site's key were dropped, joined by ", ".
	KeyAcceptedSites = "accepted_sites"
	// KeySitesSource is where that list came from: SourcePanel or
	// SourceFile.
	KeySitesSource = "accepted_sites_source"
)

// The two values KeySitesSource takes.
const (
	SourcePanel = "panel"
	SourceFile  = "file"
)

// Entry is one key a program may report.
type Entry struct {
	Program string
	// Key is the TOML path in the program's config file, exactly as
	// written there - "network.listen_addr" - or one of the live keys
	// above.
	Key string
	// Secret marks a key whose value is never reported. It is listed so
	// the wizard can say it exists; Values never reads it.
	Secret bool
	// Live marks a key that is not in the file: the process sets it from
	// what it is doing. Values does not resolve it; the program adds it.
	Live bool
}

// Entries is every key that may be reported.
//
// Adding a key here is how a new config value becomes visible in the
// wizard; nothing is discovered, because a value somebody did not decide
// to show is a value nobody decided was safe to show.
var Entries = []Entry{
	{Program: Collector, Key: "site_id"},
	{Program: Collector, Key: "mode"},
	{Program: Collector, Key: "network.listen_addr"},
	{Program: Collector, Key: "network.backend_addr"},
	{Program: Collector, Key: "tls.cert_file"},
	{Program: Collector, Key: "storage.timescale_dsn", Secret: true},
	{Program: Collector, Key: "logging.dir"},

	{Program: Beacon, Key: "listen_addr"},
	{Program: Beacon, Key: "path_prefix"},
	{Program: Beacon, Key: "trusted_proxies"},
	{Program: Beacon, Key: "sites"},
	{Program: Beacon, Key: "timescale_dsn", Secret: true},
	{Program: Beacon, Key: "logging.dir"},
	{Program: Beacon, Key: KeyAcceptedSites, Live: true},
	{Program: Beacon, Key: KeySitesSource, Live: true},

	{Program: API, Key: "listen_addr"},
	{Program: API, Key: "timescale_dsn", Secret: true},
	{Program: API, Key: "logging.dir"},

	{Program: Panel, Key: "listen_addr"},
	{Program: Panel, Key: "trusted_proxies"},
	{Program: Panel, Key: "panel_dsn", Secret: true},
	{Program: Panel, Key: "developer_gate.password_hash", Secret: true},
	{Program: Panel, Key: "logging.dir"},
}

// For returns a program's entries, in Entries' order.
func For(program string) []Entry {
	var out []Entry
	for _, e := range Entries {
		if e.Program == program {
			out = append(out, e)
		}
	}
	return out
}

// Known reports whether program is one of Programs.
func Known(program string) bool {
	for _, p := range Programs {
		if p == program {
			return true
		}
	}
	return false
}

// MaxValueBytes bounds one reported value. A site list can be long and
// the row is rewritten every minute; the wizard shows a value in a table
// cell, so a value cut with "…" loses nothing anybody reads there.
const MaxValueBytes = 2048

// Values resolves program's non-secret file keys against cfg, the
// configuration the process loaded - a struct or a pointer to one, read
// by its toml tags.
//
// A key the struct does not have is left out rather than reported empty:
// "not reported" and "empty in the file" are different facts, and only
// the second is about the deployment. TestEveryEntryResolves holds every
// listed key to the program's real config type, so leaving out happens
// only to a key somebody listed wrongly.
func Values(program string, cfg any) map[string]string {
	out := map[string]string{}
	root := reflect.ValueOf(cfg)
	for _, e := range For(program) {
		if e.Secret || e.Live {
			continue
		}
		v, ok := resolve(root, e.Key)
		if !ok {
			continue
		}
		out[e.Key] = Bound(format(v))
	}
	return out
}

// Bound cuts a value to MaxValueBytes on a rune boundary.
func Bound(s string) string {
	if len(s) <= MaxValueBytes {
		return s
	}
	cut := MaxValueBytes
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// resolve walks a dotted TOML path through a struct by its fields' toml
// tags.
func resolve(v reflect.Value, path string) (reflect.Value, bool) {
	for _, part := range strings.Split(path, ".") {
		for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		field, ok := FieldByTag(v.Type(), part)
		if !ok {
			return reflect.Value{}, false
		}
		v = v.FieldByIndex(field.Index)
	}
	return v, true
}

// FieldByTag finds the field of a struct type whose toml tag names key.
func FieldByTag(t reflect.Type, key string) (reflect.StructField, bool) {
	if t.Kind() != reflect.Struct {
		return reflect.StructField{}, false
	}
	for i := 0; i < t.NumField(); i++ {
		if strings.Split(t.Field(i).Tag.Get("toml"), ",")[0] == key {
			return t.Field(i), true
		}
	}
	return reflect.StructField{}, false
}

// format writes a value the way the wizard shows it: a list joined by
// ", ", a nil pointer as nothing.
func format(v reflect.Value) string {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Slice, reflect.Array:
		parts := make([]string, 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			parts = append(parts, format(v.Index(i)))
		}
		return strings.Join(parts, ", ")
	case reflect.Map:
		keys := make([]string, 0, v.Len())
		for _, k := range v.MapKeys() {
			keys = append(keys, fmt.Sprint(k.Interface())+"="+format(v.MapIndex(k)))
		}
		sort.Strings(keys)
		return strings.Join(keys, ", ")
	}
	return fmt.Sprint(v.Interface())
}

// Report is what one process says: which program it is and its values.
// It is what the heartbeat's config column holds, as JSON.
type Report struct {
	Program string            `json:"program"`
	Values  map[string]string `json:"values"`
}

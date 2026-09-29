package logging

// Taking a log line off the machine.
//
// The panel's diagnostic file (catalogue #37) carries the recent WARN and
// ERROR lines to whoever supports the deployment. A log line is written
// for the operator of this machine, and some of what it holds is about
// people who never agreed to leave it: a trust decision records the
// visitor's network address (KeyPeer) and what the client claimed
// (KeyClaim). The owner's decision (2026-09-28, option c): the message
// and the listed attributes go, addresses go masked the way the product
// stores them (/24, /64), and the client's claim never goes.
//
// This file is the list. A key nobody has classified is withheld - the
// safe direction - and internal/invariants/logkeys_test.go holds the
// list against every key the tree writes, both ways, so a new key is a
// question somebody has to answer rather than something that silently
// goes missing, and a key no code writes any more does not linger here.

// ExportRule is what happens to one attribute of a log line that leaves
// the machine.
type ExportRule int

const (
	// ExportUnknown is an unclassified key. Withheld: an attribute nobody
	// has looked at does not leave the machine.
	ExportUnknown ExportRule = iota
	// ExportKept goes, with every address and email address in it masked.
	ExportKept
	// ExportAddress is a field whose value is a network address. It goes
	// masked, or not at all when its value is not recognisable as an
	// address: a field meant for one that holds something else is not a
	// field anybody has reasoned about.
	ExportAddress
	// ExportNever does not go.
	ExportNever
)

// exportRules classifies every key the tree logs under.
var exportRules = map[string]ExportRule{
	// What the client asserted: a forwarded-for header is a visitor's
	// address in the client's own words, and a claim is precisely the
	// thing this project never takes on trust.
	KeyClaim: ExportNever,

	// Network addresses. peer is the immediate client of a trust
	// decision; addr is a listener's address in the startup lines and,
	// in the proxy's one warning, the remote address it could not parse.
	KeyPeer: ExportAddress,
	"addr":  ExportAddress,

	// Everything else the tree writes: errors, counts, identifiers of
	// requests, backups and settings, versions, sizes, timings, and the
	// server's own reasons. Addresses and email addresses inside them are
	// masked as text, because an error can quote what it was given.
	KeyVerdict: ExportKept, KeyReason: ExportKept, KeySource: ExportKept,
	CategoryKey: ExportKept,
	"accepted":  ExportKept, "action": ExportKept, "actions": ExportKept,
	"actor_kind": ExportKept, "after_days": ExportKept, "asn": ExportKept,
	"asns": ExportKept, "attempted_rows": ExportKept, "available": ExportKept,
	"backend": ExportKept, "backup": ExportKept, "base": ExportKept,
	"base_url": ExportKept, "breakdown": ExportKept, "buckets": ExportKept,
	"build": ExportKept, "by": ExportKept, "bytes": ExportKept, "card": ExportKept,
	"ceiling_from": ExportKept, "ceiling_mb": ExportKept, "chosen": ExportKept,
	"chunks": ExportKept, "column": ExportKept, "compressed": ExportKept,
	"configured": ExportKept, "consecutive_failures": ExportKept,
	"count": ExportKept, "countries": ExportKept, "country": ExportKept,
	"cutoff": ExportKept, "database": ExportKept, "days": ExportKept,
	"deadline": ExportKept, "dev_access": ExportKept, "diagnosis": ExportKept,
	"dir": ExportKept, "dropped": ExportKept, "dropped_by_server": ExportKept,
	"dropped_total": ExportKept, "elapsed_ms": ExportKept, "err": ExportKept,
	"error": ExportKept, "estimate": ExportKept, "fallbacks": ExportKept,
	"family": ExportKept, "fetched_at": ExportKept, "file": ExportKept,
	"files": ExportKept, "fingerprints": ExportKept, "from": ExportKept,
	"from_days": ExportKept, "gomaxprocs": ExportKept, "gomemlimit": ExportKept,
	"gomemlimit_from": ExportKept, "hint": ExportKept, "how": ExportKept,
	"id": ExportKept, "in_force": ExportKept, "installed": ExportKept,
	"interval": ExportKept, "ipv4_err": ExportKept, "ipv4_ranges": ExportKept,
	"ipv6_err": ExportKept, "ipv6_ranges": ExportKept, "keep_days": ExportKept,
	"key": ExportKept, "keys": ExportKept, "label": ExportKept,
	"language": ExportKept, "latest": ExportKept, "level": ExportKept,
	"login_attempts": ExportKept, "logs": ExportKept, "max_concurrent": ExportKept,
	"max_per_second": ExportKept, "memory_limit": ExportKept,
	"memory_limit_from": ExportKept, "method": ExportKept, "missing": ExportKept,
	"mode": ExportKept, "ms": ExportKept, "needs_mb": ExportKept,
	"networks": ExportKept, "newly_compressed": ExportKept, "numcpu": ExportKept,
	"operation_id": ExportKept, "operations": ExportKept, "page": ExportKept,
	"panic": ExportKept, "path": ExportKept, "path_prefix": ExportKept,
	"policy_from": ExportKept, "policy_to": ExportKept,
	"pool_max_conns_default": ExportKept, "problems": ExportKept,
	"profile": ExportKept, "rate_store_bounded": ExportKept, "reached": ExportKept,
	"rejected": ExportKept, "removed": ExportKept, "request": ExportKept,
	"requests": ExportKept, "rolled_back": ExportKept, "rows": ExportKept,
	"sealed": ExportKept, "service": ExportKept, "sets": ExportKept,
	"since_last_report": ExportKept, "site": ExportKept, "sites": ExportKept,
	"span": ExportKept, "stack": ExportKept, "stage": ExportKept,
	"stale_for": ExportKept, "state": ExportKept, "status": ExportKept,
	"step": ExportKept, "suggestion": ExportKept, "table": ExportKept,
	"tables": ExportKept, "tables_bytes": ExportKept, "throttle_queue": ExportKept,
	"through": ExportKept, "to": ExportKept, "to_days": ExportKept,
	"took": ExportKept, "until": ExportKept, "used": ExportKept, "using": ExportKept,
	"value": ExportKept, "version": ExportKept, "was": ExportKept,
	"was_asns": ExportKept, "was_countries": ExportKept, "what": ExportKept,
	"where": ExportKept, "why": ExportKept, "work": ExportKept,
	"written": ExportKept, "zone": ExportKept,
}

// ExportRuleFor is how an attribute under this key leaves the machine.
func ExportRuleFor(key string) ExportRule { return exportRules[key] }

// ExportClassified lists every classified key, for the structural rule
// that holds this list against the tree.
func ExportClassified() []string {
	keys := make([]string, 0, len(exportRules))
	for k := range exportRules {
		keys = append(keys, k)
	}
	return keys
}

# Deferred design notes

Not a changelog and not user-facing documentation (see README.md for
that) - this is where design decisions that were deliberately deferred
get written down in enough detail that picking them back up later doesn't
require re-deriving the reasoning from scratch.

## Richer country/ASN rule engine (deferred from Aşama 3)

Aşama 3 (see README's "Optional: IP → country / ASN lookup") ships a
**denylist only**: `asn_lookup.blocked_countries` / `asn_lookup.blocked_asns`,
flat lists, a match is an unconditional reject regardless of
`limits.overload_policy`. That was a deliberate scope decision, not an
oversight - the alternative (a per-rule policy engine) is real extra
complexity that wasn't worth building until there's an actual need for
it. This note is what that richer version would look like, so building
it later doesn't start from zero.

### What "richer" means

Per-rule *policy*, not just a binary block: e.g. "always fail_open for
ASN 15169 (Googlebot) no matter the collector's load", "throttle
everything from country X to N req/s", alongside the plain block case
the denylist already covers. Reusing `limiter.Policy`'s existing
vocabulary (`fail_open` / `fail_closed` / `throttle`) as each rule's
action, rather than inventing new terminology, seems like the obvious
choice - but see the open questions below before assuming that's the
whole story.

Rough config shape (TOML array-of-tables, matching how this project
already prefers flat/simple config over a bespoke DSL):

```toml
[[asn_lookup.rules]]
match  = { country = "CN" }
action = "fail_closed"

[[asn_lookup.rules]]
match  = { asn = 15169 }
action = "fail_open"
```

### Open questions a future implementation needs to actually resolve (not pre-decided here)

1. **Rule ordering / precedence.** If a request's country matches one
   rule and its ASN matches a different rule with a different action,
   which wins? First match in list order? Most specific (asn) beats
   least specific (country)? The denylist doesn't have this problem -
   a match is a match, order is irrelevant when every match means the
   same thing (reject).

2. **Does a `fail_open`/"always allow" rule bypass just the geo-check, or
   the global `limiter.Limiter` too?** These are very different in
   consequence. Exempting a rule match from the *geo-block* only is
   safe and roughly what "always allow this ASN" sounds like it should
   mean. Exempting it from the collector's own concurrency/rate
   protection as well is a much bigger behavioral change - it would mean
   a spoofed or compromised address inside an "always allow" ASN could
   be used to bypass DDoS protection on the collector itself. Whichever
   way this goes, it needs to be a deliberate, documented choice, not an
   accident of implementation order.

3. **Does per-rule `throttle` need its own bounded queue/state per rule
   (effectively its own `limiter.Limiter` instance per configured rule),
   and if so, does that revive the "unbounded key space" concern that's
   the whole reason the global limiter stays non-keyed by IP?** Probably
   not a real problem in practice - unlike per-IP state (attacker-
   controlled cardinality), the key space here is the admin's own rule
   list, bounded by how many rules they configure - but it's worth
   confirming explicitly rather than assuming, and it does mean a
   real per-rule concurrency counter and its own tests (mirroring
   internal/loadtest's real-concurrency style, per rule), not just a set
   lookup like the denylist needs.

4. **Where should this live?** The denylist-only version deliberately
   stays a small, stateless, sibling check next to `Limiter.Admit` rather
   than being folded into `Limiter` itself (see the Aşama 3 commit) -
   it's just a set-membership test, no shared state with the global
   limiter's counters. A stateful per-rule engine (point 3 above) is a
   different shape of thing - a set of independent little limiters keyed
   by rule, not a single check - and might genuinely deserve its own
   package (something like `internal/geopolicy`) rather than living
   inside `internal/limiter`. Worth deciding deliberately rather than
   growing `internal/limiter` into two unrelated responsibilities by
   accretion.

### Non-goals even for the richer version

Nothing here implies IP-level (as opposed to country/ASN-level) rules -
that's a different, much larger key space with the same cardinality
concerns the global limiter's own doc comment already explains for why
it isn't keyed by IP. Country/ASN stay the only supported match
dimensions; if IP-level blocking is ever wanted, it's a genuinely
different feature (an IP blocklist/allowlist), not an extension of this
one.

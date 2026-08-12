# Deferred design notes

Not a changelog and not user-facing documentation (see README.md for
that) - this is where design decisions that were deliberately deferred
get written down in enough detail that picking them back up later doesn't
require re-deriving the reasoning from scratch.

## Where the project stands

Built and verified against real infrastructure, in order:

1. **Collector** - passthrough (TCP/TLS, never decrypts) and full
   (TLS-terminating) modes, JA4 fingerprinting, per-IP sliding-window
   rate tracking, 0-100 bot score, batch flush to TimescaleDB.
2. **Self-protection** - `[limits]` with fail_open / fail_closed /
   throttle policies, proven under real concurrent load.
3. **IP intelligence** - country + ASN resolved from sapics/ip-location-db
   (`user-country`, `origin-asn`), four in-memory range tables, ~135 MB
   for the full real dataset, ~103 ns cache-hit lookups.
4. **Acting on it** - country/ASN denylist (unconditional reject,
   independent of overload policy) and an ASN scoring signal
   (`known_bot_asns`, gated by `apply_to_scoring`).
5. **Read API** - `cmd/analytics-api`, a separate read-only binary with
   SHA-256-hashed bearer tokens and per-token site grants; 11 endpoints
   (overview, summary, timeseries, top-ips, per-IP detail, countries,
   asns, ja4, score-distribution, snapshots, sites).

`site_id` is required in the collector config and stamped on every row,
so one database can serve several sites.

### Deployment topology (decided, not incidental)

The collector is a reverse proxy, so it **must** run in each site's
traffic path - that isn't a choice. Storage and the API could have been
centralised but deliberately weren't:

- Each site's collector writes to a TimescaleDB **local to that VDS**.
- The management panel **pulls** over the read-only HTTP API with a token.
- No customer VDS ever needs to expose a database, and no central
  database needs credentials distributed to customer machines.

This was chosen over a central database because sites are mixed (some on
the operator's servers, some on customers' own), because exposing a
read-only HTTPS API is a much smaller attack surface than exposing
Postgres, and because it leaves central aggregation available later
without touching the collector: the panel can mirror what it pulls into
its own store, which becomes the "central analytics" by pull rather than
push.

A consequence worth remembering: with a central database, N collectors
would all `TRUNCATE`+`COPY` the shared `ip_country_ranges` /
`ip_asn_ranges` tables on their own refresh schedules and stomp each
other. The local-storage topology avoids that entirely; any future move
to central storage has to solve it (one designated refresher, or
`local_csv_path` everywhere).

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

## Page/path, referrer, user agent and session analytics (never collected)

The single biggest gap between what this project produces and what a
customer expects from the word "analytics". Written up in full because
it's a product decision, not a small missing endpoint, and because the
obvious first instinct - "just add a `/pages` endpoint" - doesn't work.

### What's missing, and why, per mode

- **Passthrough mode: cryptographically impossible.** The proxy never
  decrypts TLS. The URL, headers, referrer and user agent are all inside
  the encrypted stream. No amount of work on the storage or API layer
  changes that; the bytes aren't readable by design, which is also the
  mode's main selling point ("point it at your site, it never sees your
  users' data").
- **Full mode: available but deliberately not recorded.** Here TLS *is*
  terminated, so `recordingHandler` genuinely holds `r.URL.Path`,
  `r.Header`, everything. It passes only `(ip, ja4, time)` to
  `RateStore.RecordRequest`. So this half is a scope decision that could
  be revisited, not a physical limit.

Session/pageview/bounce-rate concepts don't exist in either mode: there
is no cookie, no client-side identifier, and no notion of a visit - only
IP-level activity.

### Why "just record the path" is harder than it looks

`ratestore` keeps O(1) state per IP on purpose. Keying by (IP, path)
multiplies that by an **attacker-controlled** number of distinct paths -
`/x1`, `/x2`, ... is a trivial way to blow up memory. This is precisely
the unbounded-key-space concern `internal/limiter`'s own doc comment
already cites for why it isn't keyed by IP.

A real implementation therefore needs one of:

- **Bounded top-K per site** (count-min sketch / heavy hitters), giving
  "your 50 busiest paths" rather than exact per-path totals.
- **A separate append-only table** written per request rather than per
  flush - accurate, but that's a request log, a fundamentally different
  storage shape and cost profile from the snapshot table this project is
  built around.

Either way it only ever works in full mode, so the panel would show path
data for some customers and not others depending on their deployment -
which needs a deliberate UX answer, not silence.

### The alternative worth considering first: a JS beacon

The conventional approach (Umami, Plausible, Matomo, GA) is a script tag
on the customer's site posting pageview, referrer, screen size and a
session identifier. That yields exactly the missing fields, works
identically in both modes because it doesn't depend on the proxy at all,
and is a well-understood pattern.

The important part is that the two sources are **complementary rather
than redundant**:

- A JS beacon only fires for clients that execute JavaScript, so it is
  nearly blind to bots - which is why conventional analytics
  systematically under-reports automated traffic.
- This collector sees every connection including the ones that never run
  a line of JS, and fingerprints them.

Together they answer both "what did real humans do on which pages" and
"what actually hit the server, human or not" - and the second is the
thing conventional tools can't give. That framing is worth keeping in
mind before treating the beacon as merely catching up to competitors.

### Caveat that already applies today: unique IP is not unique visitor

Worth stating wherever "unique_ips" is presented to a customer as
"visitors":

- **CGNAT undercounts.** Mobile carriers (including the major Turkish
  ones) put many subscribers behind one address, so a busy mobile
  audience collapses into far fewer IPs than people.
- **Dynamic addressing overcounts.** One person across several days
  appears as several IPs.

Neither is fixable at the IP layer - it's inherent to identifying people
by address. A JS beacon with a session identifier is what resolves it,
which is another reason the two approaches pair well.

## Exact cumulative request counts (deferred from the read API)

The read API reports `peak_window_requests` - the busiest single sliding
window - and deliberately reports no cumulative "total requests over this
range". That's a data-model limit, not an oversight, and it's worth
writing down because "how many requests did my site get this week" is an
obvious thing a customer panel will want.

**Why it can't be computed from what's stored.** `traffic_snapshots` is a
periodic *sample* of the collector's in-memory sliding window, not a
request log. Consecutive samples of the same IP overlap (a 60s window
sampled every 10s by default), so `sum(curr_window_count)` overcounts by
roughly window/interval.

**What was tried and rejected.** An earlier version of the API
reconstructed a total by integrating the sampled rate over time: sum the
per-flush total rate, multiply by the flush interval inferred from the
gaps between flush timestamps. The arithmetic was correct and a synthetic
test with evenly spaced, steady traffic passed exactly. It was still
wrong in practice, and a real end-to-end run caught it: 38 requests
actually sent, 3 reported. Two compounding reasons:

1. `request_rate` averages over the *window* (60s), but the integral
   multiplies by the *flush interval* (2-10s). A burst shorter than the
   window is therefore scaled down by roughly that ratio.
2. The collector only writes rows for IPs seen since the previous flush,
   so flush events are genuinely irregular under bursty traffic - the
   "evenly spaced samples" premise the integral rests on simply doesn't
   hold.

The lesson worth keeping: the synthetic test validated the *arithmetic*
but not the *premise*, and only running the real thing exposed it.

**What an exact total would need.** A monotonic per-IP request counter
that `ratestore` doesn't currently keep (its two-counter sliding window
deliberately discards history to stay O(1) per IP). Adding one means a
new counter in `ratestore.WindowStats`, a new column, and deciding what
happens when an IP's entry is evicted after `cache.ttl_seconds` - the
counter would need flushing before eviction or the tail would be lost.
That's a real design change across three packages, not a query tweak,
which is why it isn't bolted onto the API layer.

## Tunable per-ASN scoring weight (deferred from Aşama 4)

Aşama 4 (see README's "Optional: IP → country / ASN lookup") ships a
**flat bonus**: `asn_lookup.known_bot_asns`, any match adds the same
`scoring.maxASNScore` (20 points) regardless of which ASN matched -
mirroring `KnownBotJA4`'s own flat-bonus shape (`maxJA4Score`, every
known-bad JA4 worth the same 30 points) rather than inventing a new
weighted shape just for ASN. That's the same "don't build the richer
thing until it's needed" reasoning as the denylist above, not an
oversight.

A richer version would let the *weight* vary per ASN - e.g. a
data-center/hosting ASN scored higher than a residential-ISP-adjacent
one that merely happens to host some bots - via a config shape like:

```toml
[[asn_lookup.known_bot_asns]]
asn    = 16509   # AWS
weight = 25

[[asn_lookup.known_bot_asns]]
asn    = 8075    # Microsoft/Azure
weight = 15
```

replacing the current flat `known_bot_asns = [16509, 8075]` list. Same
open question as the blocking engine's rule ordering (point 1 above)
would resurface in a smaller form here too: multiple weighted matches
still just sum like today's flat bonus does, so there isn't real
ordering ambiguity for scoring the way there is for block-vs-allow
policy - the harder part would be sourcing defensible per-ASN weights at
all (see the "known hosting/datacenter ASN" data-source question raised
and deliberately not pursued during Aşama 4's design discussion - a
curated classification dataset is a real research question, not
something to assume exists, the same "verify before building on it"
discipline this whole project has followed for its other external data
sources). `internal/scoring`'s `Score` signature would need `asn int,
knownBotASNs map[int]struct{}` to become something like `knownBotASNWeights
map[int]int` - a small, mechanical change, not an architectural one.

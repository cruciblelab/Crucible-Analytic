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
6. **Client-side beacon** - `cmd/beacon`, a third binary serving a 2.1 KB
   (gzipped) JS snippet and ingesting the events it sends into
   `beacon_events`: pages, referrers, campaigns, browsers, devices,
   custom events, cookieless visitor IDs. Verified end to end by driving
   the real snippet in a real headless Chromium against the real binary.
7. **Read API for both sources** - 17 more endpoints on the same
   read-only binary: `/beacon/` for people and pageviews, `/crossover/`
   for the questions that need both tables at once. 28 routes total.
8. **Panel data and auth layer** - `internal/panel`: accounts with
   argon2id, per-site roles resolved through one choke point,
   Postgres-backed sessions, replay-resistant TOTP, CSRF, an
   append-only audit log, and consent-gated developer access. The HTTP
   surface (wizards, dashboard views) is designed but not yet built -
   see the two planning sections below.

`site_id` is required in the collector config and stamped on every row,
so one database can serve several sites. The beacon takes an allowlist of
the same identifiers, so both sources land under one key.

### Three binaries, deliberately

`cmd/collector` writes and sits in the traffic path; `cmd/analytics-api`
only reads and is token-gated; `cmd/beacon` writes and is the only thing
the whole internet may POST to. Each split buys a specific guarantee:
the API's read-only database role is only meaningful while no writer
shares its process, and putting attacker-supplied JSON parsing in the
collector would hand the one component that must never go down a threat
surface it currently doesn't have.

### What is left, in order

Written down because "is that all of it?" deserves an answer that does
not change each time it is asked. Everything below is planned in detail
in the sections further down; this is the sequence.

**A. Operational settings and retention** (nothing above works without
this)
1. `panel_settings` table, per-site and global, typed values.
2. The three profiles as named sets of defaults that write individual
   settings.
3. `asn_lookup` gains a country-only mode - the memory difference
   between Dengeli and Tam Crucible.
4. Retention policies on both analytics tables via TimescaleDB's own
   `add_retention_policy`, plus the panel setting that drives them.
5. **Move the operational keys out of the TOML files into
   `panel_settings`**, leaving only the eight bootstrap keys behind. The
   full division is written out under "For any of this to work"; without
   this step the repair catalogue has nothing it can actually change.
6. Collector and beacon re-read those settings periodically and apply
   what can honestly be applied while running - keeping the last known
   values when the read fails, never falling back to defaults.

**B. Observability** (what makes SSH avoidable)
7. `panel_logs` with its own short retention, a self-expiring verbose
   toggle, and the same NUL/UTF-8 sanitising the beacon needs.
8. `panel_operations` - the operation journal, with correlation IDs,
   before/after values and rollback state.
9. The repair operations themselves: thirty-nine named, typed, bounded
   functions (the catalogue below), each journaled.
10. The health page, surfacing counters the services already keep.
11. The support token and the authenticated health route on the read
    API.

**C. The panel's HTTP surface**
12. Turkish message catalogue, templates, embedded HTMX, CSS.
13. First-run detection and the developer wizard.
14. The owner wizard, with the confirmation-gated door to the technical
    one.
15. Login, two-factor, account settings, member management.
16. The developer-access approval screen (the request banner, approve
    and deny).

**D. The dashboard itself**
17. Site picker, then the six-card default view per site.
18. Drill-downs: pages, sources, campaigns, devices, countries, events.
19. Developer-mode layers on the same pages: fingerprints, ASNs,
    scores, the crossover views, raw export.
20. Settings pages, each change opening the streaming operation modal.

**E. Consolidation and hardening**
21. `cmd/crucible` with subcommands and one config file.
22. The `Sprintf`-near-SQL test.
23. README rewrite for the whole product rather than the collector.

Two things deliberately **not** on this list, so they are not assumed:
a mobile app, and any form of alerting to end customers (email,
webhooks). Both are reasonable later; neither is in this arc.

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

## Analytics depth: how much of this to actually run (planned, not built)

This system collects considerably more than a conventional analytics
tool, and that is a cost as well as a feature. A shop owner with a
thousand visitors a month does not need JA4 fingerprinting, and running
it for them means memory, disk and CPU spent on a question they never
ask. The answer is not to make it lighter - the depth is the product -
but to make the depth **chosen**.

### What actually costs something

Measured or structural, not guessed:

| Knob | Cost when on | Cost when off |
| --- | --- | --- |
| IP intelligence, full (`asn_lookup`) | ~135 MB resident, four range tables, periodic CSV download | 0; country and ASN columns stay empty |
| IP intelligence, country only | ~65-70 MB (half the tables) | — |
| Collector fingerprinting | CPU per connection, per-IP sliding-window state | 0; no bot dimension at all |
| Flush interval (10s default) | one row per active IP per flush | 60s is six times fewer rows |
| Beacon | one row per pageview/event; negligible CPU | 0; no page-level data |
| **Retention** | **unbounded - see below** | — |

The largest of these is the one currently missing entirely.

### The retention gap (a real defect, not a preference)

There is no retention policy anywhere in this project. Neither
`traffic_snapshots` nor `beacon_events` is ever trimmed, and neither
schema declares a TimescaleDB retention policy. On a customer VDS that
means the disk fills, slowly and silently, and the first symptom is the
collector failing to write - i.e. the traffic path degrading because of
an analytics table.

Retention has to become a first-class setting with a conservative
default (90 days is a reasonable starting point and matches the read
API's `maxRange`), implemented with TimescaleDB's own
`add_retention_policy` rather than a cron job doing DELETEs, because
dropping whole chunks is nearly free and deleting rows is not.

### Profiles rather than knobs

Nobody buying this can reason about flush intervals. The panel offers
three presets, and developer mode exposes every underlying setting for
whoever wants them:

- **Hafif** - beacon only, no fingerprinting, no IP intelligence.
  Roughly what Umami costs and roughly what Umami tells you: visitors,
  pages, sources, devices, campaigns. The honest positioning is "this
  mode is not why you bought this".
- **Dengeli** (default) - beacon plus collector with JA4 and rate
  scoring, IP intelligence in country-only mode. The bot dimension is
  present; the ASN half of the range tables is not loaded.
- **Tam Crucible** - everything, including ASN, the crossover views and
  raw export.
- **Özel** - each setting individually, in developer mode.

A profile is a named set of defaults, not a mode the code branches on.
Switching to "Hafif" writes the individual settings; the panel then
shows "Hafif (değiştirilmiş)" if any of them is later touched. Anything
else would mean two sources of truth for the same behavior.

### The dashboard is written to Tam Crucible; profiles subtract data

The panel is designed for the full product, and lighter profiles remove
*data*, never pages. Every view exists in every profile. When the
current profile does not collect what a view needs, the view says so
plainly and links to the setting that would change it:

> Bu veri şu anki modda toplanmıyor. → Dengeli moda geç

Three things follow, and all three are easy to get wrong:

- **A view is never hidden.** Hiding it means a customer who upgrades
  their profile never discovers what they bought, and a customer
  comparing us to a competitor sees a shorter feature list than we have.
- **"Not collected" is never rendered as zero.** Showing "0 bot" when
  bot detection is switched off is a lie in exactly the way this project
  refuses elsewhere - the same distinction `beacon_events`' empty-group
  flag and `asnlookup`'s `''`-means-unresolved convention already make.
  Zero means we looked and found none. Absent means we did not look.
- **The profile is stated on any view it affects**, so a screenshot of a
  number always carries the context needed to read it.

Note the separation this creates, which is worth keeping straight:
**the profile decides what is collected, developer mode decides what is
shown.** A Tam Crucible deployment with developer mode off still gathers
everything - the owner simply is not looking at fingerprints today.
Turning the toggle on reveals data that already exists rather than
starting collection.

### Where settings live, and who sees them

Two kinds, split by who is responsible for them:

- **Deployment settings** - database DSNs, listen addresses, TLS
  certificate paths, trusted proxies. Config file only, set by us
  through developer access. These never appear in the customer's panel:
  they are not decisions a shop owner should be asked to make, and a
  form that can change a DSN is a form that can point the panel at
  somebody else's database.
- **Operational settings** - analytics profile, retention, flush
  interval, IP-intelligence mode, geo blocking, scoring thresholds.
  Stored in the database, editable in developer mode.

The second kind is new: everything today is config-file only. The
collector and beacon will need to re-read operational settings
periodically (a minute is fine) and apply what can be applied live.
Some settings cannot be - a listen address, a TLS certificate - and
those stay in the config file rather than pretending to be editable.
The split is by *whether it can honestly take effect*, not by
convenience.

## Setup: two wizards, one handover (planned, not built)

Installation is done by us; ownership is not. The two are separate
flows, and the handover between them is the moment the developer's
standing access ends (see the developer-access section above).

### The developer wizard

Reached through developer access while no account exists. Covers the
technical ground: database connectivity and schema application, which
sites exist, collector mode and backend, TLS, trusted proxies, the
analytics profile, retention. It ends by confirming the deployment is
ready to hand over.

### The owner wizard

The first thing the customer sees. Creates their account, names the
site in language they use, sets the timezone, shows the snippet to
embed, offers to invite colleagues. It must never *require* a technical
step, because those are already done - where it needs a technical
value, it shows what the developer configured rather than an empty
field.

### The "geliştirici sihirbazı" door

The owner wizard carries an unobtrusive link to the technical wizard,
for owners who are technical themselves. The first click does not open
it. It warns:

> Bu bölüm geliştiriciniz tarafından tamamlandı. Yine de baştan yapmak
> isterseniz onaylayın.

Confirming opens the full technical wizard. The warning exists because
the common case is somebody exploring, and reconfiguring a working
deployment by accident is a support call at best. Making it a
confirmation rather than a hidden page is deliberate: it is their
server, and a technical owner should not have to ask us for access to
their own settings.

## Operating a customer's deployment from the panel instead of SSH

The goal: when a customer breaks something and calls, we ask for
developer access, sign in to **their panel in a browser**, diagnose and
fix it there. SSH into the machine is the last resort rather than the
first move.

To be unambiguous, because the wording "remote" invited exactly the
wrong reading: **there is one way in and it is the one already built.**
`crucible dev-access request` on the server, the owner approves it in
their panel, a single-use link. No second mechanism, no login from our
infrastructure into theirs, nothing that bypasses the owner's consent.
The whole difference being discussed here is `ssh root@vds` +
`journalctl` + `psql` versus the same person looking at the same
deployment through its own panel.

The constraint stated alongside it: no injection, no open door, no back
door in the database.

**These two pull against each other, and pretending otherwise is how
back doors get built.** A panel that can repair a deployment is by
construction a remote control surface for it, and every capability
added to help us is a capability available to anyone who compromises
the panel. The design below is an attempt to get most of the diagnostic
value while giving up almost none of the safety, by separating two
things that are usually conflated.

### Diagnosis and repair are different powers

- **Diagnosis** - reading logs, seeing system state, running read-only
  health checks. Low risk: it reveals operational data to someone who
  already has developer access. Should be generous.
- **Repair** - changing what the system does. High risk, and therefore
  a *finite, typed* set of named operations rather than a general
  ability to change things.

Making the second one finite is the whole design.

### No general-purpose escape hatch. Ever.

Specifically ruled out, permanently:

- a "run SQL" box
- a config-file textarea
- shell command execution
- a "restart" that takes a command string
- any operation whose parameter is code, a query, a path, or a hostname
  the panel then connects to

Each repair is a named Go function with typed, validated parameters -
`SetRetentionDays(int)`, `SetProfile(ProfileName)`, `ReloadIPData()`,
`RotateAPIToken(id)`. Adding one is a code change that goes through
review; it is not something a running system can be talked into.

This sounds *less* powerful than a SQL box and is in practice more
useful. In a real incident nobody wants to compose SQL against a schema
they half-remember at speed; they want "retention is wrong, set it to
90". A well-chosen catalogue covers almost every real incident, and
none of its entries can be turned against the customer whose data this
is.

The size of that catalogue is the whole question, and an earlier draft
of this note answered it with four examples, which was far too thin to
support the claim being made. If SSH is genuinely to be almost never
needed, the catalogue has to be written out properly - so it is, below.

### The operation catalogue

Grouped by what actually breaks. Every entry is a named Go function
with typed parameters, validated against explicit bounds before it
reaches the database, journaled with a correlation ID, and reversible
wherever reversing it is meaningful.

**A. Collection has stopped, or the numbers are wrong**

1. `FlushNow()` - push the pending buffer to Postgres. Parameterless.
   The answer to "kayıtlar on dakikadır gelmiyor" when the flush timer
   has wedged, and the first thing to try because it is free.
2. `SetFlushInterval(seconds)` - 1..300. Slow disk wants a longer one;
   diagnosing wants a shorter one.
3. `PauseCollection(site)` / `ResumeCollection(site)` - stop *recording*
   without stopping the proxy. The safe move mid-incident: traffic keeps
   reaching the customer's site, the disk stops filling, nobody loses a
   sale while we work.
4. `ReloadIPData()` - re-read the ASN/country tables. Parameterless. The
   answer to "ülkeler yanlış" or "ülke sütunu tamamen boş".
5. `SetASNLookupMode(mode)` - closed enum, `off` | `country_only` |
   `full`. Both a privacy setting and the fix for lookup eating the CPU.
6. `SetASNSource(source)` - closed enum over sources compiled into the
   binary. `local_csv` selects a path from a fixed allowlist of
   directories; it is not a free-text path, because a free-text path is
   a file-read primitive.
7. `SetTrustedProxies(prefixes)` - parsed to `netip.Prefix` values,
   never stored or compared as text. This one earns its place at the
   top: sitting behind Cloudflare with an empty trusted-proxy list makes
   every visitor look like the same IP, which makes *every other number
   in the system* wrong at once. It is the most common real
   misconfiguration there is and today it costs an SSH session.
8. `SetLimits(maxConcurrent, maxRPS)` - 1..100000 each. The fix when the
   collector itself has become the bottleneck.
9. `SetOverloadPolicy(policy)` - `fail_open` | `fail_closed` |
   `throttle`.
10. `SetThrottleQueueSize(n)` - 0..10000.
11. `SetBotScoreThreshold(site, score)` - 0..100. The answer to "gerçek
    müşterilerim bot sayılıyor", which is a tuning problem and should
    never have needed a shell.
12. `SetBlockedCountries(codes)` / `SetBlockedASNs(asns)` - country codes
    validated against a compiled-in ISO 3166-1 list, ASNs bounded to the
    assigned range. Wrong entries here silently discard real traffic, so
    the operation shows the last 24 hours' hit count per rule before
    accepting the change.
13. `SetKnownBotASNs(asns)` - the scoring signal, same validation.

**B. The disk and the database**

14. `ShowTableSizes()` - read-only, and first in this group because it is
    the first question every time.
15. `SetRetention(table, days)` - closed table enum, 1..3650. Refuses to
    shorten retention without a second confirmation naming how many rows
    that would delete, because "set it to 30" against two years of data
    is destructive and should feel like it.
16. `RunRetentionNow()` - don't wait for the scheduled job.
17. `SetCompressionPolicy(table, afterDays)` - 1..3650. Usually the
    better answer than deleting: the customer keeps their history and
    gets most of the disk back.
18. `ApplyPendingMigrations()` - runs the project's own
    `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` blocks. The DDL comes
    from a file compiled into the binary, never from a parameter, and is
    idempotent by construction. This single operation removes the most
    common reason we reach for `psql` today: an upgrade that added a
    column to a schema file nobody re-applied.
19. `CreateMissingIndexes()` - same shape, compiled-in list,
    `CREATE INDEX CONCURRENTLY IF NOT EXISTS`.
20. `ReindexTable(table)` / `AnalyzeTable(table)` / `VacuumTable(table)` -
    closed enum over this project's own tables. `AnalyzeTable` is the fix
    for "dashboard birden yavaşladı" more often than anything else.
21. `ShowSlowQueries()` - read-only, from `pg_stat_statements` when it is
    available and skipped cleanly when it is not.

**C. Access, and getting locked out**

22. `SendOwnerPasswordReset(userID)` - issues a single-use link to the
    address already on the account. **We never see, set or choose a
    password.** The distinction matters: an operation that set a
    password would be an operation that impersonates the owner.
23. `DisableTOTP(userID, reason)` - the genuinely stuck case, phone lost
    and no recovery code. Deliberately the loudest operation in the
    catalogue: it requires a written reason, writes an audit entry, and
    raises a banner in the owner's panel that only the owner can dismiss.
    It is the entry most open to abuse, so it is the entry that cannot be
    performed quietly.
24. `EndAllSessions(userID)` - the stolen-laptop button.
25. `UnlockLoginThrottle(emailOrIP)` - clears the throttle after a
    customer locked themselves out guessing their own password.
26. `GrantOwnership(userID, siteID)` - recovery from "the only owner left
    the company", which otherwise strands a deployment permanently.
27. `RevokeAPIToken(id)` / `RevokeAllTokens(siteID)` - the leaked-token
    response, at the speed a leaked token deserves.
28. `SetDeveloperMode(userID, enabled)` - turning a technical customer's
    own developer view on for them over the phone, rather than talking
    them through finding the toggle.

**D. The beacon**

29. `ShowBeaconStatus(site)` - last event, events in the last hour,
    rejected count **and the reason for each rejection class**. Read-only
    and always the first question: "JS verisi hiç gelmiyor" is usually a
    site not on the allowlist, and today finding that out means reading
    logs over SSH.
30. `ShowBeaconSnippet(site)` - the exact `<script>` tag with their site
    ID already filled in. Removes an entire category of support call.
31. `SetBeaconSites(sites)` - the allowlist, validated against the site-ID
    character set.
32. `SetBeaconBuffer(size, batch, flushSeconds)` - bounded.
33. `TestBeaconIngest(site)` - writes one synthetic event, flagged as
    synthetic so it never pollutes real figures, then reports whether it
    arrived and how long it took. Proves the whole path without asking
    the customer to go and load their own site while we watch.

**E. Depth, logging and profile**

34. `SetAnalyticsProfile(site, profile)` - `hafif` | `dengeli` | `tam`.
35. `SetVerboseLogging(site, minutes)` - 1..120, self-expiring, because
    verbose logging left on by accident is how the disk fills.
36. `SetLogRetention(days)` / `SetLogLevel(service, level)` - closed
    enums.
37. `ExportDiagnosticBundle()` - one file containing settings, health,
    recent WARN+ logs and table sizes, with values redacted. For the
    cases that do end up needing a human to read everything at once,
    without that human needing a shell to collect it.

**F. Process control**

38. `RestartService(service)` - closed enum over our own four services.
    No command string, no arguments, no path. Implemented as **exit
    cleanly and let systemd restart us**: the panel never spawns
    anything. That is the important property - a repair surface that can
    only *stop* processes cannot be turned into one that starts arbitrary
    ones, no matter what is done to it.
39. `ReloadConfig(service)` - re-read what remains in the file, without a
    restart.

That is thirty-nine operations, and the shape of the list is the point:
almost all of them are settings changes and read-only inspections, and
the handful that are genuinely powerful (23, 26, 15) are each wrapped in
something that makes them impossible to do quietly.

### For any of this to work, the settings have to move to the database

The catalogue above quietly assumes something that is **not true today**,
and the assumption is the real work: an operation can only change a
setting that is changeable at runtime. Right now almost every value in
section A lives in a TOML file, so the actual repair procedure is `ssh` +
`vim` + `systemctl restart` - precisely what this whole design exists to
avoid. Writing the catalogue therefore forces a migration, and the line
has to be drawn deliberately rather than discovered halfway through.

**Stays in the config file** - only what must exist *before* the
database can be reached, plus what identifies this machine:

- `timescale_dsn` - unavoidable and self-evident: you cannot consult the
  database to find out how to reach the database.
- `listen_addr` for each service, and `backend_addr` / `mode` for the
  proxy. Changing where a process listens is a restart anyway.
- `tls.cert_file`, `tls.key_file` - read at startup, tied to the
  filesystem.
- `site_id` for the collector - it names which deployment this is, and a
  deployment that could rename itself from the database is a deployment
  that could be talked into writing into someone else's site.

Roughly eight keys. Short enough to read in full during installation and
then never open again, which is its own goal.

**Moves to `panel_settings`, read live:** `trusted_proxies`; the whole of
`[limits]`; the whole of `[asn_lookup]` including both blocklists and the
scoring list; `flush_interval_seconds`; the beacon's buffer sizes and
batch sizes; the beacon's site allowlist; the analytics profile and
per-site depth; retention and compression policies; the bot-score
threshold; cache window and TTLs; log level and log retention.

**How a running service notices.** Not a signal and not a restart: each
service re-reads its settings row on a short interval and swaps the live
values atomically. The operation modal waits for that acknowledgement
before reporting "uygulandı", so the word means the change is in effect
rather than merely written down.

**The cost, stated before it surprises anyone:** this makes the database
a dependency of the collector's *behaviour*, not only of its storage. So
the failure mode has to be written deliberately - **if the settings read
fails, keep the last known values.** The naive version (on error, fall
back to defaults) would silently reset a customer's tuning during a
database blip, which is a worse outcome than a stale setting and far
harder to notice.

### The operation journal

The audit log answers "who did what". Diagnosing a break needs "what
happened while they did it", which is a different and much more verbose
record, so it is a second table with its own much shorter retention.

Every settings change becomes an **operation** with:

- a correlation ID, carried through every log line the change produces
- the actor and the audit entry it belongs to
- the setting's value before and after
- each step and its outcome
- on failure, the full error chain and whether the change was rolled
  back

That last field matters most. "Bir şeyi ayarlarken hata olmuş" is only
answerable if a half-applied change is recorded as half-applied rather
than silently left in place.

### The modal that streams the operation

Every settings change opens a window that streams that operation's own
log lines, then closes. Two corrections to how this should work:

- **It streams the operation's lines, not everything.** A modal showing
  the whole system's log during an unrelated change is noise, and noise
  is what people learn to click through without reading. The
  correlation ID is what makes this possible.
- **Nothing is ever padded or invented to look busy.** The stated
  secondary purpose is to make a non-technical customer think twice
  before playing with settings, and real system logs already do that
  perfectly well. Fabricated or inflated lines would be theatre, and
  the first technical customer who reads them carefully would stop
  trusting everything else the panel says. The deterrent has to be a
  side effect of honesty, not a feature - otherwise it costs more than
  it buys.

### Log persistence, and its own budget

All four services already log through `log/slog`. A second handler
writes to a `panel_logs` table so the panel can show them without SSH.

The obvious trap: a log table becomes the largest table in the
database, which is precisely the disk problem identified above. So:

- WARN and above persisted by default.
- A per-site "ayrıntılı kayıt" toggle that raises it to DEBUG **and
  expires by itself** (an hour, say). Verbose logging that stays on
  because somebody forgot is how disks fill.
- Its own retention, much shorter than the analytics tables'.
- Bounded line length, and the same NUL/UTF-8 sanitising
  `internal/beacon` already needs - log lines contain user-controlled
  text, and one hostile string must not be able to break the writer.

### The system health page

The single highest-value thing for "minimize VDS entry" is not repair,
it is knowing what is wrong before anybody calls. A read-only page
answering, per deployment:

- is the collector flushing, and when did it last succeed
- is the beacon receiving events, and when was the last one
- how large are the tables, is retention configured and running
- are the IP range tables loaded and how old are they
- is the read API reachable
- recent write failures, dropped-event counts, throttled logins

All of this is already measured internally today - the counters exist,
nothing surfaces them.

### What still requires SSH, stated plainly

A panel cannot repair a panel that is not running, and no catalogue
changes that. These stay physical:

- the process will not start, or crashes on boot
- the database will not start, or will not accept connections
- the disk is full at the OS level - once writes fail, the operation
  journal that would record the repair cannot be written either
- TLS certificate renewal, and anything else about the filesystem
- upgrading the binary
- restoring from a backup
- the eight config-file keys above: DSN, listen addresses, TLS paths,
  `site_id`
- the panel itself being the broken component

The honest observation about that list: **every entry is an install-time
or machine-level concern, not an "analitik yanlış / veri gelmiyor /
ayarı değiştir" concern.** That is the actual claim being made here.
"Everything without SSH" is not achievable and is not what is being
promised; what is achievable is that the entire class of problem
customers actually telephone about is repairable from the panel, and the
residue is the class where the machine itself needs a human.

### Health monitoring: we poll them, they never call us

An earlier draft of this note proposed an outbound heartbeat - the
deployment reporting to us. That was the wrong direction and is
rejected: a customer's server opening unrequested connections to its
vendor is a fair definition of the back door this design exists to
avoid, and it is unpleasant to explain no matter how it is configured.

The correct shape is a **pull**: an authenticated health endpoint on
the deployment, which our own system polls on a schedule. Nothing
leaves the customer's machine unasked; when a poll reports trouble we
telephone them and ask for developer access, exactly as if they had
called us first. Being able to make that call before the customer
notices is most of the value.

### The support token: read everything, write nothing

An earlier draft proposed restricting this to a health-only scope. That
was too narrow for the actual job. Monitoring a customer's deployment
means answering questions that need the analytics themselves: is their
SEO working, are real visitors arriving at all, is a newly launched site
getting traffic, did the numbers fall off a cliff last Tuesday. None of
that is visible in a liveness check.

So the support token reads **everything the read API serves**, across
every site - it is an ordinary wildcard token in the shape
`api.Token{Sites: []string{"*"}}` that already exists. What makes it
safe is not its scope but the wall behind it:

- The read API's database role can only `SELECT`. A token cannot write
  through an API that has nothing to write with, so "read-only" is
  enforced by PostgreSQL rather than by application code that could
  have a bug in it.
- Only the SHA-256 is stored, at both ends. A leaked database or config
  file hands over no working credential.
- It reaches only the read API. It is not a panel session: it cannot
  change a setting, add a member, mint another token or approve
  developer access.

Health is simply one of the things it can read, rather than the only
thing.

**The part that has to be got right, stated plainly:** this is standing
access to a customer's business data after handover. Traffic volumes,
which pages sell, when the campaigns landed - that is commercially
sensitive information about someone else's company. Read-only does not
make it not access.

What separates legitimate vendor monitoring from the back door this
design exists to avoid is not the capability. It is whether the owner
knows and can say no. So:

- The token is **listed in the owner's panel** under a plain name -
  "Crucible destek erişimi" - beside their own API tokens, not hidden
  in a developer-only page they never open.
- It is **revocable in one click**, by them, without asking us.
- Its **last use is shown**, so "are they actually looking at my data"
  has an answer they can check themselves rather than take on trust.
- Creating it is an audit entry like any other.

A customer who revokes it loses proactive support and keeps everything
else; that trade is theirs to make. Making it revocable is also what
makes it honest to describe the deployment as theirs.

The endpoint itself belongs on the read API, which is already
token-authenticated and already read-only. `/healthz` stays as it is -
unauthenticated, dataless, for load balancers - and the detailed
version is a separate authenticated route.

## Injection and the database's blast radius

"Enjeksiyon olmasın, veritabanında açık kapı olmasın" deserves a
concrete answer rather than a reassurance, because it is already mostly
true structurally and the remaining work is keeping it that way.

What holds today:

- Every query in every package uses bound parameters. Values never
  become SQL text.
- There are exactly two places where a string is formatted into a
  query, both interpolating a **column name from a closed set of
  package constants** that no request can reach:
  `api.Store.countDistinct` and `api.Store.beaconBreakdown`. Both carry
  a comment saying so, and `breakdownExpr` is a distinct type
  specifically so a request-derived string cannot be passed by
  accident.
- Role separation limits the blast radius even on total compromise of
  one component. The panel's role cannot read `traffic_snapshots` or
  `beacon_events` at all; the read API cannot write anything; the
  beacon can only insert into one table.

What to add:

- **A test that fails when a new `Sprintf` appears near SQL.** Grep the
  package source for query-building patterns and assert the set of
  interpolating call sites matches a known list. It is a crude check
  and it is exactly the crude check that catches the one careless line
  added at 2am two years from now.
- Settings values reach SQL only as bound parameters, never as
  identifiers. A setting that must name a column or table is a design
  error; the mapping belongs in Go, keyed by a validated enum.
- The panel's repair operations validate their typed parameters against
  explicit bounds before any of them reaches the database.

## Panel design principle: plain surface, unlimited depth

"Görsel açıdan sade, derinlik açısından uçsuz bucaksız." Concretely,
what that has to mean when writing the pages:

- The default view is six cards and one chart, in ordinary Turkish. No
  fingerprints, no ASNs, no scores, no English jargon.
- Every number is a door. A card is clickable and leads somewhere that
  explains it; nothing is a dead end.
- Developer mode adds **layers to the same pages**, not new pages full
  of jargon. The pages page gains columns; it does not become a
  different pages page.
- Nothing technical is visible until asked for, and asking for it is one
  toggle in one place.
- Depth is reached by clicking, never by reading. If a view needs a
  paragraph of explanation to be usable, the view is wrong - the
  paragraph belongs in a tooltip on the one term that needs it.

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

## Page/path, referrer, user agent and session analytics

**Resolved by the beacon (source 6 above); the collector-side half is
still open.** Kept in full because the reasoning is what decided the
architecture, and because the collector-side option below is still a live
choice for something the beacon structurally cannot do.

This was the single biggest gap between what the collector produces and
what a customer expects from the word "analytics", and the obvious first
instinct - "just add a `/pages` endpoint" - was never going to work.

### What's missing from the *collector*, and why, per mode

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

Session/pageview/bounce-rate concepts don't exist in either mode at the
proxy level: there is no cookie, no client-side identifier, and no notion
of a visit - only IP-level activity. That is what the beacon supplies.

### Why "just record the path" *in the collector* is harder than it looks

Still deferred, and still worth doing eventually - the beacon does not
replace it, because a path scanner probing `/wp-admin`, `/.env`,
`/phpmyadmin` never runs a line of JavaScript and is therefore invisible
to the beacon by construction. Collector-side path visibility is the only
way to see it.

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

### What was built instead: the JS beacon (`internal/beacon`)

The conventional approach (Umami, Plausible, Matomo, GA): a script tag on
the customer's site posting pageview, referrer, screen size and an
identifier. It yields exactly the missing fields, works identically in
both collector modes because it doesn't depend on the proxy at all, and
is a well-understood pattern.

The decisive argument was that the two sources are **complementary rather
than redundant**:

- A JS beacon only fires for clients that execute JavaScript, so it is
  nearly blind to bots - which is why conventional analytics
  systematically under-reports automated traffic.
- The collector sees every connection including the ones that never run a
  line of JS, and fingerprints them.

Together they answer both "what did real humans do on which pages" and
"what actually hit the server, human or not" - and the second is the
thing conventional tools can't give. `beacon_events.ip` is the join key
that makes it one question rather than two.

Decisions worth not re-litigating:

- **Its own binary.** It is the only component the entire internet may
  POST to, and it writes; sharing a process with either the collector or
  the read-only API would give away a guarantee each of those currently
  has.
- **Bot user agents are flagged (`is_bot_ua`), never dropped.** A client
  that runs JavaScript *and* admits to being a bot is the most
  interesting row in the table. The real end-to-end run demonstrated
  this: Playwright's headless Chromium was correctly flagged.
- **Query strings are allowlisted, not stored raw** (`utm_*`, `ref`,
  `gclid`, `fbclid`, `msclkid`). Real query strings carry reset tokens and
  email addresses, and an analytics table has a wider audience than the
  application's own database.
- **`asn_lookup` stays off in the beacon when a collector shares the
  host.** Geography is recoverable by joining on `ip`; enabling it loads a
  second ~135 MB copy of the range tables. When it *is* enabled, the
  beacon sets `SkipRangePersistence` - which is the same
  `TRUNCATE`+`COPY` collision the deployment-topology note above warns
  about, showing up for real inside one machine.

### Resolved: unique IP is not unique visitor

This used to be an unavoidable caveat wherever `unique_ips` was presented
as "visitors":

- **CGNAT undercounts.** Mobile carriers (including the major Turkish
  ones) put many subscribers behind one address.
- **Dynamic addressing overcounts.** One person across several days
  appears as several IPs.

Neither is fixable at the IP layer. `beacon_events.visitor_id` is the
answer: `HMAC(daily_salt, site_id ‖ ip ‖ user_agent)` with the salt held
only in memory and rotated every 24h. Two browsers behind one CGNAT
address separate; one IPv6 device separates *less* than it would
otherwise, because the address is truncated to its `/64` first (RFC 8981
privacy extensions rotate the low 64 bits daily and would otherwise mint
a new "visitor" every rotation).

The trade, stated plainly: restarting the beacon mid-day counts one
visitor twice. Persisting the salt would fix that and would also make the
IDs recoverable from a database backup - which is the property the
accuracy is being spent to avoid. `unique_ips` from `traffic_snapshots`
still carries the old caveat; it is the right number for "how many
addresses hit us", not for "how many people".

### Read endpoints for `beacon_events` (built)

17 endpoints under `/beacon/` and `/crossover/`, served by the same
read-only `analytics-api` process. Decisions worth not re-litigating:

- **Sessions are derived at read time, not assigned at ingest.** A
  session is one visitor's events with gaps under 30 minutes, computed
  with `lag(time) OVER (PARTITION BY visitor_id ORDER BY time)` - which
  is exactly what `idx_beacon_events_visitor_time` exists for. Assigning
  session IDs at ingest would make that path stateful, needing
  per-visitor memory whose cardinality an attacker controls: the same
  objection that keeps the collector from keying by (IP, path).
  The timeout is a constant, not a parameter, so two charts in one panel
  can't report different session counts for the same traffic.
- **`bots` is an explicit parameter defaulting to `exclude`.** There is
  no single honest answer - a customer asking for "pageviews" means
  human ones, but silently discarding automated traffic is the exact
  behavior this project criticizes in conventional tools. A named
  parameter with a documented default and the value echoed in every
  response resolves it without hiding anything.
- **`bot_score_min` on a `/beacon/` route is a 400, not an ignore.** It
  is the collector's behavioral score and beacon events have no such
  column, so accepting it would hand back an unfiltered number to a
  caller who believed it was filtered. Quietly wrong numbers are the
  failure this project has already paid for once (`EstimatedRequests`).
- **`/beacon/countries` falls back to `traffic_snapshots`.** The
  recommended deployment leaves the beacon's own geo lookup off, so
  without the join on `ip` the endpoint would return one large empty
  group for every correctly-configured install. The beacon's own value
  wins where it has one.
- **`/crossover/` takes no `bots` filter.** The question is whether
  anything from an address executed JavaScript; a headless browser that
  ran the snippet really did run it, whatever its User-Agent claims.

The endpoints worth having built this for are the crossover ones. Top
pages and referrers are table stakes that any tool has; "which addresses
hit us but never ran JavaScript", "what share of our traffic is
JS-capable at all", and "which clients render pages *and* look
automated" need both sources and are what a beacon-only tool structurally
cannot answer.

### Still open on the read side

- **No beacon numbers in `/api/v1/overview`.** That endpoint aggregates
  several sites' collector figures in one request for a panel's landing
  page; the beacon equivalent would need a second cross-site query and
  a decision about what a mixed row means when one site has only one of
  the two sources running.
- **Sessions are truncated at the range boundary.** One that began
  before `from` is cut at it, so a very narrow range inflates session
  counts and depresses durations. Standard for range-scoped
  sessionization, and the reason a panel should prefer whole days - but
  it means session counts are not additive across adjacent ranges.
- **Session-heavy endpoints sort the whole range.** `summary`,
  `timeseries`, `entry-pages` and `exit-pages` sort by (visitor, time)
  for the window function. Fine at one site's volume on a local
  database; a continuous aggregate would be the answer if it ever isn't.

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

## Where the work stands (2026-08-17, later)

Since the entry below, four more things landed: the panel's rendering
layer and binary (C1), language packs (C1.5), first-run detection and
the developer wizard (C2), and the decision to fetch the known-bot
dataset rather than redistribute it (A10). PLAN.md §0.5 carries the
current counts; the rationale for each is at the end of this file.

A modularity measurement was taken at that point and is worth recording
as a number rather than an impression: **13 of 21 packages are leaves**,
with no internal dependencies at all. The graph is shallow, one
directional and acyclic, and the widest fan-out is six. Between
packages the boundaries hold. *Inside* two of them they have not:
`internal/panel` is 4,424 lines across 16 files carrying at least four
separate responsibilities, and `internal/api` is 3,315. AI.2 takes the
first cut, before C4 adds to the same place.

## Where the work stands (2026-08-16, earlier)

PLAN.md's §0.5 is now the at-a-glance status and is updated at the end of
every phase; this note exists so a reader who opens NOTES.md first is
sent there rather than reconstructing it from the history below.

Since the panel arc began, eleven things have landed:

- **Campaign parameters became dimensions.** They were one flat string,
  which could answer "which exact campaign link performed best" and could
  not answer "how much did Instagram bring in total" - the second being
  the question anyone actually asks. Typed columns, per-dimension
  breakdowns, and a filter that reaches every other view. The filter cost
  a seventeen-query parameter renumbering, which is guarded two ways
  because a wrong position does not fail, it answers a different
  question.
- **A structured log tree**, one directory per service per day per
  category, with the values sanitised against log injection and anything
  credential-shaped redacted.
- **Trust decisions recorded with both halves** - what the client
  claimed and what the server concluded. The code already refused to
  trust clients; what was missing was the record, and without it "why do
  all my visitors share one IP" has no answer short of SSH.
- **panel_settings**, with bounds checked when writing *and* when
  reading, so a row from an older build cannot hand a running service a
  value outside what it was written against.
- **A three-stage log life** - plain, compressed, deleted - with the
  categories somebody asks about a year later kept far longer than the
  ones that fill the disk.
- **Live settings**, so a change made in the panel reaches a running
  process. This needed one more database grant, and the decision to take
  the narrowest of the three available routes is written up in
  internal/settings' package comment.
- **A setup wizard that ends by checking itself**, listing what the panel
  can never do and then verifying it rather than asking the installer to
  remember.
- **Retention that a chunk can actually express.** Real TimescaleDB
  policies rather than a nightly DELETE, set at the *longest* retention
  any site asks for so the policy can never drop a row somebody still
  wants, with a targeted delete for the sites that asked for less.
- **No mode stores a raw address.** Two modes, both masking; what "full"
  buys is a keyed token, which is the precision anybody actually wanted
  from it. Leaving masked mode needs the developer password *and* a key
  already on disk, because without the key full mode does not fail, it
  degrades into masked while the setting still says "full".
- **A second password on the settings that carry legal weight**, asked
  every time, hashed, read from the config file, and enforced on the
  single write path so a call site added later cannot forget it.
- **Seeing a setting and changing it became different questions.**
  Developer mode is a *page*, not a permission: the customer opens it,
  reads every value with its source and its reason, and finds no control
  on the ones that would break the deployment or create a legal problem.
  Config-file settings are listed read-only, secrets by presence only.
  A warning at the door says plainly that looking carries no risk.

The largest remaining gap is stated plainly because it shapes what to do
next: every one of those is a callable function and none of them is
clickable. `cmd/panel` does not exist. Group C is what turns a tested
data layer into a product, and everything under it is already tested.
That is the phase now in progress, starting with C1 - the catalog,
templates and embedded assets everything else renders through.

## Masked addresses, and a second password on the settings that carry weight

Two things landed together because they answer one question from
opposite ends: what personal data does this keep, and who gets to change
that answer.

### The default is the decision

Counsel's answer on IP storage was masked, so masked is what the
software does. The option to store whole addresses exists, but almost
nothing about this change is about the option - it is about the default,
because the default is what runs.

Nobody installing this will read every setting. Whatever the unread ones
say is what ends up in production, on every deployment, permanently. So
every path that could produce a mode now produces `masked` when it has
nothing better to go on: an absent `[privacy]` section, an empty
`ip_storage` key, a settings row containing a typo, a `RowOptions` a
future caller forgot to fill in, a `beacon.Server` built without the
field. None of those is a case anyone plans for. All of them happen.

There is one deliberate asymmetry. `privacy.ParseIPMode` falls back
silently, because it is read on the request path from a table a running
service does not control, and a bad value there must not stop the
service. `config.Validate` refuses the same bad value outright, because
somebody is standing at the file and can fix it, and a deployment that
wrote `tam` expecting whole addresses should be told it did not get them
rather than finding out in a year.

### The ordering is the whole trick

Masking happens when the row is built, and as the last step before it is
built. The whole address does two jobs first: it derives the cookieless
visitor id, and it resolves country and ASN. Then it is masked, and the
masked value is what exists from there on.

Get that order wrong and nothing breaks. Two visitors behind one /24
would collapse into one visitor id, and the geography would resolve the
network's registration instead of the visitor's. No error, no log line -
just numbers that are quietly different from the ones the customer had
last week. That failure mode is the reason there is a test asserting
which address the resolver was *asked* about, rather than only asserting
what came out.

The masking itself lives in `internal/privacy` rather than in each
writer, and that is not tidiness. The collector and the beacon write the
two columns the crossover join compares. Two implementations that
disagree by one bit would make that join return nothing, and returning
nothing looks exactly like "this customer has no crossover traffic".

### What masking costs, said out loud

The crossover join still works, at /24 resolution. Two visitors in one
/24 become one row there. That is a real loss on the project's own
distinguishing feature, and the honest thing is for the affected views to
say so - not to show a smaller number with no explanation, and not to
hide the feature. That is D5's rule, and it now has a concrete case to
apply it to.

Switching modes is not retroactive, which raises a question the panel has
to answer honestly: it cannot claim "all addresses are masked" when some
rows predate the switch. It does not need a per-row column for that. The
setting change is already in the append-only audit log, with its old
value and its timestamp, so the panel can say when masking started
instead of implying it was always on.

### Why the second password is not the panel password

The panel password answers "who are you". Changing whether whole IP
addresses are stored asks something else: "are you entitled to make this
particular change". If those are the same key, then everyone who can
reach the panel - the customer, their intern, whoever picked up a stolen
session - can quietly widen what this system keeps about people. Those
are not operational settings. They are the ones somebody has to answer
for afterwards.

So the second password comes from the config file, which means changing
it needs a shell on the server, which means neither the customer nor
anything that compromised the panel process can change it without also
getting one. It is stored only as an argon2id hash. A plaintext
`password` key exists in the config struct purely so that putting a
password there is a startup error with a useful message instead of an
unrecognised key that TOML drops without a word.

### Making the rules structural rather than remembered

Every rule here is enforced by something that fails loudly, because a
rule enforced by everyone remembering is a rule with a half-life:

- *Asked every time.* `Verify` returns an `Authorization` that names one
  action and expires in seconds. A handler that stashed one to save the
  user a second prompt would find it useless a moment later, and there
  is a test that says so.
- *Cannot be forged.* The field that makes an `Authorization` valid is
  unexported and has no setter. Another package can declare one; it
  cannot make a valid one. That is the compiler, not a review checklist.
- *Cannot be moved sideways.* Verifying to change the log retention does
  not authorize turning masking off.
- *Cannot be routed around.* `SetSetting` refuses guarded keys outright.
  Only `SetGuardedSetting` writes them, and only against an
  authorization. A call site added next year cannot forget a check it is
  unable to compile without - which is why the check lives on the write
  path rather than in the handler that happens to exist today.
- *Reset is a write.* `ResetSetting` is guarded too. For
  `campaign.drop_params` the default is the empty list, so "reset to
  default" means "start storing utm_term again". A gate covering writes
  but not resets would have a way around it that reads as tidying up.

Two smaller decisions worth recording. The gate fails closed when no hash
is configured, which is only acceptable because the defaults are the
privacy-preserving values - fail-closed on a setting whose default was
the permissive one would be a different and much worse decision. And
verifications are serialised behind a bounded queue, because one argon2
run is deliberately ~19 MiB: an endpoint that runs one per request is an
amplifier pointed at the machine the collector is supposed to be
protecting. The gate that took the site down when somebody leaned on it
would not have been protecting anything.

### Why it is its own package

`internal/devgate` knows nothing about databases or HTTP. It verifies,
throttles, and reports; the caller supplies an audit hook. That is what
lets it be imported wherever a guarded operation turns up next -
purging analytics, exporting a customer's data, rotating a token -
without dragging the panel's data layer along behind it.

`internal/argon2id` came out of the same work for a duller reason: the
panel and the gate both need PHC parsing with bounds checks on what a
stored hash claims about its own cost, and two copies of those checks
would be two places to forget them. The whole value of a bound like
"refuse m=16777216 before argon2 sees it" is that it is never missing.

## Seeing a setting and changing it are different questions

The first version of the developer password got this wrong in a way
worth recording, because the mistake is the natural one.

Settings marked `Developer: true` were hidden from anyone not in
developer mode, and guarded ones simply refused writes. Both halves
follow from treating "may they change it" as the only question. It is
not. The customer's own deployment behaves the way these settings say,
and a setting they cannot see is a setting they cannot ask about - they
would be left unable to account for their own system, and every question
about it would become a support ticket.

So the rule is now: **every setting is visible to anyone who reaches the
settings page, and what is withheld is the control.** Four renderable
states rather than a writable flag, because the panel has to say
different things:

- `writable` - an ordinary control.
- `gated` - a control plus the password, asked every time. Operator only.
- `locked` - value, reason, a lock, no control. This is what a customer
  sees on the settings that carry legal weight.
- `read_only` - value, explanation, no control. Operator-owned but not
  legally loaded.

The customer gets the value, where it came from (default, deployment-wide
or per-site), and the setting's own reason - "this decides whether whole
IP addresses are stored" rather than "you cannot change this". A missing
control with no sentence attached is indistinguishable from a bug, which
is why `SettingsView` refuses to produce one: there is a test asserting
that every non-editable row carries a lock notice and every editable one
does not.

### The password field is not shown to somebody who cannot have it

That sounds like a UI nicety and is not. The failure counter is shared
across the process. Five wrong guesses close the gate for fifteen
minutes - which is correct against an attacker and catastrophic if the
guesses come from the customer, because the party locked out is the
operator, in a deployment the operator is responsible for. A security
control that a customer can trip on their own behalf is a denial of
service wearing the right clothes.

So a customer's attempt is refused on **who they are**, before any argon2
work and without touching the counter: `GateRequest` produces no actions
for a principal who may not attempt, and `ApplySetting` checks
entitlement before it looks at the authorization at all. A customer
holding a genuinely valid authorization still cannot write - the refusal
does not depend on what they supplied. There is an integration test that
fires twenty guesses from a customer and then checks the operator can
still get in.

### One route to operator status, not two

Writing this, the first version of `operator()` accepted either
`Superadmin` or a developer-kind principal. A test built a developer
principal without superadmin and produced an incoherent result: classed
as the operator by one check and refused by the next.

The real `developerPrincipal()` is always superadmin, so the second
clause never mattered in production - it was a weaker path to privilege
that nothing used. Those are worth deleting on sight. Something
eventually constructs the object the weaker path accepts, and it is
granted authority by accident. One condition is checkable; two are a
question nobody re-asks.

## Retention: the per-site setting a chunk cannot express

Nothing in this project deleted a visit record until now. Both
hypertables grew forever, on a machine that also serves the customer's
site, where the first symptom of a full disk is the collector failing to
write - an analytics feature taking down the traffic path, which is the
outcome refused everywhere else in this design.

The obvious implementation is a nightly `DELETE`. It is the wrong one:
TimescaleDB stores a hypertable as time-ranged chunks, and dropping a
chunk unlinks a file while deleting rows rewrites pages, updates indexes
and leaves the space to VACUUM. At a year of traffic those are not two
versions of the same operation.

Then the collision: retention is a **per-site** setting, and a chunk
holds every site's rows for its time range. "Site A's rows older than 30
days" cannot be expressed as a chunk drop at all.

The resolution is to let each mechanism do what it is good at. The
hypertable policy uses the **longest** retention any site asks for -
cheap, daily, and structurally incapable of removing data a site still
wants. A site asking for *less* than that gets the difference removed by
a targeted delete, which is the one thing chunks cannot express. In the
ordinary case, where every site uses the deployment-wide figure, the
row-level path never runs at all.

Doing it the other way round is the trap worth naming: a policy set to
the *shortest* value would destroy the data of every site that asked to
keep more, and it would look exactly like the feature working.

### DryRun, because shortening destroys

"90 days to 30" is a number somebody types without picturing what it
removes. `DryRun` reports the row count before anything happens, so the
panel can say "this will delete 4.2 million rows" at the only moment
that is useful. It is tested by asserting that the count is right *and*
that the rows are still there afterwards - a dry run that quietly did
the work would pass a test that only checked the number.

### Remove, then add

TimescaleDB refuses a second retention policy on one hypertable. An
implementation that only ever called `add_retention_policy` would work
the first time and fail on every change after it, and the failure would
read as a permissions problem rather than what it is. So every apply
removes first with `if_exists => true`. Re-applying the same figure is a
no-op, which matters because a service calls this on a timer.

### The table name is a closed set

`add_retention_policy` takes its interval as a value but its table as an
identifier, and an identifier cannot be a bound parameter. So the table
name is the one string in this package that gets interpolated - which is
why `Table` is a two-member closed set, validated at every entry point,
with the count and delete statements written out per table as complete
literals. `Table` is a string type, so a caller can write
`Table(userInput)` and the compiler will not object; the validation is
what makes that harmless, and there is a test that feeds it
`beacon_events; DROP TABLE panel_users`.

### One gap, named rather than hidden

The beacon reads retention from the panel; the collector reads it from
its config file, because the collector has no live-settings reader yet.
Two tables configured in two places is a gap, not a design, and it
belongs to A6-devam. It is written in both config files so that whoever
meets it knows it is known.

## Why a customer may not change a developer setting

Worth writing down plainly, because the reason is not "they are ours" -
that is an assertion, and an assertion is what makes a panel feel
arbitrary.

The reason is what would actually happen. These settings mirror what is
normally given directly in the server's configuration. A customer
changing one from the panel either disturbs how the system runs, or
switches on a technical surface whose output then has to be interpreted -
fingerprints, per-address detail, debug logging. Neither is something to
find out by trying it on a live deployment, and neither is something the
customer signed up to operate. On top of that, seven of them decide what
personal data is kept and for how long, which is a different kind of
problem again.

So the lock notice says three things and the tests enforce all three:
what it is, what would go wrong, and what to do instead - "tell us and we
will connect and do it". The third is the one usually left out, and it is
the one that decides whether the customer feels governed or stonewalled.

Two safety properties came out of stating it this way:

**Guarded implies operator-owned.** analytics.retention_days was guarded
by the password but not marked Developer, so lifting the password guard
would have handed it straight to the customer rather than dropping it
back to read-only. A guard should degrade to the safe state when it is
removed, not to the permissive one. There is now a test asserting every
guarded setting carries both flags.

**Every setting is currently operator-owned.** *(Corrected below - this
turned out to be the over-application, not the rule.)*

### The correction: developer mode is a page, not a permission

The rule above was wrong, and wrong in the direction that quietly takes
capability away. `Developer: true` had been reading as "the customer may
not change this", which locked the entire developer settings page - and
the customer is meant to have full access to developer mode.

Three questions decide access now, and only the first two can withhold a
control:

1. **Does it live in a config file?** Then nobody edits it from the
   panel, the operator included. A listening socket is bound once; you
   cannot ask the database how to reach the database. Offering a control
   there would be offering something the panel cannot honour.
2. **Does it carry legal or ethical weight?** Then only the operator may
   change it, against the password, every time. Seven settings, listed
   explicitly rather than derived from a flag - somebody has to have
   decided that each one really does decide what personal data is kept,
   and a test that read the flag back would only be asserting that the
   code equals itself.
3. **Otherwise** it is ordinary, and whoever may manage settings may
   change it. Customer included, developer mode or not.

Being a developer-mode setting decides which page it appears on. Nothing
else. Conflating that with permission made a log-compression schedule and
an IP-masking mode look identical to the code, when the only thing they
share is that a shop owner does not want either on their front page.

Five settings moved back to the customer: log archiving, log level, the
verbose window, chunk compression, and the beacon's own site allowlist.
The last one is the clearest case - a customer adding their second domain
was exactly the support call the settings table was built to answer
without SSH, and the old rule had taken it away again.

## Showing config-file settings without a control

A customer asking "which address does my beacon listen on" should be able
to look. The value is not the panel's to change - it is read once at
startup from a file on disk - but that is a reason to withhold the
control, not the information.

So there is a registry of the config-file keys with a label and an
explanation each, and the panel renders them read-only. Three details
turned out to matter:

**The notice must say the limit applies to everyone.** "You may not
change this" sends a customer looking for who may. "Nobody changes this
from the panel, the developer included" tells them the shape of the
answer, which is that it needs somebody on the server.

**Unknown and empty are different facts.** A value the panel cannot see
is a fact about the panel; a value that is empty in the file is a fact
about the deployment. Rendering both as a blank would have a customer
chasing something that is set. The entry carries `Known` alongside
`Value` so the panel can say which it is.

**Secrets are named but never carry a value.** A connection string holds
a password and the developer hash is a credential. Both are in the list -
leaving them out would put a hole in the account of how the deployment is
configured - but the fill step skips any entry marked secret regardless
of what the caller passed. The test hands it a map containing real-shaped
credentials for exactly that reason: the guarantee is on the type, not on
the caller remembering.

## Hashed addresses: what the mode buys, and the sentence it does not earn

Counsel's position was: mask and hash the addresses so that even we
cannot know them, and then keeping the ASN name and country is
unproblematic. The mode is built. The premise is not quite met, and
saying so is more useful than building something that lets a false
sentence into a privacy notice.

**What it buys.** No address reaches the disk at all. `ip` is NULL and a
keyed pseudonym takes its place, so a stolen backup, an imaged disk, a
SQL injection or a compromised read-only API yields nothing - none of
them include the key.

**What it does not buy.** An IPv4 /24 has about 16.7 million possible
values. Trying every one against a known key takes a fraction of a second
on a laptop. So the honest claim is "an address cannot be recovered from
the data alone", not "nobody can ever recover it". The key lives in the
same config file as the database password, which means the party who
could reverse it is exactly the party who could already read everything.

Getting to the stronger claim would mean destroying the key after
deployment - and then no new rows can be written and the crossover join
stops working, which is the product's distinguishing feature. That trade
is a decision for the customer and their counsel, not one to make quietly
in code. It is written into the data inventory as an explicit question
back to them.

### Equality is all the join needed

The pleasant surprise: hashing preserves equality, and equality is the
only thing the crossover join uses. Two processes hashing the same masked
address with the same key produce the same pseudonym, so the join still
works at exactly the /24 resolution masked mode gives.

Every crossover query now joins on one shared expression,
`COALESCE(ip_hash, inet_send(ip))`, which yields a comparable value in
either mode. Two properties fall out of it and both are wanted: rows
written before a mode switch never join rows written after (the
encodings differ, so they compare unequal - correct, because nothing can
tell whether they are the same visitor), and a row with neither column
set joins nothing, because NULL is not equal to NULL.

### NULL rather than a placeholder

The address column had to become nullable. The alternative - a fixed
placeholder address in every row - would have kept the constraint and
every existing query compiling, and it was the wrong choice: a query that
forgot to switch columns would join every row to every other row and
return a plausible, entirely false number. With NULL it returns nothing,
which is visibly wrong. Fail-visibly beats fail-silently, especially for
a number somebody will put in a report.

A structural test reads the crossover source and fails on any `a.ip =
b.ip` comparison, because the next person to add a query there will reach
for the obvious spelling.

## The developer-mode warning

What is behind the toggle is bot scoring, TLS fingerprints, rate windows
and attack detection. Those readings mean one thing to whoever built them
and something else to whoever reads them cold: a shop owner handed a JA4
hash and a score of 61 will reach a conclusion, and it will be the wrong
one.

So the dashboard warns at the door - and then lets them in. A warning,
not a barrier. They own the deployment, and locking the page would be
deciding on their behalf what they are allowed to understand about their
own system. The wording says plainly that looking carries no risk, and
that the thing to be careful about is changing something whose effect
they cannot predict.

## No mode stores a raw address

The design settled here after two passes, and the final shape is simpler
than either of them.

Two modes, and both mask. `masked` writes the network and nothing else -
no key, no configuration, which is what lets the safe option be the
effortless one. `full` writes the same network *plus* a keyed token
derived from the whole address. So the raw address never reaches disk in
either mode; what full mode buys is the ability to tell two visitors
inside one /24 apart, which is what the crossover join and the
per-address views actually wanted from "full" in the first place.

That reframing is the whole trick. "Full" had meant "store the address",
and the thing anybody actually needed from it was precision, not the
address. Once those are separated, precision can be had without the
address - and the mode nobody would have chosen for privacy reasons
becomes acceptable on its own terms.

### Two conditions to leave masked mode

Switching to full is a serious act and now needs both:

1. The developer password, like every legally weighted setting.
2. The token key already present in the config file, put there by
   somebody with a shell.

The second is a precondition rather than a permission, and it earns its
own error type. Without the key, full mode does not fail - it *degrades*.
The writers would store the masked address and no token, so the
deployment would sit in masked mode while its setting said "full". A
mode that quietly becomes a different mode is the worst way for this
particular setting to be wrong, because everything downstream keeps
working and reports the wrong thing.

Clearing the setting needs no precondition check, and that is not an
omission: clearing restores the default, and every default is a value the
deployment can always honour. Masked needs nothing on disk.

### The key is generated, not typed

`devpass -ipkey` draws it from the system's randomness. It is not a
password, nobody types it, and the one property that matters is that
*both* writers carry the same value - they write the two halves of the
crossover join, and different keys make that join find nothing with no
error to say why. One value, one place it came from, copied into two
files. The preflight check reports its presence, and the wizard lists it
as a manual step, because it is one.

## The panel becomes a thing you can open

Every layer under the panel was written and tested before this and none
of it was clickable. `cmd/panel` did not exist; `SettingsView`,
`RunPreflight`, `PromptFor` and the rest were callable functions with no
caller. This is the phase that gives them a surface: a message
catalogue, a template set, a stylesheet, one JavaScript library, and a
binary that serves them.

### One file, on purpose

The stack is `html/template` plus htmx, both compiled in. No CDN, no
npm, no build step. That is a deployment decision before it is a taste
one - the requirement all along was that installing this should not mean
"edit nginx here, run this build there" - and it has a second effect
that matters more: the panel works on a machine with no outbound network
at all. A page that quietly fetches a font from a third party would
break exactly the installations this software is meant for, and would
tell that third party who is looking at which customer's panel.

htmx is vendored rather than fetched, and its SHA-256 is asserted by a
test. That file is the one thing in the repository nobody reads during
review, so an accidental edit, a bad merge, or a "patched" build pasted
in by somebody helpful would otherwise ship to every customer unnoticed.

### The catalogue is checked in both directions

A template naming a key that does not exist normally renders as nothing:
a blank space in the middle of a sentence, on a page somebody may not
open for weeks. So the renderer walks the parsed templates at startup,
finds every constant key, and refuses to build if one is missing. The
binary does not start. That moves the discovery from a customer to
whoever changed the template, which is the only person who can fix it
cheaply.

The reverse direction is a test rather than a startup check: a key no
template and no Go source names is an error too. A catalogue that only
grows becomes a file nobody trusts, because the reader cannot tell which
sentences are on a page and which were left behind by a rewrite two
phases ago. The practical consequence is that this phase's catalogue is
small - the navigation labels for pages that do not exist yet are not in
it, and will arrive with the pages.

Two kinds of Turkish text deliberately live outside it. A setting's lock
reason stays beside the rule that locks it, because splitting an
explanation from the thing it explains is the shortest path to the two
disagreeing. And the month names live in the formatter, because they are
not messages - they are an ordered list indexed by `time.Month`, useless
to a translator as twelve separate keys and dangerous as twelve separate
keys, since one edited out of order produces a date that is wrong rather
than untranslated.

### Turkish is not English with different words

`golang.org/x/text` was already an indirect dependency, and it is now a
direct one for two things Go's standard library gets wrong for this
language. Numbers: `1.234.567` and `45,7`, with the percent sign
*leading* - `%45,7`, not `45,7%`. And casing: the capital of "i" is "İ",
the small letter of "I" is "ı", so `strings.ToUpper` on an email address
produces something that no longer matches the address it came from.

What Turkish does *not* need is the machinery an English-first design
grows by reflex. A numeral does not inflect the noun after it - "1
ziyaretçi", "3 ziyaretçi" - so there is no plural rule engine here, and
writing one would have been cargo cult.

### The zone is a correctness question

Every date and time renders in the site's configured zone, and an
unknown zone name is a startup error rather than a fall back to UTC.
Falling back is the tempting choice and it is the wrong one: the panel
would put every timestamp hours away from the customer's clock while the
config file said otherwise. A panel that reports the evening traffic
peak in UTC tells a customer in Istanbul it happened in the afternoon,
and nothing on the page would say so.

### Render into a buffer, then write

`template.Execute` straight into an `http.ResponseWriter` sends `200`
and half a document before discovering a nil field on line forty. The
reader gets a page that stops mid-sentence, under a status code that
says it worked. So pages are rendered into a buffer and copied only on
success, and the failure path serves a 500 page that was rendered once
at startup - because the error path must not depend on the thing that
just failed.

### The policy, and what a real browser found

The Content-Security-Policy allows neither `unsafe-inline` nor
`unsafe-eval`. There is not one inline `<script>`, `<style>`, `style=`
or `on…=` attribute in the templates, and a structural test fails on the
source if one appears - because the sequence otherwise is predictable:
somebody adds one inline handler, the page silently does nothing, and
the fix that looks obvious is to loosen the policy for the whole panel.
Failing at the source makes the cheap fix the correct one. A second test
guards the policy string itself, since a test that only checks templates
would pass happily on the day somebody adds `unsafe-inline`.

None of that caught the actual defect. htmx injects an inline `<style>`
for `.htmx-indicator` when it starts; Chromium refused it under
`style-src 'self'`; and the only symptom would have been a loading
indicator that never hides, months later, on a page nobody had written
yet. It took a real browser to see it, which is exactly the reason this
project keeps insisting on one.

The fix was not to allow the hash. Turning the injection off with
`<meta name="htmx-config">` and carrying the four rules in `panel.css`
is better in both directions: the rules are visible where we can edit
them, and an htmx upgrade cannot quietly change what a hash in the
policy was blessing.

The same run found a second, smaller thing: every page load asked for
`/favicon.ico`, hit the catch-all route, and was answered with a whole
HTML error page. Declaring the icon in the layout stops the request
being made at all.

### Startup failures go to two places

The panel files its logs in the structured tree like every other
service, and that turned out to hide the most important messages it
produces. A panel that cannot reach its database exits `1` having
printed *nothing* to the terminal the operator is standing in front of,
because by then the logger writes to a file. Startup failures now go to
both: the tree for whoever looks afterwards, stderr for whoever just
typed the command.

## A second language, before there were pages to translate

The request was to make the panel language-extensible "so we do not have
trouble later, when building a site for somebody working
internationally". The right time to do it was immediately: the next
phase adds dozens of strings, and every one of them would have to be
revisited.

The retrofit reopened a decision from the phase before it. C1's note
argued that month names belong in the formatter rather than the
catalogue, because they are an ordered list indexed by `time.Month`
rather than twelve independent messages. That reasoning was correct for
one language and wrong for two: the list has to vary per language, so it
is now data, one list per pack, with its length checked at load. The
protection the original argument wanted - that one name edited out of
order produces a wrong date rather than an untranslated one - is now a
real check instead of a placement.

Date *ordering* had to become data for the same reason and it is the
better example: Turkish writes "17 Ağustos 2026" and English writes
"August 17, 2026". A formatter that hard-codes either cannot be
translated at all, only re-written.

### Two rules, deliberately asymmetric

The base pack owns the key set. A template naming a key it does not
define stops the binary from starting, exactly as before.

A translation may be incomplete. Missing keys fall back to the base
language, are reported once at startup with the exact list, and fail a
test.

The asymmetry is the whole design. Refusing to boot on an incomplete
translation sounds stricter and is worse: adding one Turkish string
would break every deployment running English, including the ones whose
readers do not speak English and would never have seen the sentence. CI
is where an untranslated string should hurt. A customer's server at
three in the morning is not.

### Plural rules are a library, not a guess

English inflects after a numeral and Turkish does not - "1 gün", "3
gün", but "1 day", "3 days". That is the point at which an English-first
design reaches for a `one`/`other` pair and declares the problem solved.
It is not solved: Russian needs a different word for 1, for 2, and for
5, and 21 goes back to the form 1 uses. Nobody derives that by hand
correctly.

So the forms come from `golang.org/x/text/feature/plural`, which carries
the real CLDR rules, and a pack supplies only the categories its
language has. Anything it does not supply falls back to `other`. That
fallback is what keeps the mechanism invisible where it is not needed:
the Turkish pack writes `other = "%s gün"` and never learns that plural
categories exist, while a Russian pack can write four and get them.

The test suite carries a Russian pack for this. Turkish and English
between them cannot exercise the mechanism - one never inflects and the
other has two forms - so a design tested only against the two shipped
languages would look finished while being wrong for most of Europe.

### Adding a language is demonstrated, not asserted

The loader takes an `fs.FS` rather than reading the embedded directory
directly, and the test hands it a synthetic file system holding the real
base pack plus a language this repository does not ship. That test
loads it, negotiates to it, renders its dates with its own month names
and its own ordering, picks its plural forms, and checks that its
untranslated keys fall back.

Which is the difference between a claim and a check. "You can add a
language without touching Go" is exactly the kind of statement that is
true when written and false a year later, and the only way to keep it
true is to have something fail when it stops being.

The loader also refuses a pack whose file name and declared code
disagree. They are two independent statements of the same fact, and
somebody asking "why is my pack not loading" would otherwise have
nothing to look at.

### What the validator refuses, and why each one exists

Every check on a language pack stands for a specific way a translation
goes wrong without anybody noticing: eleven months, a date pattern
missing the year, a relative phrase with nowhere to put the number, a
counted unit with nowhere to put the count.

The subtlest is the time layout. Go's layouts are written with a
reference time, so `"gg.aa.yyyy"` - which looks exactly like a date
pattern to anybody who has not met Go before - is printed back
verbatim. Every timestamp in the panel would read `gg.aa.yyyy`, nothing
would error, and the pack would look correct in review. The check
formats two genuinely different instants and requires different output.

### Where the language is decided

Deployment setting first, then the browser's `Accept-Language`, then the
base language. The deployment setting is a choice somebody made and a
browser's default list is not, so it wins; leaving it empty hands the
decision to the reader entirely, which is what serves a team that does
not all read the same language.

There is deliberately no `?lang=` switch. A page that renders
differently for the same address makes every screenshot in a support
ticket ambiguous, and the browser already carries this preference. When
accounts gain a language field it goes in front of the deployment
setting, and nothing else in the resolution changes - the parameter is
already variadic for exactly that.

`Vary: Accept-Language` goes on every rendered response. Pages are
`no-store` anyway; it is there so the two rules cannot drift apart the
day something downstream decides to cache after all.

### `dir` now, right-to-left later, honestly

`<html>` carries `lang` and `dir` from the pack, and the stylesheet was
already written with logical properties. That is groundwork, and it is
worth saying plainly what it is not: no right-to-left pack has been
written or rendered, so nothing here is evidence the layout survives one.
What the attribute buys is that adding one is a layout review rather
than a retrofit of every template.

## The first run, and the door into it

C2 is the phase where the panel stops being a thing that renders and
starts being a thing you set up. Two questions had to be answered before
any of it worked: how does somebody get in when there are no accounts,
and what is a setup wizard actually for when most of what a deployment
needs cannot be set from a browser.

### There is nowhere to sign in, so say that

A fresh deployment has no users. Sending whoever opens it to a login
form is a loop, so the front page becomes the first-run page and prints
the exact command that produces a developer link. Naming the command is
the whole point: "ask your administrator" is useless when the person
reading it *is* the administrator, standing at a shell, which is the
only situation in which that page is ever seen.

The link is minted by the panel binary itself rather than a separate
tool, because it needs that config's database and nothing else, and
because running it requires a shell on the server - which is precisely
the authority the link stands for. It prints to stdout, not to the log
tree: this is the one output of the program a person copies with their
mouse, and burying it in a JSON line beside the day's requests would be
hostile.

It also says out loud which of the two grants just happened. Before
anybody owns the deployment the link is approved on the spot; afterwards
it is inert until an owner says yes. That is the most important property
of the whole mechanism and it stops being true silently, so the output
states it every time.

### A wizard that mostly refuses to configure

The instinct with a setup wizard is a form per setting. Most of what
this deployment needs cannot be set from a browser and should not be:
the database roles (a role cannot grant itself privileges), the schema
(no service runs DDL, deliberately), TLS, the collector's backend.

So the steps that cover those *read the real state and report it*. A
field that writes nothing is worse than a sentence saying where to go,
because the installer fills it in, sees no error, and believes the job
is done. Two of the six steps write; the other four verify, and say so.

The other rule is that each step commits immediately. No draft
accumulating in the session, no "finish" that applies everything at
once. Somebody who gets halfway and closes the tab has left a
half-configured deployment - which is true, and visible on the next
visit - rather than a deployment that looks untouched while a session
somewhere holds their answers.

### The defect the live run found: scope is not decoration

The retention step was written to save both its keys globally. Log
retention is global; **analytics retention is per site**, and the store
refused the write with a message no unit test was watching for.

The fix was not to pass a site through - it was to make the page tell
the truth. It now shows one field per configured site, named after the
site, and says plainly when there are no sites yet that this cannot be
set until the sites step is filled in. A per-site setting rendered as
one global field is not a simplification, it is a different setting.

That is the second time in this project a defect survived every
synthetic test and died on the first real run, which is why the rule
about live dependencies is not negotiable.

### One password, several sites

The gate mints an authorization per *action*, and an action is a key -
so one password covers one key across every site the form named. That is
right rather than lax: the person filled in one form and pressed save
once. Each write is still audited separately, and the next change asks
again, which the integration test checks at the HTTP layer rather than
only in the gate's own tests.

A field returned unchanged is not a change. Without that check, walking
through the wizard with "next" would spend a developer password and
write an audit entry for every value on the page.

### The audit gap

The dev-access actions had been defined for phases and nothing wrote
them. `panel_dev_access` records `used_at` and `used_from` itself, which
is why nobody noticed: the information existed. But that table is a work
list - purged after a month, answering "which links exist" - and the
audit log answers "what happened on this deployment". Somebody asking a
year later who was in here should not have to know a second table
existed.

Redemption is now recorded, in the store beside the rule that decided
it, with a bootstrap grant given its own action. A failed audit write is
logged and does not fail the redemption: the session is already minted
and the row already marked used, so failing then would leave a spent
link and a developer who cannot get in. An append-only log that quietly
stops appending is worse than no log, so it is never silent.

### Two tests that only passed on a pristine database

Adding this phase's integration tests broke two older ones, and the
older ones were right to break. They asserted that the pending
dev-access list had exactly one entry, which is true only in a database
nothing else has touched - and this suite shares one database across
packages that `go test ./...` runs in parallel.

Both were changed to find their own request rather than count the list,
and this phase's tests now clean up the rows they create. The same
lesson as the audit-page test two phases ago: a test that stops testing
once the system has been used is worse than no test, because it keeps
reporting success.

One assertion was dropped rather than fixed, and that is worth being
explicit about. The browser test used to re-read `beacon.sites` from the
database after the browser exited, to prove the save landed. It shares
that global key with another package's suite running in parallel, so the
read was asserting on a value another package is entitled to change. The
proof kept instead is the value coming back on a freshly loaded page -
which the server reads from the database anyway - and the direct
database assertion lives in the test that makes it in the same goroutine
as the write.

## Ship the mechanism, not the data

The repository is public and MIT. Inside it was a snapshot of somebody
else's dataset: 51 JA4 fingerprints from The Bot Aquarium's community
archive, retrieved once and committed. The README named the source and
did not name the terms.

That is a specific kind of wrong, and it is worth being precise about
why. Our licence covers our code. It says nothing about data we did not
create, so a repository that says "MIT, help yourself" while carrying
third-party data under unstated conditions is not being generous - it is
passing an unresolved question to everybody who clones it, and they
inherit it silently.

The fix chosen was not to research the terms and write them down. It was
to stop redistributing. The data now arrives by being fetched, onto the
deployment's own machine, under the source's own terms:

    collector -config collector.toml -update-bot-data

Cron, by hand, from somewhere else - the schedule is deliberately not
this software's business, which is why the mechanism is a plain function
behind a flag rather than a scheduler nobody asked for.

### The cost, said out loud

A deployment that never runs it has **no known-bot signal at all**. That
is strictly less than before, and pretending otherwise would be the
worst outcome of the whole change: a deployment quietly missing a signal
it thinks it has.

So absence is loud. The collector says at startup whether it loaded a
set, or has never fetched one, or was never told where to look. The read
API says whether fingerprints will be labelled. The setup wizard carries
a check that separates *never fetched* from *fetched and stale* from
*never told where to look* - three different facts with three different
next actions, and the last of them is a `skip` rather than a `pass`,
because "we did not look" is not a clean bill of health.

That check is **recommended, never required**. Blocking somebody's
installation over a third party's dataset would be the wrong trade.

### Unpicking a package-level global

`scoring.KnownBotJA4` was a `var` initialised from an embedded file, and
`internal/api` read it in five places. That shape is what made the data
feel unavoidable: a global loaded at init has nowhere to be absent.

It is now `scoring.KnownBots`, a value the caller supplies, and the nil
set is meaningful - `Known` and `Label` handle it, so no caller checks.
Both readers changed the same way: the collector passes what it loaded,
the API store carries it as a field. The empty-fingerprint guard moved
onto the type itself rather than being a filter applied at load time,
because `""` is the sentinel for "no JA4 available" and a set that
contained it would label all non-TLS traffic as a bot. Now it is refused
at lookup, even if a set somehow contains it.

### What the fetcher does that a `curl | jq` would not

Three things, each because of a specific failure:

- **Browser-classified entries are dropped.** The archive is
  community-submitted and includes reference fingerprints for real
  browsers. Keeping them would make the panel call every ordinary
  visitor a known bot - the single most damaging thing this parser could
  get wrong, and the reason it has its own test.
- **The write is atomic.** Temp file, sync, rename, same directory. A
  crash mid-write would otherwise leave a half-written file that the
  collector refuses to parse at its next restart, and the failure would
  arrive hours later wearing a different face.
- **A failed fetch changes nothing on disk.** Yesterday's fingerprints
  are worth far more than none, so `Update` writes only after a fetch
  has fully succeeded.

The response is also read through a byte cap. Not a guess at the file's
size - a bound on somebody else's server, because without it a redirect
to something enormous turns a cron entry into an out-of-memory kill on
the machine that also runs the collector.

### A message that sent me the wrong way

The first version reported "every entry was filtered out; the source's
shape may have changed" for an empty response. It is the wrong sentence:
nothing was filtered, there was nothing. One is somebody else's outage,
the other is our parser meeting a shape it did not expect, and only the
second is worth reading this code over. Two failures, two sentences.

### One live test, and what it is for

The unit tests run against a fixture this repository controls. That
fixture can go on passing forever while the real source changes shape -
and since no copy ships any more, that would mean every deployment
quietly losing the signal with no error anywhere. So one integration
test fetches the real archive and checks that what comes back still
parses into something that looks like JA4 fingerprints.

It found the first useful thing immediately: the live source now has 52
usable entries against the committed snapshot's 51, with 59 more
filtered out as browsers. The data had already moved on from the copy we
were shipping.

## Splitting a package before the phase that would have grown it

`internal/panel` had reached 4,424 lines across 16 files with at least
four separate jobs inside it — authentication, authorisation and
settings, the audit log, and the setup checks. C4 adds login, two-factor
and member management, all of which land in the same package. Splitting
afterwards costs more than splitting first: every phase adds to that
package, and every addition enlarges the surface that would have to
move.

So this went in between phases, deliberately small: two cuts, chosen for
being the highest-value and the least tangled.

### Why preflight, and why it takes a pool

`preflight.go` was the largest file in the repository at 985 lines, and
it was never really domain logic. It is a *diagnostic*: it runs real
queries, stats real directories, makes real requests and reports what it
found. Two of its checks are deliberately negative — the panel's role
must **not** be able to read the analytics tables, the API's role must
**not** be able to write — and those questions are about a deployment,
not about the panel's data model.

The new `Checker` holds two things: a `*pgxpool.Pool` and a boolean
saying whether an IP token key exists. It does not take the panel's
`Store`, and that is the decision that makes the cut worth making rather
than merely tidy. These checks ask what a *different* role may do, and
which tables exist in a schema the panel cannot read. Expressing that as
`Store` methods had grown the panel's data API by a dozen functions that
only ever serve one page — and it meant every test for these checks had
to build a whole panel: users, sessions, audit cleanup, all to ask
whether the beacon's role can write to a table.

One thing the checks genuinely needed from the panel was `GuardedKeys()`,
the list of settings the developer password protects. It now arrives
through `Config` as a `[]string`, supplied by `cmd/panel`. The
alternative was importing `internal/panel`, which would drag the panel's
store, sessions and auth into every binary that wants to run a check.
The rule that came out of it: **if a check needs something from the
panel, it comes through Config.**

### The rule is a test, because comments do not fail

A package sitting in its own directory while importing everything it
used to be part of has been *moved*, not separated — and nothing in the
compiler notices the difference. That is the failure mode of most
splits, and it is invisible until somebody measures again a year later.

So `TestPreflightDoesNotImportThePanel` reads this package's imports out
of `go/build` and fails on any path under `internal/panel`. Test
dependencies are excluded on purpose: the hand-run demo may reasonably
reach for the panel's real key list to show real output, and a test
dependency does not travel into anybody's binary.

This is the same instinct as the CSP tests that scan the source for
`unsafe-inline`. A rule written in a package comment is a rule the next
change contradicts silently.

### The new failure the split created

Worth writing down, because it is the honest cost of the cut and it was
not obvious in advance.

While the checks were `Store` methods, there was no way to hold one
without a database — a `Store` always has a pool. A standalone `Checker`
can be built with nothing. The place that would discover it is the last
step of the setup wizard, one button from handover, where a panic is the
worst outcome available: a blank page at the exact moment somebody is
deciding whether the installation worked.

So a `Checker` without a pool now reports every database check as a skip
that says nothing was examined, and a `nil` `*Checker` takes the same
path. Handover stays blocked, because a skipped required check blocks
`Complete` — which is right. Nothing was verified.

The skip text is attached by a helper that takes the half-built result
rather than returning a fresh one, so each check's ID, label and
severity stay written down in exactly one place. The version I wrote
first had a second list of database check IDs to fill in. That is a
mirror, and this project has already been bitten by mirrored lists
twice.

### The second cut, which was only a rename

`internal/config` → `internal/collector`. The name claimed a scope it
never had: there are five binaries here and every one of them has
configuration, with the beacon's, the API's and the panel's each living
next to the code that reads them. A package called `config`, read from a
call site, says nothing about whose settings it is holding. Only
`cmd/collector` imported it, so the change was one `git mv` and a
package clause.

### Something unrelated that fell out

`internal/api`'s integration test still used `scoring.KnownBotJA4`, the
global deleted in A10. It had gone unnoticed because that file only
compiles under `-tags integration`, and the tag was not part of the
routine check.

The fix is better than the original: the test now supplies its own
`KnownBots` set instead of picking an entry out of whatever happened to
be embedded. The old version would have *skipped itself* once the
embedded list went away — a test that quietly stops testing is worse
than one that fails.

`go vet` now gets run under `integration` and `loadtest` as well as
untagged, which is where this should have been caught.

### What this did not do

The settings family (1,531 lines) and the auth family stay where they
are. They share `Access`, `Principal`, `Role` and `Store`, and splitting
them means either moving those types somewhere common or giving each
sub-package its own store — both of which touch every call site and
every integration test. That is not a between-phases job, and done
halfway it is worse than the current state. It comes after C4, when the
auth family has settled with the surface C4 adds.

`internal/panel` is 3,397 lines now, down from 4,424. Twelve of
twenty-two packages are leaves, there are no cycles, and the only
importers of `internal/panel` are `cmd/panel` and `internal/panel/web`.

### Two races the split shook loose

Neither was caused by the split. Both were latent, and moving 1,000
lines out of `internal/panel` changed how long that package takes, which
changed how the parallel packages interleave, which made both start
failing. That is worth writing down on its own: **a timing change is a
way to discover a race, and the race is the bug, not the timing.**

Every integration suite runs against one database, and `go test ./...`
runs packages in parallel. Two things were global in a way the tests
were not.

**Both suites wrote the same setting rows and both wiped the table.**
`internal/settings`' live test and `internal/panel`'s settings test each
wrote real keys like `logs.retention_days` into `panel_settings`, and
each ended with a bare `DELETE FROM panel_settings`. When they
overlapped, one suite's hand-written row collided with the other's
primary key, or vanished mid-read.

The fix names an owner for every row. `internal/settings`' live suite
now uses a `test.settings.` prefix and deletes only what matches;
`internal/panel` deletes everything *except* `test.`. Nothing was lost:
`Source` takes keys as plain strings with caller-supplied bounds, so a
namespaced key exercises the identical code — and the test now says out
loud that it is not testing the panel's key list, which was always true.

**"This deployment has no accounts" cannot be checked and then relied
on.** `TestSetupFlow` walks the first-run flow, which only exists while
nobody owns the deployment. It already guarded with `if CountUsers() ==
0`, and that guard is unfixable as written: between the count and the
page it renders, the panel suite can create a user. It failed about one
run in three, always with a confusing diff — the page was correct, for a
database that had changed underneath it.

A condition that must hold *for the length of a test* needs a lock, not
a check. Both suites now take a Postgres advisory lock and take turns.
Two details that matter more than they look:

- The connection is pinned with `Acquire`. Advisory locks belong to a
  session; locking on a pooled connection and unlocking on whichever one
  came back next leaks the lock and deadlocks the following run.
- Unlock happens *before* release, on the same connection — otherwise a
  still-locked connection goes back into the pool.

The helper is duplicated in both packages, because they share no
test-only package and inventing one to hold a test helper would be worse
than sixty duplicated lines. Only the constant has to agree, so a test
on each side asserts it against the literal. Two copies that silently
disagree would leave both suites green with the race restored, which is
the failure worth spending a test on.

The remaining conditional in `TestSetupFlow` is deliberate: a previous
run that died before its cleanup leaves real accounts behind, and no
lock helps with that. A dirty database is not a race, and skipping is
the honest answer.

## The customer's door

Every security property behind the sign-in page was written and tested
earlier: argon2id, two independent throttling counters, TOTP with replay
refusal, session-token renewal, the audit log, roles and capabilities,
last-owner protection. C4 wrote no new ones. What it wrote is the part
that decides whether any of them is actually *reached* — which is where
this historically goes wrong.

So the file that matters (`internal/panel/web/auth.go`) is built around
three rules, and each is there because omitting it is invisible.

### The throttle is consulted before the password, not after

A throttle checked only once a password has failed still runs one
argon2id verification per guess. That is the cost the attacker was going
to pay anyway, and it hands them the answer. It has to come first.

### Every failure produces the same page, including the same timing

One sentence, one status. And the password check runs even for an
address with no account — `panel.VerifyDummy` exists for exactly this.
Skipping it answers "does this address have an account here" in about
eighty milliseconds, from anywhere on the internet, for the whole
customer list.

The disabled-account check is *after* the password for the same reason.
Refusing earlier would make "suspended" distinguishable from "wrong
password" without knowing the password.

The throttle message is separate, because that one is actionable —
waiting fixes it — but it names neither the account nor the address.
Which of the two counters fired goes to the audit log, where the
operator can see it; saying "this address is blocked" versus "this
account is blocked" to the person at the form would confirm the account
exists, undoing everything above.

Two integration tests assert the sameness directly: the status codes
must match, and the extracted sentences must be byte-identical.

### The half-finished state is not a session

`Principal` returns `ErrNoSession` for a pending second factor, so every
authenticated page already refuses somebody who stopped after the
password — without any handler remembering to check. The test asserts it
from both sides: the code form opens, and everything else stays shut.

### The destination parameter

A sign-in form that will redirect anywhere is a phishing springboard
wearing the customer's own domain: the address bar reads correctly right
up until the credentials are typed somewhere else.

The interesting cases are not `http://evil.test` — everybody catches
that. They are the ones that look relative: `//evil.test` is
scheme-relative, and `/\evil.test` is the form every hand-rolled check
misses, because browsers normalise the backslash. Both are in the test.

Bad values are **rejected, not repaired**. There is no sanitisation of
`//evil.test` that produces what the sender intended, so it becomes the
site list — which is where signing in leads anyway. `/` is dropped too,
for being noise.

## A session is weaker evidence than a password

Two things on the account page cost the current password: changing the
password, and turning the second factor off.

A session cookie can be copied off a shared machine, lifted from a
laptop left open, or inherited from a browser nobody signed out of. Any
of those gets an attacker the customer's numbers, which is bad. Without
these two fields it also gets them the account permanently, and gets
them the account with the second factor removed — which is worse in a
way that cannot be undone by noticing later.

### The second factor's secret lives in the session until it is proved

Writing it to the user row on the way out is the obvious implementation
and it creates the one unrecoverable state this panel can produce by
itself: an account demanding codes from an authenticator that never
finished scanning. Nobody can sign in, including the person who would
fix it.

So the secret goes into the session, the QR is drawn from there, and it
reaches `panel_users` only after a code proves an app actually has it.
The test walks exactly the abandoned path — start enrolment, do not
confirm, sign in again — because that is the failure, and it is the one
a happy-path test never visits.

### Why the QR has its own endpoint

The obvious way to show it is a `data:` URI in the page. The policy even
allows it (`img-src 'self' data:`). It is still wrong: an embedded
secret is in view-source, in the browser's memory cache, in whatever a
screenshot or "save page as" produces, and in every copy of that markup
somebody pastes into a support conversation.

`/hesap/iki-faktor/qr` renders the secret held by *this session*,
same-origin, `no-store`, and 404 when nothing is being enrolled — a 404
rather than a blank image, because an image that renders empty looks
like a broken page.

### No recovery codes, and that is a decision

Somebody who loses their phone is recovered by an owner or the operator
resetting their second factor. They could already remove that person
entirely, so this grants no new authority. A recovery-code table brings
its own storage, hashing and single-use problems, and it is not free.

The gap it leaves is real and is written down in three places: the
plan, the enrolment page, and the code form. **A sole owner who loses
their phone still needs shell access.**

## Hiding a link is not authorisation

`internal/panel/web/chrome.go` decides what to *draw*. Nothing in it is
allowed to be the reason a request succeeds. Every handler asks for its
own capability again, against the same `Access`.

That rule is only worth stating if it is tested from the wrong side, so
every permission test here has a pair: the thing an authorised person
can do, and the same request forged by somebody who may not. A viewer's
POST to the member page is sent with a **valid CSRF token taken from
their own account page** — a token belongs to the session, not the page,
so somebody refused on one page still carries a good one. Taking the
token from the page under attack would have made the test pass for the
wrong reason: no token, rather than no authority.

Two status codes that are deliberately different:

- **404** for a site the principal has no access to. A 403 confirms the
  site exists, which turns the URL into a way to enumerate a
  deployment's customers from any account on it.
- **403** for a page their role does not open on a site they *can* see.
  They know it exists, they are looking at it, and "this needs a role
  you do not have" beats a page pretending not to be there.

`Access` grew a `SiteID` field during this. An access decision and a
site id travelling as separate arguments can be separated, and a handler
that authorises against one site and then reads another is the bug the
field makes unwriteable.

## What the browser found, again

Third phase running, third defect that every HTTP test was blind to.

**There was no way to sign out.** The route existed, the handler
existed, it was wired into the tree, and an integration test posted to
it and passed. No page had a button. A signed-in person had no exit at
all — and nothing failed, because every test that could have noticed was
calling the endpoint by URL rather than by clicking.

The regression test for it is four lines and lives in `ui`: if the
chrome names somebody, it offers them the door, as a POST form, carrying
a token. It does not need Chromium to run.

**And `autofocus` had quietly broken the skip link.** Putting focus in
the email field on load moves it past the "skip to content" link at the
top of the document, so a keyboard user tabbing from the start can no
longer reach it. That link is an affordance this panel already committed
to and already tested — the browser test caught the conflict on the next
run. `autofocus` came off both sign-in pages.

The other regression was self-inflicted and worth recording for shape:
adding a sign-out form to the header made `button[type="submit"]` in the
wizard's browser script select the *header's* button, so the script
signed itself out instead of saving a form. Scoping the selector to
`main` fixed it. A page's first submit button is not a stable thing to
select on.

## A nil session manager, failing in two directions at once

`Server.Handler` already tolerated a nil `Sessions` — a leftover from
before there was a login — and the new `requireUser` called it
unconditionally. Nil pointer, on the front page.

Both halves needed fixing, in opposite directions:

- Every `Sessions` method reachable before authentication now answers
  safely for a nil receiver, and answers **closed**: no session, no
  token, and `CheckCSRF` returns false, so every write is refused.
- `ListenAndServe` refuses to start at all. Because with those guards in
  place a panel in that state runs perfectly, reports itself healthy,
  and rejects every login forever — which is precisely the shape of
  failure this project spends its startup checks on.

The unit test that used to build a Server without sessions became the
test for the refusal, and draining-on-cancel moved to the integration
suite, where there is a real database to build a session manager over.
The coverage got better rather than worse: what it proves now is that a
fully wired panel binds, serves a real document with its headers, and
stops when its context is cancelled.

## The link that was missing from the chain

Before this phase there was no way to create the first account. The
technical wizard did not make one, `-dev-link` did not make one, and the
sign-in form cannot sign anybody into an account that was never created.
A finished installation had nobody to hand it to.

That is the shape of gap that survives a long time in a project like
this: every individual piece worked and had tests, and the missing edge
between two of them belonged to neither.

### The invitation is a row, not a user

The obvious implementation is a user with an unusable password and a
flag saying "not claimed yet". It is two pieces of state that have to
agree, and the failure when they disagree is either an account nobody
can sign in to or an account anybody can.

An invitation that has not been accepted is not a user. It is an
invitation, in its own table, holding the address the account will have
and a SHA-256 of the token. Claiming creates the account, grants
ownership of every configured site, and consumes the invitation - **in
one transaction**, because each half is useless without the other:

- a consumed invitation with no account is a customer locked out with no
  second chance,
- an account with no ownership is a customer signed in to a panel that
  shows them nothing,
- and an account created twice is two owners where one was promised.

The consuming `UPDATE` carries `used_at IS NULL` and runs first, so the
race is settled by the database rather than by which request arrived
first. The test fires eight simultaneous redemptions of one token and
requires exactly one account and seven refusals — and then checks that
the winner really does own the site, because that is the half of the
transaction it would be easy to lose without noticing.

Accepting an invitation never produces a superadmin. Owning a site and
running the deployment are different jobs; the second is created
deliberately, from a shell.

### Three things that were already broken

None of these were introduced here. All three surfaced because handover
is the first thing in the system that *acts* on the setup checks rather
than displaying them.

**`cmd/panel` never passed the role names to preflight.** The two
isolation checks - the panel's role must not read analytics, the API's
must not write - reported "we were not told", which is a skip, and a
skipped required check blocks handover. So handover would have been
permanently impossible in production, and only in production: every test
that constructed a Server passed its own config.

The fix is a `[roles]` section rather than making the check lenient. An
unset one still blocks, loudly, because a deployment handed over without
its isolation ever having been verified is exactly the one where nobody
finds out until it matters.

**`Complete()` contradicted its own documentation.** `CheckWarn` is
defined, in the same file, as "something worth knowing that does not
block handover" - and `Complete()` blocked on anything that was not a
pass. Nothing noticed for two phases because nothing consulted it. Then
a log directory at 0755 made an installation unhandoverable over a
permission bit that the check itself calls a warning.

The rule now matches the definitions: a required check blocks when it
**failed or could not run**. Skip still blocks, and that is the
distinction the whole package rests on - "we looked and it is imperfect"
and "we could not look" are different facts, and only the second is a
reason to stop.

**The wizard answered 200 to refused submissions.** Fine in a browser,
a lie to everything else: a test, a script, an access log somebody is
scanning for the moment an installation went wrong. It now answers 400,
which is what the rest of the panel already did.

### A setting whose Kind cannot describe it

`panel.timezone` is `KindString`, and `KindString` means "some text" -
right for a site's name, useless for a zone. "Europe/Istanbul" and
"Avrupa/İstanbul" are both text and only one of them is a timezone, and
a panel that accepts the second and then renders in UTC tells a shop in
Istanbul that its evening peak happened in the afternoon.

Rather than adding a Kind per such value - each bringing a parser the
settings package would then own - `Definition` grew a `Check` function.
It runs after the Kind's own rules, on the canonical form, and returns a
sentence the panel shows. The point of putting it at the *write* is that
the person who typed the value is still there to hear about it.

### The technical door

Both obvious designs are wrong, which is why this needed a decision
rather than an implementation.

Hiding the technical wizard from the customer is wrong because **the
server is theirs**. A technically capable owner who wants to look at
their own retention policy should not have to ask us, and a product that
makes them is one they are right to distrust.

Leaving it plainly linked is also wrong, because the common case is not
a capable owner: it is somebody curious clicking through a menu, and
reconfiguring a working installation is a support call at best.

So it is neither. It is a door with a sentence on it saying the work
behind it is already done, and one confirmation. That costs a technical
owner four seconds and stops the accidental visit entirely.

Two details that are the actual design:

- **The confirmation lives in the session, not on the user row.** The
  warning is about *this visit*. Somebody who looked at the retention
  policy last March should meet the sentence again, because what it
  warns about does not get less true with familiarity.
- **The confirmation is not the authorisation.** It only answers "have
  they been warned". Every request still asks whether this principal
  owns anything - owners and the operator, never an admin, never a
  viewer. A flag in a session is not a role.
- **The developer password is untouched.** Getting into the wizard is
  not the same as being able to change what is in it; the settings with
  legal weight still ask, every time.

An owner who walks straight at the wizard is redirected to the door
rather than refused, because they may go through it. They just have not
been warned yet.

### What the owner's wizard is not allowed to do

The technical wizard verifies more than it configures, because most of
what a deployment needs cannot be set from a browser. The owner's wizard
is the mirror image: it configures and never verifies, because **it must
never require a technical step**. Those were done before the customer
arrived.

Where it needs a technical value it shows what the developer configured
rather than an empty field. The clearest case is the snippet step: the
panel reads only its own config file, so it may genuinely not know where
the beacon is. An unset `beacon_url` produces a step saying where to get
the snippet — not a snippet built from a guessed address, which would
look right and measure nothing.

## Auditing this against the lists, and what the lists found

The instruction was to stop adding features for a moment and go through
the system against the most established checklists — OWASP Top 10
(2021), the CWE Top 25, and the ASVS requirements that apply to a
self-hosted admin panel — and fix whatever they turn up.

The timing is not arbitrary even though the request was. C2, C3 and C4
gave this panel its first surface that the internet can reach **without
credentials**: a sign-in form, an invitation link, a developer link, a
first-run page. Before those, everything behind the front door was
behind a front door that did not exist yet. An audit run a phase earlier
would have found a smaller system and a much smaller share of its risk.

### Order of work: the machine first, then the reading

`govulncheck` ran before anything was read, because it is the only part
of this that a person cannot do better. It took under a minute and
produced the two highest-severity findings in the whole audit:

- **`pgx/v5` 5.7.6 — SQL injection through placeholder confusion**
  ([GO-2026-5004](https://pkg.go.dev/vuln/GO-2026-5004)). A `$1` inside
  a dollar-quoted string literal could be treated as a placeholder,
  which turns a correctly parameterised query into an injectable one.
  This is the project's single loudest rule — every query is
  parameterised, and §3.1 of the plan says so — defeated one layer below
  where the rule is enforced. `govulncheck` reported it as *reachable*,
  through `api.Store.BeaconSites`.
- **`golang.org/x/text` 0.24.0 — infinite loop on invalid input**
  ([GO-2026-5970](https://pkg.go.dev/vuln/GO-2026-5970)), reachable from
  `ui.Formatter.Title`, which the panel calls on names people type into
  the account page. One request that never returns, in a process with no
  concurrency limit.

Neither would have been found by reading. Both were fixed by an upgrade
that also moved the module to Go 1.25. That is worth stating plainly:
**the two worst things in this audit were in dependencies, not in code
anybody here wrote**, and the defence against them is not care — it is
running the tool on a schedule.

### The hole that reading found, and the measurement that proved it

Go's `ParseForm` reads an entire urlencoded body into memory with no cap
of its own. Only *multipart* bodies get one, through
`ParseMultipartForm` and its explicit size argument. So every `POST` in
this panel was an unbounded allocation, reachable by anybody, before
authentication.

That claim was measured rather than asserted: a 64 MiB body cost about
128 MiB of heap — and was *then* refused for a missing CSRF token,
having already been paid for in full. A handful of concurrent ones takes
the process out.

This is the shape of hole that survives review, because nothing in the
handler looks wrong. The missing thing is a line nobody wrote.

The cap is middleware (`LimitRequestBodies`), outermost of the three
wrappers, deliberately not per-handler. "Every handler remembers" is
precisely the property that fails here: the eight `ParseForm` calls in
the package were written across three phases by somebody thinking about
something else each time. `MaxBytesReader` also closes the connection
once the limit is hit, so a client streaming an endless body is
disconnected rather than politely read to the end.

**The test measured itself first.** The first version built its body
with `strings.Repeat` and reported 34 MB of "growth" — which was the
test allocating the body, not the server accepting it. It now streams
from an endless reader that allocates nothing, and the assertion changed
shape too: not "stays under N bytes", which is a number that rots the
first time a template grows, but **"the cost stops scaling with the size
of the body"**. That is the actual property, and it is the one that
still means something in two years.

### An error message is an output, and outputs have audiences

`Store.SetSetting` returns two entirely different kinds of error through
one return value. "This must be between 1 and 3650" is written for the
person who just typed a number, and rendering it into the page is the
whole point of validating. A wrapped pgx failure is written for whoever
reads the log, and it carries constraint names, SQL state and sometimes
the query text.

The panel rendered both, because the call site could not tell them
apart. One bad write from showing a customer the schema — CWE-209.

The fix is a sentinel (`ErrInvalidSetting`) rather than a convention,
and that distinction is the whole decision. A convention means every
future caller correctly guessing which errors were written for a person;
a sentinel means the caller asks and the answer is a fact. `errors.Is`
is now the difference between showing a message and summarising it away.
The test asserts both directions: every validation refusal is marked
safe, and a wrapped database error is not — the second half being the
one that would silently regress.

### A blanket middleware was the wrong shape, and the 503 page said so

Four routes the internet reaches without credentials read a row before
they know who is asking. Each was a nil-`Store` dereference away from a
remote crash — CWE-476. `ListenAndServe` already refuses to start
without a database, so this state should not exist; it is guarded anyway
because "unreachable" and "takes the process down" are a bad pair of
properties to hold simultaneously, and the thing making it unreachable
is one line in another file.

The first attempt was a `RequireStore` middleware wrapped around the
whole tree. It broke eleven renderer tests, and the reason it broke them
is the reason it was wrong: **a panel that cannot serve its own
stylesheet renders its 503 page as unstyled text.** Static assets do not
need a database, and answering 503 to them makes the page explaining the
outage worse than the outage. The guard moved to `haveStore()`, called
at the first place each handler actually needs a row.

### Two tests that passed for the wrong reason

Both were caught by asking what the assertion would say if the code were
broken.

**"413, not 419."** `CheckCSRF` reads a form field, so it was the thing
that first touched the body — and an oversized body surfaced as "your
CSRF token is stale", sending whoever hit it to reload a page that would
fail again identically. Hence `acceptPost`: parse the form, *then* check
the token. It is also the honest order on its own terms, since a token
cannot be checked in a form that has not been parsed.

**"303 means it happened."** The CSRF coverage test walks every
state-changing route with no token and requires a refusal. It counted
any 303 as success — but a redirect to the sign-in form *is* a refusal,
and a redirect anywhere else is the success path. It now accepts 303
only when `Location` starts with the sign-in path. It also moved to the
integration suite: a storeless server answers 503 to everything, so the
same test in the unit suite would have passed without a single handler
being reached. A test that cannot fail is worse than no test, because it
is also a claim.

### The fix that half-landed, found by writing it down

Documenting the audit is what caught the last one, and it is the most
embarrassing item here because it is a defect *in the audit's own fix*.

The whole point of reordering `acceptPost` was to report a size problem
as a size problem instead of as a stale CSRF token. The status line duly
became 413. The **page** did not: the renderer maps a status to a pair
of catalog keys and falls back to the 500 wording for anything it does
not know, and 413 was not in that switch. So the panel answered `413
Request Entity Too Large` with a page reading "the panel could not
complete this request — the failure was recorded on the server."

Status honest, page lying, every test green. The fallback is right *as
a fallback* — a status reaching a browser with no words at all is worse
— but it is silent, and silence was the whole problem.

Fixed with words for 413 in both catalogs, and with the test that would
have caught it: the statuses are **read out of the handlers** rather
than listed in the test, each one rendered, and each one required not to
come back wearing the 500 page. A list would have been a mirror, and a
mirror's failure mode is exactly this one — somebody adds a status and
does not know there is a second place to add it. The test was then run
against the un-fixed renderer to confirm it fails, and it names the file
and both places to change.

### The comment that was doing a compiler's job

One SQL identifier in the read API is interpolated, because Postgres has
no placeholder for a column name. The guard was a comment saying "only
pass constants". That is not latent because the callers are wrong today
— they are right — but because nothing stops the next one.

It is now a closed type with unexported values and a `default:` that
refuses anything else. A request-derived string cannot reach it, and a
new column that skips the check does not compile. Same class as the
closed-enum types elsewhere in this project, and the same reason: the
rule belongs in the type, not in a sentence next to it.

### Writing down what was checked and found correct

`SECURITY.md` lists thirteen things that needed no change — SQL
injection, XSS, access control, open redirect, SSRF, CSRF, session
management, timing, log injection, path traversal, header injection,
supply chain, resource limits.

That list is more useful than the fixes. "We found nothing" and "we did
not look" are different facts, and only one of them should reassure
anybody — the same distinction the preflight checks are built on, applied
to the audit itself. Next time, the question is what changed since, not
where to start.

Three things are open and stated in the file rather than smoothed over:
changing a password does not end sessions on other devices (the session
store has no user column, so finding them is a table scan today); there
are no 2FA recovery codes; and there is no global concurrency limit, so
each sign-in attempt costs one argon2id verification bounded by the
throttle counters rather than by a queue. Each is also said on the page
it affects, which is the part that matters — a limitation the software
knows about and the customer does not is worse than the limitation.

## The person who was supposed to be asked

C2 built a rule and no way to obey it. A developer link is inert once a
deployment has an owner, and stays inert until that owner approves it —
which was true, tested, and unreachable. The request sat in a table with
no page, and the only way through it was a SQL client. A consent
mechanism nobody can give consent through is not a consent mechanism; it
is a deployment that quietly cannot be worked on.

So this phase is mostly a page. The three things worth writing down are
what that page had to decide.

### A developer must not approve developer access

A redeemed link produces a principal with `Superadmin` set, because a
developer has to reach every site to do the work. `ownsAnySite`
therefore answers **yes** for them — correctly, for the technical
wizard, which is their own tool. On this page it would be a hole the
whole mechanism fits through: an approved developer approves the next
request, and the next, and the owner is asked exactly once, ever.

So the guard is not `ownsAnySite`. It is "a signed-in **person** who
owns something", with the kind checked first and no ownership question
asked at all until it has passed.

**The test was run against the guard with that check removed**, which is
the only way to find out what it proves, and the answer needed the
comment rewritten. The developer is still refused without it — the next
line loads a `User` by an id a developer does not have, and the load
fails. But they are refused with a **500**, by an accident nobody
designed. The Kind check does not create the refusal; it makes the
refusal deliberate and gives it the right status. A rule whose only
enforcement is an unrelated lookup failing is a rule that ends the day
somebody fixes that lookup.

### There is no "who", and the page says so

The plan said this screen shows the owner *who* asked. It cannot. A
request is minted by somebody with a shell on the server, and the reason
attached to it is a sentence that person typed. The panel verified
nothing.

Adding a `requested_by` column would have made the page look like it
answered the question while changing nothing about what is known — a
self-asserted string from the same shell, presented as an identity. So
the first thing on the page, above the first request, is the sentence
saying the panel cannot verify who asked and that the reason is a claim.
Before the reason, not after: somebody deciding whether to let a
stranger into their customers' data should learn how much was checked
*before* they form an impression of the text.

That sentence is asserted by the integration test and by the browser
test, because it is the part of this page most likely to be dropped by
somebody tidying up.

### An install-time grant is dead here, whatever the row says

A bootstrap grant carries `approved_at`, because during installation
there was nobody to ask. Redemption refuses it the instant an account
exists. This page cannot be reached without somebody being signed in —
so an account always exists — which means **every** `auto_approved` row
on it is already spent, and drawing one as "approved" would tell the
owner somebody can still walk in.

It gets its own state and its own words: *install-time grant, no longer
valid*. It is also credited to nobody, explicitly, even though nothing
writes an approver on those rows today: the column exists, and a page
saying "approved by X" about a grant nobody consented to is a lie with a
name attached.

The unit test walks all nine ways a row can arrive and requires every
state to be produced by some case, so a state nobody exercised cannot
sit there being drawn wrongly.

### Asking the same question twice

The banner and the navigation both need "does this reader decide
developer access", and the first version asked twice — once in the nav,
once in the banner — which is two identical membership queries on every
page in the panel.

Worse, the first version had them in *different orders*, and one of the
comments claimed a property the other broke. It is resolved once in the
shared chrome and handed to both. The count of pending requests then
runs only for somebody already found entitled, and stays a count: the
banner needs a number, and the page that needs the rows is one click
away.

### Four audit actions that were defined and never written

`dev_access.requested`, `.approved`, `.denied` and `.rejected` had
existed since the audit constants were written, with a comment
describing the three-step story they tell. Nothing wrote any of them.
The log began at "somebody redeemed a link", which reads as though the
panel invented the request.

They are written in the store, beside the rule that decides them, for
the same reason redemption already was: a future caller — a command-line
approve, a support tool — would otherwise have to remember. A decision
that lost the race writes nothing, because a log recording decisions the
database refused cannot be trusted about the ones it accepted.

**`.rejected` needed a limit that the others did not.** A link presented
after it was denied, or after it was already used, is the single most
interesting event in this mechanism — and the redemption URL is public.
Filing an entry for every string presented would let a stranger write
rows into an append-only table, at the speed of their connection, in a
table this panel is deliberately not allowed to `DELETE` from. So an
entry is written only when the token **matches a real row**: a token
matching nothing is somebody guessing base64 of 32 random bytes and
teaches nobody anything, while a token matching a row is a fact about a
link this deployment actually issued. The test asserts both halves,
including that a made-up token writes nothing.

### One bound that only appeared once somebody read the text out loud

The reason column is `TEXT`, and nothing capped it, because until this
phase nothing rendered it to a customer. It is now bounded — refused
rather than truncated, since a sentence cut off mid-word is one an owner
might decide differently on, and the person who typed it is at a shell
and can retype it. Counted in runes rather than bytes, because a byte
limit would cut a shorter sentence than it promises, and would do it
only for the languages this panel was written for.

The stylesheet carries the other half: the reason is the one string on
that page that arrived from outside, bounded in length and not in shape,
so a single legal 500-character word must wrap rather than push the page
sideways. That is not something the browser test can see — no policy
violation, no console error, just a page that looks wrong — which is
worth remembering about what browser tests do and do not cover.

## Moving a setting out of a file without losing what the file said

A5 is the plan's most critical item because thirty-nine repair
operations assume "a setting you can change while running", and until
now most of them could not be. This phase does the mechanism and the two
families the repair catalogue's own evidence puts first — not a guess
about what would be convenient, but the catalogue's words: *"an empty
list behind Cloudflare shows every visitor as the same address and makes
every other number in the system wrong at the same time."*

### Reconciling two rules that looked contradictory

A5 says a migrated value is "ignored from the file thereafter". A6 says
a failed read must keep the last known values, never fall back to
defaults, because silently resetting a customer's tuning is worse than a
stale value and far harder to notice.

Take both literally and they conflict: if the file is ignored, a process
that starts while the database is unreachable has only the built-in
defaults — exactly the silent reset A6 forbids.

They reconcile once you see they describe different things. **In code
the file is always the fallback**: stored row, else file, else built-in
default, each layer narrower than the last. **The migration writes the
row once**, and from then on the row wins — so editing the file changes
nothing, which is what "ignored" means in practice. Nothing is deleted;
a migration that edits somebody's config file is a migration that can
corrupt it.

The part that needs saying out loud, and that the command prints every
time it runs, is the consequence: *the file is still the fallback, it
has simply stopped being where the value is changed*. Without that
sentence somebody edits the file, restarts, sees no change, and
concludes the software is broken — the same lesson A7.6 taught from the
other direction.

### The migration is a shell command, and that is not laziness

The collector's database role may only `SELECT` on `panel_settings`.
Widening it so the service could migrate its own settings would hand a
compromised collector the power to change the retention period and the
IP storage mode — the two settings sitting behind the developer password
precisely because they carry legal weight.

So it runs as the panel, from a shell, once: the same shape as applying
the schema, minting a developer link, minting an owner invitation. Work
that needs authority the service does not have is work a person does at
a prompt.

It reads the TOML generically rather than through each service's config
loader. That keeps the panel's binary from linking the beacon's HTTP
server, and it keeps the command honest about its own scope: it reads
the keys it knows how to move, it does not validate somebody's whole
deployment. Validation is the registry's job and every value goes
through it.

Three rules, and the first is the one that matters: **an existing row is
never overwritten**. Undoing a value somebody set in the panel, using a
line in a file they had forgotten about, would be invisible — the panel
would go on presenting the setting as theirs while showing a number
nobody chose.

### One setting cannot mean two things

The first draft made `limits.*` a single family read by both services.
That would have been a number that cannot mean what it says: the
collector sees every connection to the site, the beacon sees only the
visitors whose browser ran the snippet. A ceiling right for one is wrong
for the other by an order of magnitude, and one number covering both is
wrong somewhere no matter what it is set to.

They are now `collector.limits.*` and `beacon.limits.*` — the prefix
convention `beacon.sites` already established, where the name says which
process reads it. Eight registry entries generated from one function
rather than written twice, because the difference between the two
families is meant to be *which process reads them* and nothing else.

### Three things this phase found that were already broken

**`Definition.Check` was dead for four Kinds out of five.** Its own
documentation says it "runs after the Kind's own checks, on the
canonical form". It was called inside the `KindString` branch of the
switch and nowhere else, so a validator attached to a list, an int, a
bool or an enum was never called at all.

It surfaced the worst possible way — a test expecting a malformed
network to be refused, watching it be stored — which is what a dead
validator always looks like: not an error, an *acceptance*. `Check` now
runs once after the switch for every Kind, and a test walks all five
rather than the ones a setting happens to use today.

**A test cleanup that silently did nothing.** The live-settings tests
opened a pool with `defer pool.Close()` and registered row deletions
with `t.Cleanup`. Deferred calls run when the function returns;
`t.Cleanup` functions run *after* that. So the close happened first,
every delete ran against a closed pool, and the error was discarded —
four rows survived the suite, and the next test read one of them and
failed claiming the two services shared a setting. A cleanup that
quietly does nothing is worse than no cleanup, because the suite goes on
looking tidy. The pool's close is a cleanup now too, so ordering is LIFO
and the deletes run first.

**A test that asserted something about the rest of the suite.** The
migration's audit check walked every `setting.migrated` entry and
required each to name *this* test's file. Leftovers from an interrupted
run made it fail for a reason that had nothing to do with the code. It
now looks only at entries from its own file — and `clearMigrated` clears
before as well as after, because cleaning only on the way out makes a
suite that passes or fails depending on how the previous one ended.

### What the limiter needed, and what it deliberately did not get

`limiter.Config` was a plain struct read field by field on every
decision. It is now behind an atomic pointer, loaded **once per
`Admit`** and passed down. Reading the pointer again further down would
let a config change land between two checks and produce a decision made
half under the old limits and half under the new — a state nobody
configured and nobody could reproduce afterwards.

A caller already queued under `throttle` keeps the config it started
under. That is deliberate rather than an oversight: finishing under a
different set of rules is how a queued request gets rejected by a limit
that did not exist when it started waiting.

The collector also gained a live settings reader here, which it had
never had. That closes the standing risk in the plan — that the two
tables this system writes were configured from two different places —
and it means A5.2's remaining keys are wiring rather than architecture.

## The page the product exists for

A measurement changed the order of the plan. Before this phase: 26 000
lines of non-test Go, 742 test functions, coverage between 67% and 100%
— and the panel made **not one call** to the analytics API. A customer
could install it, sign in, see a list of their sites, and stop. The next
item in the plan was A5.2, which adds keys to a mechanism that already
works; this is the page the whole thing is for. The order changed.

### The panel reads its own numbers over HTTP

The panel's database role has no access to the analytics tables, on
purpose: it is the process the customer's browser talks to, and giving
it read rights on every visitor record would make the widest-reachable
component also the one with the broadest database access.

So this phase is the first real test of that rule, and it holds — the
dashboard calls the read-only API exactly as an external tool would.
A structural test now refuses any mention of `traffic_snapshots`,
`beacon_events` or `internal/api` in the panel's HTTP tree, because a
handler that reached for a pool directly would compile, work in
development against a superuser, and fail in production. That is the
worst order to discover a thing in.

**The token's blast radius is stated rather than assumed.** One token,
granted every site, because the panel serves every site. What keeps one
customer's numbers away from another is *entirely* the panel's own
access check. So that check gets the paired test this project gives
every permission: the owner's request, and the same one from an account
with no membership — which gets 404 rather than 403, because a 403 would
confirm the site exists and turn the URL into a way to enumerate a
deployment's customers.

### Three kinds of nothing

Zero pageviews means three completely different things, and they look
identical in a summary:

- the snippet was never embedded — a setup step nobody performed,
- it is embedded and nobody visited in this period — a measurement,
- the API did not answer — neither.

Collapsing them into "no data" would present an unfinished installation
as a result. That is §D5's "we stopped collecting" versus "we never
collected", one level down, and it is why the client has a
`KnownSites` call at all: it asks whether a source has *ever* written
for this site, and it asks **only when a summary comes back empty**. In
the ordinary case the answer would change nothing on the page, and a
dashboard should not pay for a distinction it is not drawing.

The fourth state exists too — the API answered and refused the token —
because "wait" and "somebody has to fix the configuration" send a reader
to different places. And one rule sits above all of them: **a failure is
never read as zero.** A card saying "0 visitors" because a call timed
out is not a missing number, it is a wrong one, and the customer has no
way to tell.

### A range is whole days in the panel's timezone

§6 recorded the reason long before there was a page to apply it to:
sessions are counted inside the range, so one that began before it is
truncated at the boundary. A range starting at 14:37 cuts every session
running at 14:37 — in the period shown *and* in the one before it — so
neighbouring periods cannot be added together and neither matches what
a customer means by "last week".

Local rather than UTC is the other half, and it is what makes the
timezone setting more than cosmetic: computing "today" in UTC hands a
shop in Istanbul three hours of yesterday and loses three hours of
today, every day, invisibly.

The test that pins this uses a mid-afternoon instant on purpose — the
bug is invisible at midnight — and a second one walks a
daylight-saving week, where the right answer is that the boundaries are
still local midnights and the week is 167 hours rather than 168.

### The card set is data, because C6 is coming

C6 makes which cards appear a per-deployment setting. A page with six
cards written into the template is a page C6 would have to take apart
first, so the registry is written now, in the shape the settings
registry already uses: a closed set, adding to which is a code change
that goes through review. C6 becomes a selection rather than a rewrite.

The default six are four a customer recognises from any analytics tool
and two that are the reason this product exists — the collector counts
the visitors no JavaScript-based tool can see, and separates the
automated ones from the people. A test requires both sources to be
represented, because a default view drawn only from the beacon would
look like every other tool and hide the thing that is different.

### Three tests that were wrong before they were right

**A test that panicked on its own input.** The range parser is fed
deliberately hostile values, one of which contained a space —
`httptest.NewRequest` parses its argument as a request line, so the
*test* died on a malformed HTTP version. It says nothing about the
handler. The values are percent-encoded now, which is how a browser
would send them anyway.

**A race the concurrency test was asserting.** The client fetches both
summaries at once; a second test captured request details into plain
variables from the handler, which now runs twice concurrently. `-race`
caught it — the property one test asserts arriving as a failure in
another.

**Comparing rendered HTML against raw catalog text.** The Turkish for
"the snippet is missing" contains `snippet'in`, and `html/template`
escapes the apostrophe. The assertion failed while the page was
perfectly correct. Every catalog comparison in that suite now goes
through the same escaping the template does.

### What a browser added that nothing else could

Six cards, four columns at desktop width, one at phone width, nothing
overflowing, no horizontal scroll, no policy violations — and the range
picker checked as a reader meets it: three links plus one marked
current, and clicking one actually changes the dates underneath. None of
that is visible from a response recorder, where the bytes are correct
and the page could still be unusable.

## Writing the installation down, and what writing it found

`KURULUM.md` is the fixed installation guide: build, database, secrets,
config, services, TLS, first run, handover, verification, recommendations,
common mistakes, known gaps. It is in Turkish because the person it is
written for reads Turkish, and every command in it was run before it was
written down.

That last rule is the whole point of the exercise. A document assembled
from memory is a set of plausible commands; a document assembled from a
terminal is a set of commands. The difference showed up four times, and
each time the document was wrong rather than the code.

### The example config pointed at a database that would break handover

`panel.example.toml` shipped `postgres://panel@localhost/crucible` —
its own role, its own database. Reasonable, and wrong, and the reason is
not obvious from either file.

The setup wizard's isolation check asks Postgres, over the panel's own
connection, whether the *other* roles can reach `traffic_snapshots` and
`beacon_events`. From a different database those tables do not exist to
ask about. The query does not return "no", it errors — and an error is
`CheckSkip`, "we could not look", which is one of the two states that
blocks handover on purpose. So a panel installed exactly as its own
example file described could never be handed to a customer, and the
check that stopped it would be reporting the truth.

Measured rather than reasoned: the same `has_table_privilege` call
returns `t` in `analytics` and `ERROR: relation "traffic_snapshots" does
not exist` one database over. The example now says the database must be
shared and the role must not, with the reason attached, because a
constraint whose reason is missing gets optimised away by the next
person.

### A GRANT block that quietly widened the thing it was demonstrating

The first draft ended with the line everyone writes:

    GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO collector, beacon_writer;

There are six sequences in this database and all six belong to the
panel. `traffic_snapshots` and `beacon_events` have no `BIGSERIAL` at
all, so that line grants the two analytics roles rights over the panel's
identity columns in exchange for nothing. In a document whose §4.5 is a
set of queries proving the roles cannot reach each other, that is worse
than a redundant line — it is the document contradicting itself four
lines apart.

Named sequences now, panel role only, for the same reason `ALL` was
wrong in the first place: it is a grant to whatever exists later, not to
what exists now.

The fix was verified the way the rest of the project verifies things —
the entire §4.2 and §4.4 block was run against the real TimescaleDB with
prefixed role names, the resulting privilege matrix printed for all four
roles across five tables, and the roles dropped. The matrix is the
document's claims, one row each: the panel reads no analytics table, the
API can read both and write neither, the beacon can insert into its own
table and not read it back, the audit log takes `SELECT` and `INSERT`
and refuses `UPDATE` and `DELETE`, and both collectors can read
`panel_settings` and nothing else of the panel's.

### A check that exists, is tested, and never runs

Writing "the wizard runs 14 checks" meant counting them, and counting
them found a fifteenth that cannot happen. `preflight.checkService` asks
whether the collector, beacon and API are actually up. It is written, it
has tests, and `cmd/panel` never passes it a single address — there is
no field in `panel.toml` that would carry one. `ServiceURLs` appears in
three test files and nowhere else.

So the wizard's answer to "is the software running" is silence, and
silence here reads as approval. The document says so in §9.3 and §16
and sends the reader to the manual checks instead; `PLAN.md` carries it
as open work. Wiring it up is small, but it widens the installer's
check surface, and this was a documentation phase — the honest move was
to write down that the gap exists rather than to quietly close it while
nobody was looking at that file.

### Two claims in the README that had stopped being true

The intro still said there is no dashboard UI, one commit after the
dashboard shipped, and "Requires Go 1.23+" against a `go.mod` that says
`1.25.0` — where an old toolchain does not fail at some later feature,
it refuses the module outright. The first is a stale sentence; the
second is a developer's first command failing for a reason the document
caused. Both fixed, along with the scope list that still filed a
dashboard and human login under "not in this project" when both are now
in it, in a process the API section correctly describes as a separate
one.

### Versions: tested, not claimed

The prerequisites table originally said PostgreSQL 14+ and TimescaleDB
2.x. Nobody has ever run this against PostgreSQL 14. What is actually
known is what `docker-compose.yml` pins and the integration suite runs
against every time: 16.6 and 2.17.2. The table says that now, and says
older is untested rather than unsupported — the distinction the whole
preflight package is built on, applied to the document describing it.

## Why a number is what it is

D1 put six figures on a page and nothing behind them. A customer could
read "1 240 pageviews" and had no way to ask which pages. This phase is
that question, six times: pages, sources, campaigns, devices, countries,
events - each a section under the cards, each with its full paginated
list one click away.

The API serves close to thirty breakdowns and six is a choice, not a
limit. Fingerprints, ASNs, score distributions and the cross-source
views are deliberately absent: they belong to the developer layer, which
adds columns to these sections rather than pages beside them, and a
default view that opened with a JA4 hash would be a different product.

### One registry, split across two packages

The kind is a closed set and it lives in the analytics package, not the
web one, because what it encodes is a transport fact: which path to
call, which key the rows arrive under, which of the API's row shapes
they are. What a breakdown is *called* belongs to the panel.

That split is not tidiness. A breakdown's identity becomes a path
segment in a request to another service, so the registry lookup is what
stops a URL somebody typed from reaching the API as an endpoint name -
and it happens in the handler before the site is looked up, let alone
fetched. A test walks the unknown segments, including a percent-encoded
traversal, and requires each to be a 404 rather than an attempt.

### Four row shapes, one table, and a column header that is not a lie

Pages and referrers and devices and countries answer with one shape,
campaigns with another, events with a third. Flattening them into one
Row is a rendering decision, and the field is called Count rather than
Pageviews for a reason: it is pageviews for pages and occurrences for
events, and naming it for one of its meanings makes the other wrong at
the point somebody reads the struct. Both column headings come from the
breakdown's own catalog entry.

The same distinction decides the denominator. A share is a row over the
summary, and events divide by the event total while everything else
divides by pageviews - dividing an event count by the pageview total
produces a percentage that looks entirely plausible and means nothing.

### The share and the filter that has to match

The API's `bots` parameter defaults to exclude on the beacon summary and
on every breakdown alike. The panel sends it on neither, so both take
that default and a row's percentage counts the same people the card
above it counts.

Send it on one and not the other and the page still draws perfectly:
every number formatted, every column aligned, and the percentages
quietly failing to add to a hundred. Nothing on screen would say why.
That is why it is a test rather than a comment, and why the integration
test asserts the property - the shares present sum to about a hundred -
rather than a hard-coded percentage that would pass for the wrong reason
the day the fixture changed.

A missing total renders as a dash. The formatter already had that rule
for cards; the same rule applies here for the same reason a failure is
never drawn as zero.

### The group with no value

The API flags the group where a value was never determined - a visit
with no referrer, an unrecognised browser, an unresolved country -
instead of dropping it, so the groups still add up to the site's total.
Two ways to lose that on the last hop: draw it with a blank label, or
leave it out. The first produces a row nobody can read and the second
makes the column quietly short.

So it gets a name, and its own name per breakdown, because "Direct" is
not the same fact as "Unknown" and a shared word would be wrong for
both. It is styled as the different kind of thing it is: a name this
panel gave the row, not a value anything measured.

The fixture is what proves it. Three of the nine seeded pageviews arrive
with no referrer at all, on purpose - a fixture with every column filled
in would never produce the row this phase is most likely to get wrong.

### The breakdown that does not add up, and the row that cannot exist

Writing that per-breakdown word found a real mistake in my own code.

`BeaconCampaigns` groups by the stored campaign query and its SQL
excludes the empty one, so untagged traffic is not a flagged group - it
is not returned at all. The client was deriving an "empty" flag from an
empty key, and both catalogs carried a word ("Kampanyasız" / "No
campaign") for a row that can never render. The registry now records
which breakdowns have such a group, the derivation is gone, and the
catalog strings with it; a test asserts both directions, so a word for a
row that cannot appear fails as loudly as a missing one.

The consequence belongs on the page rather than in a comment: unlike
every other breakdown, campaign rows do not sum to the site's total, so
a campaigns table showing 2 of 11 pageviews is correct and looks broken.
The section's help text says so.

That whole thread started because the integration test failed on a
fixture that set `utm_source` and not `query` - which would have seeded
a campaigns table that was correctly empty and proved nothing.

### Eight calls, measured rather than assumed

D1's page made two calls; this one makes eight. One `FetchSite` rather
than a summary round followed by a breakdown round, because two rounds
would bound the page at twice `PageTimeout` while reading, in the code,
as though it were bounded by one.

Against a real API over a real TimescaleDB:

	summaries alone (the D1 shape)    4.1 ms
	+ pages                           7.3 ms
	+ countries                       6.1 ms
	+ referrers, campaigns,
	  devices, events                 4.1 ms - free
	all eight together               10.4 ms

Four of the six cost nothing measurable because they finish inside the
summary calls they run alongside. The page ends up costing its slowest
query rather than the sum of eight. "It is concurrent, so it is fast" is
a claim; this is the number, and it is in the test's doc comment so the
next person does not have to take it on faith.

### What the browser found, and what it did not find the first time

Real Chromium: six sections all carrying rows, three named groups, no
overflow and no sideways scroll at 1280px or at 390px, no CSP
violations, the full-list link landing on the right page and the way
back landing on the summary.

Long paths are seeded deliberately. A fixture of "/" and "/fiyat" passes
a layout that breaks on the first real site, so the browser test seeds
paths of a hundred-odd characters and asserts the section does not grow
wider than its box - the tables scroll inside their own container, the
page does not.

The first run reported `detail_has_pager: false`. Fourteen distinct
paths against a page size of twenty-five: the pager was asserted about
and never clicked, which is the same as not testing it. Seeding past
`detailRows` rather than past `sectionRows` fixed it, and the second run
followed a real next-page link to `?gun=7&sayfa=2` - which also proves
the period survives the click, the thing a pager most often drops.

### gofmt rewrites quotes in doc comments

A stray `”` kept reappearing in a test file after every format. Not an
encoding fault and not the editor: gofmt normalises a doubled apostrophe
to a right quotation mark inside doc comments, so `WHERE query <> ''`
became `WHERE query <> ”` every time. The comment is reworded to avoid
the sequence rather than fought.

Worth writing down because the first two explanations - mojibake, a
broken heredoc - were both wrong and both plausible, and one identical
character has been sitting in `internal/beacon/useragent.go` since that
file was written for exactly the same reason.

## Not showing somebody twelve things they cannot read

The person who buys a website does not know what a TLS fingerprint is
and has no reason to. D1 gave every customer the same six cards and D2
added six sections underneath, so the page reached twelve blocks that
nobody had chosen — and for most customers several of them are numbers
they cannot interpret, which is worse than no number at all, because it
invites a wrong conclusion instead of none.

So the visible set is a per-site setting, written in the wizard by the
installer sitting with the customer, changeable afterwards. The plan
recorded this as C6 in the user's own words; what D2 changed is the
size of the gap, since C6 was written when there were six blocks rather
than twelve.

### The saving has to reach the database

Hiding a block in the template would leave its query running: Postgres
still does the group-by, the API still serialises it, and the only thing
saved is some HTML. So the request is built from the visible set, not
from the registry, and the test asserts the *request* — a count of paths
the API was asked for, not a stopwatch. A timing assertion here would be
both flakier and weaker.

The measurement from D2 says what that is worth: the pages breakdown
costs 3.3 ms, countries 2.0 ms, and the other four are free because they
finish inside the summary calls. A customer who wanted two cards and one
table stops paying for five queries per page view, for ever.

Two summaries fell out of the same rule. A page with no beacon card and
no breakdown does not fetch the beacon summary at all — which is the
collector-only deployment, the one where that call could never have
shown anything. `KnownSites` got the same treatment: it used to ask both
site lists whenever either source came back empty, and now asks only for
the one somebody is going to read.

### Unset means the default; "none" had to be spelled out

An empty list is what every deployment that predates this setting
already has, so unset has to mean the full page. A panel that emptied
itself on upgrade would be the worst possible reading of "not
configured".

The first draft stopped there and called the missing case a stated
limit: you can select a subset, not nothing. A live run showed why that
was wrong. A deployment running only the collector cannot turn the
beacon sections off, so its customer gets six tables that all say the
snippet was never installed — six blocks of noise, on the page whose
entire purpose is not confusing somebody who does not know what a
snippet is.

Unset and set-to-empty are the same value on disk, so the two states had
to be made different. `ViewNone` is a word a form can send and a database
can keep, unlike an absence. In the wizard it is what unticking
everything means, which makes the interface read the way a person would
expect while leaving unset alone.

The consequence, handled rather than argued away: a site with both sets
cleared has an empty page, and it says so in a sentence that points at
the setting instead of rendering a heading over blank space.

### Ids out of a database are ids from a version that may not exist

The stored values become catalog keys and API path segments on every
later page load. A card removed from a build, or a row written by a
version that had one this one does not, has to be dropped and logged
rather than rendered — and a set where *every* id is unknown still draws
the default rather than nothing, because a blank page is a worse answer
than a stale one.

The write path checks against the same registries, so a posted id that
is not offered by the form never reaches storage. That check lives in
the web package rather than in the settings registry, and the reason is
worth writing down: the closed sets live in the packages that own them,
and copying them into `internal/panel` to validate against would create
a second source of truth for exactly the thing a closed set exists to
prevent.

### Three failures, all mine, all found by running it

**A test that left a setting behind.** The C6 end-to-end tests narrowed
the visible set on the site the D2 tests use, and never put it back. The
suite shares one database and the row outlives the process, so the D2
sections test began failing on a *later* `go test` run with four
sections missing. Every C6 test now restores the unset state; writing
the empty list back is a genuine restore rather than a different
configuration.

**A test that asserted on a word.** Checking that the visitors card was
gone by looking for "Ziyaretçi" failed on a perfectly correct page: that
is the visitors card and also the visitors column every breakdown table
carries. The assertions read the block's own data attribute now.

**A browser test that clicked Çıkış.** `form button[type=submit]` takes
the first match in the document, and the chrome's sign-out is a form
with a submit button that comes before the step's own. The browser
signed itself out, landed on the sign-in page, and reported that the
step had saved nothing — which was true, and had nothing to do with the
step. The click is scoped to `main form` now.

That last one only became diagnosable after the script was wrapped so a
failed locator reports the page it was looking at rather than a stack
trace naming the selector. The selector was never the interesting part.

## The settings that stop an attack

A5.2 made four settings changeable on a running collector — the blocked
country list, the blocked ASN list, the known-bot ASN list, and the flag
that turns the last one into a scoring signal. The choice of *which four*
was not "the easy ones": it was what a support call actually reaches for.
"We are being hit from there, block it" meant SSH, an edit and a
restart — the longest possible path while an attack is in progress.

### Keeping an optimisation while making it changeable

`NewGeoBlocklist` used to return **nil** when both lists were empty, and
the servers read that nil to skip resolving each connection's country and
ASN entirely. That is a real saving for the common deployment, which
blocks nothing, and losing it would mean paying for a geography lookup
per connection to support a feature nobody had switched on.

A list that can be filled in later cannot answer "is there anything
here?" once at startup, so the question moved to `Active()` — one atomic
load, the same answer, asked per connection. The rules themselves sit
behind `atomic.Pointer[geoRules]` and are never mutated after `Set`
builds them, so a connection that has read the pointer keeps a consistent
view for the whole of its check even if another list arrives mid-decision.

### A live setting that was never applied

`applySettings` compared the new known-bot ASN list against the old one
and applied it only on a change — and compared them **by length**, on the
reasoning that diffing a map every poll was not worth a log line. That
reasoning was true about the log line and wrong about everything else,
because the same comparison gated the apply.

Replacing one ASN with another keeps the length. It is also the single
most likely edit a support call produces: *it is not that network, it is
this one*. The new list would have sat in the database, shown in the
panel as the current value, and never reached scoring.

The fix is not a better comparison. Every setting is now applied on every
poll, and compared only to decide whether to log it — `SetConfig`, `Set`
and `SetKnownBotASNs` are each a single atomic store over freshly built
values, so re-applying an unchanged value costs one store and is not
observable to traffic in flight. A comparison that can only ever be wrong
about whether to log is a comparison that cannot break a security
setting.

### A load test that passed without proving anything

The phase's own done-criterion asked for the change to be measured under
real concurrent load. The first version fired three separate bursts with
the blocklist change made between them, and passed: the burst racing the
change reported zero connections served.

Zero is what a working blocklist produces. It is also what a goroutine
scheduled entirely after `Set` returned produces, with nothing in flight
during the change at all. A test that passes the same way whether or not
the interesting thing happened is not evidence, so it was rewritten to
keep a continuous stream of connections running across both changes and
to assert that each phase carried traffic *before* asserting what
happened to it.

The rewritten test immediately reported 3 of 9803 connections served
while blocked — and that was the test being wrong, not the code. An
attempt was tagged with the phase current when its dial *began*; three
attempts tagged "blocked" had dialled after the block was lifted.
Attributing them to the blocked phase would have reported a leak that did
not exist. Attempts are bracketed now: an attempt counts for a phase only
if that phase held at both ends, and one that straddles a change is
counted separately. The straddle count lands on exactly workers × changes
(16 for 8 workers across 2 changes), which is itself a check that the
brackets work.

The measurement: **0 of ~11,000** connections dialled after the country
was blocked were served, traffic resumed when it was lifted, under
`-race`.

### Numbers in a file, text in a column

A config file writes ASNs as TOML numbers (`blocked_asns = [64512]`) and
the settings column holds text, so these are the only entries in the
migration catalogue whose stored shape differs from the file's. Loosening
the validator to accept numbers into any string list would have been the
smaller diff and the wrong one — it would let a number become a string
silently for every setting, everywhere, to spare one conversion in one
place. The conversion is per-entry, and anything that is not a whole
number passes through untouched so the registry's own validator is what
refuses it and the operator gets that message.

AS0 is refused at both ends, which matters more than it looks: 0 is
asnlookup's "could not resolve", so an AS0 rule would block every address
the lookup failed on rather than one network.

### Tests that only pass on a database they have not run against

Running the integration suite twice in a row failed the second time. Two
tests were writing rows they never cleaned up and then, on the next run,
tripping over their own traces.

`TestRetentionNeedsTheDeveloperPassword` posts 120 days with a wrong
password and expects a refusal — but its last step sets that site to 120
with the right one. On the second run the handler correctly saw no
change, answered "nothing changed", and never reached the password check
the test exists to make.
`TestTheStepOffersEveryBlockPreTicked` asserts an unconfigured site opens
on the default set, while its neighbours save narrowed sets for that site
and leave them there.

This is the worst way for a test to be wrong: green when you write it,
red for the next person, who has changed something unrelated and now has
to work out that they did not break it. Both are fixed by clearing the
per-site rows before the test as well as after — before, because a run
that was interrupted leaves rows behind, and cleaning only on the way out
makes the next result depend on how the last run ended.

A related one found while reading that code: `clearMigrated` had been
deleting from `panel_audit`, and the table is `panel_audit_log`. The
error went into the blank identifier that ignores it, so half of that
helper had never once run. Nothing failed, because the only test that
reads those rows filters by its own temp-dir path — but a helper whose
errors are discarded cannot report that it is not working.

A third one appeared only after running the suite enough times, and it is
the best of the three. `TestSignInWithAPassword` tries an address that
has **no account**, to prove the panel answers a wrong password and an
unknown address identically — anything else is a way to ask the panel
which addresses have accounts. `makeUser` clears the failed attempts for
every account it creates, which is every address except that one, because
nothing creates it. Its attempts accumulated across runs until the rate
limiter tripped, and the test then reported that the unknown address
answered 429 while the wrong password answered 401: a real difference,
correctly detected, entirely manufactured by the test database. The
oracle was in the fixture, not in the panel. Attempts against the test
domain are now cleared before and after like everything else.

Three of these in one phase is a pattern worth naming: **every table a
test writes to needs an owner, and "the test that created the row" is not
always available as an owner.** The two that bit hardest were both rows
nobody created on purpose — a settings value written by a handler, and
attempts recorded for an address that exists only to not exist.

### What a running collector actually did

The load test proves the mechanism; this proves the path from the panel's
table to a process in the traffic path. A real collector, against the
real database, with a two-second poll:

| Change written to `panel_settings` | What the running process logged |
|---|---|
| `blocked_countries` → `["RU","CN"]` (file said `["KP"]`) | `geo blocklist changed countries=2 was_countries=1` |
| `known_bot_asns` → `["64513"]` (file said `[64512]`) | `known-bot ASN list changed asns=1 was=1` |
| `logs.level` → `debug` | `logging level changed from=INFO to=DEBUG` |
| `blocked_asns` → `["0","abc","64520"]`, edited by hand past the panel | `geo blocklist changed asns=1` |

The second row is the whole point of the fix: **`asns=1 was=1`** is a line
the old length comparison could never have produced, because it decided
nothing had changed. The third row was impossible in the collector at all
before this phase.

The fourth is the hand-edited row the validator never saw. AS0 and `abc`
were both dropped and only 64520 became a rule — 0 is asnlookup's "could
not resolve", so an AS0 rule would have blocked every address the lookup
failed on.

## Retention left the panel

Three decisions were open at the end of A5.2 and the customer settled all
three. The first was how long analytics data is kept and where that is
decided: **the config file, default 90 days, ceiling 730.**

### Why a setting moved backwards

Everything in A5.1 and A5.2 moved settings *out* of config files and into
the panel, on the argument that reaching a server during an incident is
the longest possible path. Retention went the other way, and the reason
is not inconsistency.

Every other setting in the registry is operational: a wrong value costs
performance, accuracy or disk. Retention decides how long a person's
browsing is held by an organisation they have never heard of, and it is
the direct subject of KVKK's proportionality rule. It was already behind
the developer password, which is a strong lock — on the door of a room
the customer was still standing in. The value was visible over HTTP,
editable over HTTP, and one leaked password away from being somebody
else's decision.

Nothing was lost in the move except reach. Per-site retention came along
into the file, because "this customer asked for thirty days" is a real
request and dropping it would have made the relocation a removal.

### The ceiling, and why it is not ten years

`MaxDays` was 3650, documented as "the point past which keep it and keep
it forever stop differing in any way that matters". That is a true
sentence about arithmetic and the wrong basis for the number. A product
whose ceiling is a decade invites a deployment nobody can defend.

730 rather than 365 because the honest use for old analytics is "the same
month last year", and a ceiling of one year makes that comparison
impossible on the last day it is wanted.

### The dangerous direction of a lowered ceiling

Both services treated an out-of-range retention as unset and fell back to
90 days. With a ceiling of 3650 that was nearly unreachable. With a
ceiling of 730 it is the *most likely* state of an upgrading deployment,
because 3650 used to be legal and is exactly what somebody wanting "keep
everything" would have written.

Falling back would mean a deployment that believes it keeps five years
keeping three months, and finding out when a customer asks for last
year's figures. So both services now refuse to start, naming the value
and the bounds. Refusing is louder than any log line and cannot be
missed — the same reasoning `privacy.ip_storage` already used one
function below.

### A setting that did nothing at all

`analytics.compress_after_days` went with it, for a different reason:
**nothing read it.** It had a label, help text, a developer-password gate
and an audit trail, and no service anywhere in the repository ever looked
the key up. A customer could change it, the panel would record the
change, and TimescaleDB would go on compressing exactly as before.

That is the same class of defect A5.2 spent a phase on — a setting whose
correctness nobody could observe — and it had been sitting in the
registry the whole time. The mirror test that catches this only walks
keys marked `Live`, so a non-live key with no reader was invisible to it.
If chunk compression is worth having it needs a reader first.

### What the removal touched, and what it revealed

Removing two registry keys reached further than expected, and two of the
places it reached were already broken:

- The wizard's retention step refused itself when no site was configured,
  because analytics retention was per site. With that setting gone the
  refusal blocked a step that works.
- `TestRetentionNeedsTheDeveloperPassword` cleaned up by calling
  `SetSetting` on a password-gated key, which the store refuses. The
  error went to the blank identifier, so the cleanup had never once run.
  It deletes the row directly now. That is the third instance of the same
  pattern this week: **an error assigned to `_` in a test helper cannot
  report that the helper does not work.**
- No site-scoped integer setting remains, so the test that proved
  "site row overrides, missing row falls through" had to move to the site
  name. Worth noticing rather than deleting: it is now the only test of
  that mechanism.

One more, caught in the new tests themselves: the first version of
`internal/beacon/config_test.go` put the two required fields under a
`[storage]` table header, where the loader never read them. Every
"out of range is refused" case passed on `timescale_dsn is required` and
the bounds were never exercised. The tests assert *which* error now, not
just that there was one.

## Apache-2.0, and the two things a licence cannot do

The project was MIT. It is Apache-2.0 now, on a customer decision with
three requirements: running it as a service stays allowed, attribution
gets stricter, and no liability is accepted.

Apache-2.0 answers all three with a text lawyers already recognise.
Section 4 is the part that matters: a redistributor has to keep the
licence, state what they changed, and carry the `NOTICE` file's
attribution. MIT asks only that a copyright line travel with the source.
Sections 7 and 8 are a page of disclaimer where MIT has one sentence.

The alternative on the table was "MIT plus our own extra terms", which
would have given the exact rule asked for — including visible
attribution in the panel's own footer. It was refused for a reason worth
recording: a bespoke licence is not a stricter licence, it is an
unfamiliar one. Every corporate adopter's lawyer has to read it from
scratch, and the friction lands on exactly the people this is built for.
A well-known licence that gets 90% of the rule is worth more than a
custom one that gets 100% and is never adopted.

### Verified rather than remembered

The `LICENSE` file was copied from a dependency's own Apache-2.0 text
and then diffed against `apache.org/licenses/LICENSE-2.0.txt`. It is
byte-identical ignoring trailing whitespace. Reproducing a 202-line
legal text from memory is exactly the kind of thing that looks right and
is subtly wrong, and a licence is a bad place to find that out.

### What a licence does not do

Two of the customer's four requirements are not licence terms at all,
and putting them in the licence would have been the mistake:

**"Ship code, not data."** A licence grants rights to a work; it cannot
stop a release from containing a database dump. That is a property of
what gets committed, so it lives in `.gitignore` — collected analytics,
database dumps, logs, binaries, config files and fetched datasets, each
excluded for its own reason, several of them because they would carry
real visitors' personal data. The rule was already followed; it was
never written down or enforced.

`.gitignore` needed one exception, and it is the kind that bites later:
`*.csv` and `*.pem` are runtime output at the root and legitimate test
fixtures under `testdata/`. Without `!**/testdata/**` a future fixture
would be ignored silently, and the next person would have a test that
passes for them and fails in a fresh clone. Verified by actually
running `git add` in a scratch repository rather than by reading
`check-ignore`'s output, which prints the matching rule for negations
too and is easy to misread.

**"Accept no responsibility."** Sections 7 and 8 disclaim warranty and
liability, which is the most a licence can do. What data a deployment
keeps, for how long, and who is legally responsible for it is the
deployment's decision under its own jurisdiction, and no licence text
changes that. The README says so plainly rather than implying the
licence has settled it.

### A file that never existed

Found while checking the new cross-references: `KURULUM.md` sent readers
to a data-inventory document — VERI-ENVANTERI, named without backticks
here on purpose — for "which data is kept and why", and that file has
never existed in this repository. Not deleted, never created, though the
task list records it as done. The reference is repointed at the README's
privacy model.

The name is deliberately not written as a link above, and that is the
whole lesson rather than a footnote to it. A backticked filename reads
as a promise: a reader follows it. G1 turned this check into a test
(`internal/docs`), and the first thing that test failed on was this
paragraph — correctly, while it still used backticks to name a file that
does not exist. The fix is not to teach the checker about intent, which
is impossible, but to stop writing dangling links.

## Planning the release pipeline, and what measuring it turned up

Two of the three things standing between this and a product had no owner
in the plan: there is no CI, and there is no way to package a release.
Both are now group G, planned rather than listed.

### Not every test should gate a merge

The obvious CI is "run the tests". This repository has four test
surfaces and they do not mean the same thing, so treating them alike
would produce a pipeline that is either useless or permanently red.

`internal/botdata.TestLiveFetch` reaches the public internet to check
that the known-bot data source still exists and still has the shape the
parser expects. It failed every time it ran today, because the host was
unreachable from this container — plain `curl` timed out on the TLS
handshake too. In a merge gate that is a test which fails for reasons
having nothing to do with the change, and the predictable response to a
test like that is for people to start ignoring red. Deleting it is worse:
noticing that an upstream source moved is exactly its job. So it runs on
a schedule and reports, and never blocks.

The load tests are the other awkward case, for a different reason. Their
assertions are ranges tuned on a real machine — "between 5 and 20 of 50
got through" — and a shared CI runner is noisier than that. The honest
plan is not to guess: run them nightly, and promote them to the gate
only if they prove stable over a run of nights.

`govulncheck` needs both. On every pull request, because that is where a
new dependency arrives; and on a schedule, because a CVE published
against code nobody touched is precisely the case a PR-only run cannot
see.

### The requirement that would not have been obvious yesterday

**The integration suite has to run twice, in the same job, against the
same database.**

Three tests were found today that only passed against a database their
own previous run had not touched. A CI that provisions a clean
PostgreSQL for every run — which is the normal, recommended design —
would have hidden all three permanently. They would have shown up only
on a developer's machine, on the second run of the day, looking like an
unrelated regression.

So the pipeline runs the suite twice. It costs one more run of a suite
that takes about half a minute, and it guards a class of bug that is
otherwise invisible to the machine that is supposed to be guarding.

### Four binaries that cannot say what they are

Measured while planning G2, with `go tool nm`: the symbol `main.version`
exists only in `cmd/panel`. `KURULUM.md` tells the installer to build
all five with `-ldflags "-X main.version=$VERSION"`, and the Go linker
does not warn when `-X` names a symbol that is not there — it silently
does nothing.

So the documented build command is inert for four of the five binaries,
and support's first question, "which build are you running", has no
answer for the collector, the beacon, the API or `devpass`.

Adding the symbol belongs to G2, because it is a property of the build.
Surfacing it — in the health page, in the operations log — is B7's, and
the order matters that way round: a version stamp that nothing reads
would be the same shape as the setting A5.2 deleted for doing nothing.

### Why G1 moved to the front

The plan's order changed once before, on a measurement. It changed again
here, on another one: this session's end-of-phase verification was done
by hand and took roughly forty minutes — four tag sets, the browser
suite, cross-compilation, and rebuilding the database from nothing after
the container rolled `/usr` and `/var` back.

That cost is paid every phase, and the thing paying it is a person
remembering to. Every phase that brings the product closer to a customer
also makes that unpaid bill larger. The two worst findings in the
security audit were in dependencies and were found by a tool, not by
reading — leaving that tool dependent on someone's memory contradicts
the audit's own lesson.

## G1: the pipeline found its own reason on the first run

The gate was written, then run once by hand before being committed. Its
own justification — "the two worst findings in the audit were in
dependencies and could not have been found by reading" — turned out to
be understated.

`govulncheck` against the tree as it stood: **34 vulnerabilities in the
standard library, all reachable**, with example traces through
`fullproxy.Server.Serve`, `beacon.Serve` and `asnlookup.NewResolver`.
Zero in third-party dependencies.

Nothing in this repository had changed. `go.mod` pinned `go 1.25.0`, the
patches landed in releases up to 1.25.13, and the audit that ran
`govulncheck` by hand had run it before those CVEs were published. This
is exactly the case the nightly job was written for — "a CVE published
against code nobody touched is what a pull-request-only run cannot see"
— and the evidence arrived before the pipeline did.

The fix is one line: `go 1.25.13` in `go.mod`, which is a patch bump and
raises what a builder needs by nothing that matters. Measured after:
0 vulnerabilities. KURULUM.md's requirement moved with it, with the
number in it, because "1.25.0+" would now be a documented instruction to
build something vulnerable.

### Making the browser suite portable was a prerequisite, not a detour

The browser tests write an ESM script to a temp directory and run it
with node. A script there cannot resolve `playwright` by name, so it
imported the module by absolute path — and that path was
`/opt/node22/lib/node_modules/playwright/index.js`, written into nine
scripts across eight files. Chromium's location was hard-coded the same
way, nine more times.

Both are facts about one container. The browser suite is a merge gate,
so on a runner the gate would have been red from the first commit.

`internal/browsertest` resolves both instead of assuming them, and the
order is ask-then-guess: the environment if it was told, then `npm root
-g` because node already knows where its global modules are, then the
historical default. Every candidate is checked for existence first.

Chromium ends differently: if nothing names one, the `executablePath` is
removed from the script rather than defaulted. A Playwright that
installed its own browser finds it without help, and pointing it at a
binary that is not there fails with a message about a missing file —
a worse error than the one it replaces. That branch cannot be reached on
this machine, which is why `defaultChromium` is a var: the package's own
test swaps it to exercise the path only CI will take. A branch only CI
can reach is a branch nobody can debug when it breaks.

`Prepare` refuses a script whose expected literals are absent rather than
returning it unchanged. A silent no-op would leave the container's path
in place, the test would pass here and fail everywhere else, and the
failure would name Playwright rather than this function — the same shape
as the `-X main.version` no-op and the discarded cleanup errors found
earlier this week.

### A test that named its dependency wrongly

`TestLiveFetch` was under the `integration` tag, alongside the tests that
need a real TimescaleDB. Those two dependencies are not alike: a database
this repository starts is available whenever somebody starts it; a third
party's web server is available when they decide it is.

It has its own `network` tag now. The tag names the dependency rather
than the ceremony, which means the gate stops carrying it without a
special case in YAML, and the next network test is filed correctly by
whoever writes it.

### Two invariants that were being checked by remembering

Turkish text must not be mojibake, and a document that names another
document must name one that exists. Both have been broken here before,
and both were checked by running a regex by hand after edits.

`internal/docs` makes them `go test`. Its first run failed — on a
paragraph in this file, which named the non-existent VERI-ENVANTERI in
backticks while describing the very bug. The lesson is not that the
checker is too naive to tell a reference from a mention. It cannot be
taught that, and it should not be: a backticked filename reads as a
promise and a reader follows it. The prose changed.

One more that only appears once a machine is doing the checking: a
mojibake test passes on a document whose Turkish has been flattened to
ASCII, because "s" is not corruption. So there is a second test
asserting the characters are still present.

## C7, and a phase that was mostly already built

Opening C7 meant reading what it asked for and then reading the code,
and the two did not match. Two of its three parts — telling "the snippet
was never seen" apart from "the snippet works and nobody came", and
saying something honest when the read API is down — were already there.
D1 wrote the vocabulary (`hasData`, `neverInstalled`, `nothingInRange`,
`unreachable`, `refused`) and D2 gave breakdowns the same treatment.
Both catalogues already carry the outage sentence, and it is a good one:
*"Nothing is missing — it has not arrived."*

They were delivered as a side effect of building the interface, while
the plan went on listing them as outstanding. Writing the phase up as
though it were new work would have been the easy thing and a lie; the
phase text now says what was found.

### The half that was missing was the half nothing could see

The dashboard's survival was tested. The claim that *the rest of the
panel* is untouched by an analytics outage was not — it was true by
construction, because only two files in the package touch the client.

True by construction is a fine reason to believe something today and no
reason at all to believe it next month. The first person to put a
visitor count on the site list breaks it, and nothing would have said
so: the page would render, the number would be a zero, and a customer
would read that zero as "nobody visited".

So there are two tests now, and they catch different failures.
`TestOnlyTheAnalyticsPagesTalkToTheAnalyticsAPI` is structural and pins
the set of files allowed to call the client. It fails when somebody adds
a third, which is the point — the message tells them to add the file to
the list and, while they are there, decide what the new page says when
the fetch fails. That catches the call rather than the symptom, and a
page that called the API and swallowed the error would pass every
behavioural test while quietly showing zeroes.

The behavioural one runs the panel with its client pointed at a dead
port and walks the site list, the account page, the member list and
sign-out. Those are the pages a customer needs *most* during an outage,
because one of them is how they reach the person who can fix it.

### The assertion that could not have failed

The behavioural test was written asserting that no page rendered the
server-error text, looked up as `hata.sunucu`. That key does not exist.
A missing key comes back wrapped in markers rather than empty, so the
test was comparing the page against a string it could never contain, and
would have passed on a panel that returned an error page for every route.

It is `hata.500.baslik` now, and the fix was checked the only way worth
checking it: by forcing the branch true and confirming all three
subtests fired. An assertion nobody has watched fail is an assertion
nobody has tested.

### What could not be done, and why it is written down rather than dropped

C7 says the outage message should link to the health page. There is no
health page — that is B4, and group B stands at 1/7. Adding the link now
would point a worried customer at a 404. The requirement stays in the
plan, attached to B4, so that finishing B4 is also finishing this.

## The email discussion, and three measurements that moved it

The customer asked to talk about the email path before it was built.
Opening the question meant measuring the ground first, and the ground
was not where the plan said it was.

**Invitations already work without email.** The handover link is shown
on screen once, stored as a sha256 hash, time-limited and single-use;
developer access uses the same pattern. The two most security-sensitive
flows in the product are already solved with no mail server anywhere,
and the mechanism has been proven twice.

**Password reset does not exist at all.** Not the emailed kind, not the
other kind. So the question was never "should reset send email" — it was
"reset has not been written".

**The panel already promises recovery codes and does not deliver them.**
The catalogue says, in both languages: *"There are no recovery codes yet:
if you lose your phone, the site's owner or the operator can reset your
second factor."* A sentence containing "yet" is a promise waiting to be
kept.

### Scale is what makes recovery codes necessary rather than nice

The customer's "there could be many" turned out to mean installations,
not message volume. That distinction decides the design. With one
customer, "ring your agency" is fine. With thirty, it is a support
queue, and the person queuing is locked out of their own analytics at
eleven at night.

Recovery codes are the only mechanism that makes the owner
self-sufficient with **zero configuration** — no SMTP, no third party,
no DNS. They are generated when the account is created, shown once, and
hashed at rest, which is the pattern this repository already uses three
times over. One code serves both password reset and a lost second
factor, because from the customer's side "I lost my phone" and "I forgot
my password" are the same problem: *I cannot get in*.

### What "verify button" actually described

The customer's description of the email setup — *do these things, a
verify button, when it verifies it moves to the next step, DNS and
everything* — is the preflight pattern this project already runs on:
**the panel cannot do this, but it checks it and tells you what to do.**
It is the same shape as the database-role checks in the installer.

It is also considerably more than "add an SMTP setting", which is why it
became its own phase rather than a bullet inside C7. Its own phase can
be done properly; a bullet would have been done quickly and badly.

### Two decisions that will look conservative later

**No third-party email API.** One API key is genuinely less setup than
SMTP, and the customer even asked for a no-API-key option, so the
temptation cuts both ways. It is refused because a self-hosted,
privacy-first product should not route its customers' addresses through
somebody else's service, and because it adds an account and a bill to an
install this project has worked hard to keep to one binary and one
config file.

**DKIM is verified, not signed.** Signing needs canonicalisation code or
a dependency, and every real SMTP provider already signs. A half-built
DKIM implementation is worse than relying on the provider's, and the
check that matters — *is the mail actually signed* — can be made by
reading the headers of a test send.

### Why the link stays on screen even when email works

Because deliverability fails silently. Mail leaving a fresh VPS without
SPF, DKIM and DMARC lands in spam or is rejected outright, and Gmail and
Yahoo tightened those requirements in 2024. A password reset that
vanishes quietly is the worst available failure: the person waits, and
the panel says "sent".

So the panel will never say only "sent". It will say what it sent, to
whom, and what the receiving server answered — and the link will be on
the screen regardless, because that is the path that cannot fail.

## Recovery codes: a promise the panel had already made

The account page said, in both languages, that recovery codes did not
exist *yet*. A sentence with "yet" in it is a promise, and it had been
sitting there since two-factor authentication was built.

C7.2 keeps it. An owner who cannot get in uses a code, sets a new
password, and is inside — with no operator awake, no mail server, and
nothing configured anywhere.

### One mechanism instead of two

The plan called for two paths: recovery codes for the customer, and an
operator-minted link for somebody who lost those too. Building the
second as its own token type would have meant a second table, a second
redemption route and a second audit trail, all shaped exactly like the
first.

It is one mechanism: **the operator does not mint a link, they
regenerate the codes.** They hand one over however they like, and it
goes through the same form the customer would have used. One redemption
path, one thing to get right, one place to look when somebody asks how
an account was entered.

### Digests, not argon2id

Every other credential here is stored as a SHA-256 digest and passwords
are argon2id, so the choice needs stating rather than assuming. These
are twelve characters this process drew from `crypto/rand` — sixty bits
— not a phrase a person chose. There is no dictionary to resist, so the
slow hash would buy nothing that the entropy does not already provide,
and it would cost a redemption the same tens of milliseconds a sign-in
pays. What guards them beyond the entropy is the sign-in throttle, which
this form shares deliberately: an attacker who found the password form
rate-limited would otherwise simply move to this one and guess codes
instead. Two doors, one budget.

### Three decisions where the safe answer is not the obvious one

**The address is checked after the code, not in the query.** Filtering
on the address would do less work when the address does not exist, and
that difference is measurable from outside — it would answer, to anyone
on the internet, which addresses have accounts on this deployment. So
the code is consumed by digest alone and the account it belongs to is
compared afterwards, inside the same transaction, which rolls back when
they do not match. That rollback is a feature: somebody who mistyped
their own address does not lose the code for it.

**The second factor is kept unless asked for.** "I forgot my password"
and "I lost my phone" arrive at the same form and are not the same
request. Clearing it by default would quietly downgrade every account
that ever reset a password. And when it is *not* cleared, the recovery
code does not skip it — the redemption hands off to the second-factor
page like an ordinary sign-in. Otherwise a recovery code would make the
second factor optional for anybody who found one.

**Regenerating asks for the current password.** It mints credentials
that outlive the session asking for them, so a stolen session could
otherwise print eight codes that keep working long after the session is
gone: a temporary problem turned permanent.

### The codes are rendered, never carried

Claiming an invitation used to redirect into the owner's wizard. It now
renders the codes instead, and the redirect happens on a click.

The alternative was to keep the redirect and put the codes in the
session — which is a database table. Eight readable codes at rest, to
save one redirect, in a system that stores them as digests everywhere
else precisely so they are not readable at rest anywhere. So the page is
the only place they exist in readable form, and it says so: somebody who
closes the tab has lost them, and is told where to make more.

### Found by predicting it, then checking

The first version of the store tests minted accounts under a domain of
their own — `@kurtarma-testi.invalid` — while `newTestStore` sweeps up
accounts whose address contains its namespace. Nothing swept them up,
`CreateUser` refuses a duplicate, and the second run of the suite failed
on every test with eight accounts stranded in the table.

That is the third time this week, and this time it was predicted before
it was observed: the cleanup pattern was read, the mismatch was noticed,
and the suite was run twice to confirm it rather than to discover it.
The CI gate's second integration pass would have caught it too, which
is what that pass is for.

### The check that reported all eight of ten

Adding a table turned up two places that had to hear about it, and one
of them had been wrong for longer than this phase.

`KURULUM.md`'s GRANT block needed the new table and its sequence, which
is obvious. The wizard's "are the panel tables applied" check needed it
too — and while adding it, the list turned out to name eight tables
where the schema creates ten. `panel_owner_claims` had been missing
since invitations were built.

The check reported "all eight present" and meant it. That is worse than
having no check at all: a deployment missing either table passes the
wizard, hands the panel over to a customer, and then fails at runtime on
the one page that existed to catch exactly that.

The list is package level now and a test parses `../schema.sql` and
refuses a mismatch — by path rather than by import, because preflight
deliberately does not import panel and a test asserts that too. The
failure message says to update KURULUM.md's grants as well, since a
table the panel role cannot reach fails the same way as a table that is
not there.

Verified rather than asserted: the documented GRANT block was applied to
a real role on a real database, and the privileges read back — the panel
role can insert into and delete from the recovery table and use its
sequence, and still cannot read `traffic_snapshots`. (The collector
column of that measurement proves nothing here: this development
database gives that role superuser, unlike a real installation.)

## A feeling, and what measuring it found

The instruction was not a bug report. It was: *we are going as planned,
but I feel we are going in a way that is very exposed to security holes —
let's add security scanning phases.*

A feeling is not evidence, but it is a claim, and claims can be measured.
Two questions, asked of the whole repository:

```
=== is there a recover() on the connection path:   (empty)
=== is there a fuzz target:                        (empty)
```

Not "few". None. No `recover()` anywhere under `internal/` or `cmd/`, and
no fuzz target at all. The feeling was pointing at something real, and the
rest of the day was spent finding out what.

### What the fuzzer found: nothing, which is worth having

The JA4 parser is the code with the least protection between it and a
stranger: `ParseClientHelloFromRecords` is handed the first bytes off the
socket, before anything is validated, authenticated, or decrypted. And an
unrecovered panic in Go kills the entire process — which, for a proxy
sitting in front of the customer's website, means the customer's website
goes down. Available to anyone who can open a TCP connection.

Two fuzz targets, seeded with the five real FoxIO captures plus the shapes
that break a parser written without bounds checks. **~19.6 million
executions, zero crashes.** `cursor.go`'s discipline of returning an `ok`
from every read holds up.

That is a real result even though it found nothing, for two reasons. It
converts "written defensively" — a statement of intent — into a
measurement. And it is now regression protection: a future change that
drops a bounds check cannot pass quietly.

One detail worth keeping: the seed corpus grew from 10 to 143 entries
during the run, meaning the mutator kept reaching code it had not reached
before. Seeding from real handshakes is why. From random bytes it would
have spent the whole time failing at the record header.

### What a structural question found: a way to stop the collector

The fuzzer proved the parser does not panic *today*. It cannot prove no
code on that goroutine ever will. So the second question was structural:
if something did panic there, what happens?

`internal/proxy`'s accept loop hands each connection to a bare goroutine.
No recover, anywhere on the path. So: the process dies, every other
visitor's connection dies with it, and an attacker who found the input
repeats it after each restart — a supervisor does not help.

Demonstrated rather than argued. A rate store that panics on its first
call, one connection to trigger it, one more to prove the server still
serves. Before the fix the test did not fail; it killed the test binary
and the package reported a panic trace. That is the exposure, printed.

The fix is a recover at all three goroutine roots — the connection
handler and *both* splice goroutines, because `recover()` only sees
panics on its own goroutine and the one in `handleConn` does nothing for
the two it spawns.

There is a real argument against recovering panics: it can mask bugs and
leave a process in a corrupt state. It loses here on internal
consistency. The limiter already has a `fail_open` mode whose entire
purpose is that the collector must never be the reason a site is
unreachable. A process-killing panic contradicts a commitment this
project already made. net/http made the same call in `conn.serve`, which
is why every other server here already had the protection — they run on
`http.Server`; `internal/proxy` does not. So the log is loud, with the
stack, at Error: recovered is not the same as fine.

### What the scanner found: one in twenty-eight

`gosec` over 111 files and 29,984 lines: 28 findings. Triaged by hand,
**exactly one was real.**

The four open redirects are false positives — `rawNext` already rejects
`//host` and `/\host`, including the one every hand-rolled check forgets.
The five integer overflows are all bounded (`Score` is clamped to
`MaxScore = 100` at its source). The file permissions are on public bot
data, where `0644` is the intent.

The one real finding was `internal/fullproxy` running an `http.Server`
with no timeouts at all, while the other three servers set all four. And
it was worse than the rule's own description. net/http derives the TLS
handshake deadline from the smallest non-zero of
ReadHeaderTimeout/ReadTimeout/WriteTimeout, and applies **none** when all
three are zero. So the exposure was not slow headers after a handshake:
a client could send one byte and hold a goroutine, a socket, and up to
32KB of capture buffer open forever. Measured both ways — 10 seconds and
still holding before, 302ms against a 300ms bound after.

Only two of the four timeouts were added. A reverse proxy carries
whatever the customer's site serves, so a `ReadTimeout` breaks a large
upload and a `WriteTimeout` breaks a large download, and both break it by
the size of the file rather than by anything the customer did wrong. The
beacon and the API can cap all four because their request and response
sizes are bounded; this one cannot, and bounding those is the backend's
job because only the backend knows what it serves.

That 1-in-28 ratio is not a complaint about gosec, it is the design input
for where it belongs. Gating on it would paint the pipeline red 27 times
for every time it is right, and a red that is usually wrong is a red
people learn to click past — the same lesson G1 already learned about
`TestLiveFetch`. So it reports nightly and a person triages.

### The shape both holes shared

Neither hole was exotic. Both were *one component forgetting what the
other three do*: three servers set timeouts and one did not; everything
on `http.Server` got a per-connection recover for free and the one server
that is not on `http.Server` did not.

A person noticing would have caught both. The whole point of this group
is not to depend on that person, which is why H4 exists — the invariant
written as a test that enumerates the servers, so the next component to
forget is caught by the build rather than by whoever happens to look.

### The test that was checking nothing

Adding group H to PLAN.md changed nothing in the mirror test that is
supposed to hold the group table and the phase headings to each other.
Renaming the heading changed nothing either. Its three regexes were
written `[A-G]` — the alphabet on the day it was written — so a group
outside that range matched neither side, both sides saw nothing, and the
two nothings agreed.

That is precisely the failure the test exists to prevent, one level up: a
number nobody could check. Widened to `[A-Z]`, and verified the way it
should have been the first time — by breaking the count on purpose and
watching it fail: `group H: the table says 1/3, the headings say 0/4`.

Worth stating plainly, because I described this test earlier as a two-way
mirror: it was, within a range it did not say it had.

## The baseline, and the mechanism that would have undone it

H2 was planned as "report, do not gate", and the reasoning was the ratio:
28 findings, one real. It finished as a gate. The reversal is worth
writing down, because what changed was not the opinion but the fact
underneath it.

### Placing the phase by measurement rather than by assertion

The question was whether the scanner belonged before or after the phases
still to come. The first answer was "before, it's the smallest phase" —
which is not a reason, size has nothing to do with it.

The measurable version: does the scanner catch the kind of bug the next
phases will risk introducing? So the bug classes were written on purpose
and scanned:

```
G402 (CWE-295) TLS InsecureSkipVerify → HIGH/HIGH   ... C7.3's SMTP
G204 (CWE-78)  subprocess with variable → MEDIUM/HIGH ... F2's installer
```

Turning off certificate verification to make SMTP connect is the line
that gets written at least once in every project that sets up email, and
gosec catches it with its highest confidence. So the scanner is worth
most *after* this point — which is precisely the argument for building it
*before*.

### The mechanism that would have made that pointless

gosec's only built-in suppression is `--exclude-rules="path:RULE"` — per
file, not per finding. That was tested rather than assumed: a rule was
suppressed for a file, and then a genuinely broken second instance of the
same rule was added to that same file.

```
suppression off:  2 findings
suppression on:   0 findings
```

The mechanism hides exactly what a baseline exists to surface, and it
does it in the worst possible place: `internal/panel/web/auth.go` carries
three findings today and is the file the email wizard will grow.

So the ordering question was never the real question. Moving H2 earlier
and then using gosec's own suppression would have handed back the entire
benefit at the first suppressed file. Both halves resolve to one answer:
before, and keyed by content.

A finding's identity is the rule, the file, and a hash of the flagged
code, with the line number deliberately excluded. That is the only keying
that gets all three cases right: a line shifting is silent (a baseline
that cries on every unrelated edit is one people regenerate without
reading), editing the flagged line brings the finding back (the judgement
"this conversion is bounded" was about particular code, and that code is
now different code), and a new finding in an already-baselined file is
reported.

### Three things the work turned up that reading would not have

**The baseline had 21 entries at the end of H2, not 18.** The extra three
are in the tool itself — the scanner scans its own code, which is how an
exemption list avoids starting. A test now holds that open. The count
moves as the product grows; what does not move is that every entry has a
reason under it.

**gosec reports absolute paths, and the fingerprint includes the path.**
A baseline written in `/home/user/Crucible-Analytic` matches nothing in
`/home/runner/work/...`. Without normalisation every finding would read
as new on every CI run, and the obvious fix — regenerating on the runner
— breaks it locally instead. Symmetrical, silent, and permanent. There is
a test that fails if the two ever stop agreeing.

**The reason test caught its own author.** One of the twenty-one reasons
was "Same tool, same reason." — four words, which restates a decision
instead of supporting it. The check that requires a reason to be at least
six words rejected it before the commit. Crude heuristic, real catch.

### Why it can gate now

The thing that justified "report only" was the ratio, and the baseline
removes it: a bare scan is 21 findings of which zero are defects, against
the baseline it is zero. Red now means "this change introduced something
nobody has triaged", which is a statement about the change — the
definition of a gate.

There is a second reason, and it may be the better one: it puts the
triage where the knowledge is. The person who just wrote the line knows
why it is safe. A nightly run asks that question of whoever reads the log
next, about code they did not write.

Verified end to end rather than reasoned about: a real `InsecureSkipVerify`
planted in `auth.go` — a file that already carries three baselined
findings — comes out as `NEW G402 auth.go:464 (HIGH/HIGH)` with exit
status 1. That is the case gosec's own mechanism swallowed.

The version is pinned at `v2.29.0`. An unpinned scanner rewrites the
meaning of the baseline underneath a gate, and the first anyone would
know is a morning where every contributor is blocked by a rule nobody
chose to adopt. The nightly job runs `@latest` against the same baseline
for exactly that question — and in doing so it stopped being "the scan"
and became the same shape as govulncheck: what does a tool see today in
code nobody touched.

## D3: three sections, and two bugs the phase had to find first

D2 put six breakdowns on the site page and left a note in the registry
saying fingerprints, ASNs and the cross-source views belonged to D3.
This is the three breakdowns; the score histogram, the crossover views
and the raw export are still open, because none of them is
breakdown-shaped and forcing them into that mechanism would be the wrong
kind of reuse.

### The decision the phase turns on

The collector's rows and the beacon's rows are not the same quantity.
The beacon counts pageviews and the people behind them; the collector
counts addresses, and cannot know how many people are behind one.

That difference had to be carried in three places or it would have
produced a table that renders perfectly and answers the wrong question:

- **A third metric.** Collector breakdowns divide by the traffic
  summary's address count. Dividing them by the beacon's pageviews gives
  "twelve addresses out of four hundred pageviews", which is not a
  percentage of anything.
- **A separate row field.** The second column holds visitors for a beacon
  breakdown and bot addresses for a collector one. Reusing `Visitors`
  would have put a plausible number under a heading that asks something
  else.
- **A per-view column heading.** That heading was one fixed key in the
  template. It had to move into the view, because the alternative was a
  second renderer - and the partial's own comment says why there is only
  one: two would drift, and the empty-group row and the missing-
  denominator dash are exactly what drifts.

The countries question was the trap D2 flagged. The API has two:
`/countries` counts addresses the collector saw, `/beacon/countries`
counts people who opened a page. They are separate kinds here rather than
two modes of one, so there is no way to draw one while labelling it the
other - and the collector's is ordered last of the three, because putting
it directly under the beacon's would invite exactly the comparison the
separation exists to prevent.

### The gate, and why it is two different gates

The site page needs the role *and* the preference. A detail page reached
by URL needs only the role.

That looks like an inconsistency and is the opposite. `ShowsTechnical`
exists because the role and the preference answer different questions:
the role says whether this person may ever see a fingerprint, the
preference says whether they want to right now. The site page appears
without anybody asking for it, so it obeys D6 - no fingerprints in the
default view. Typing the address of the fingerprints page *is* asking.
Refusing that would make the preference an authority, which is the one
thing it was designed not to be.

The paired test is the one D1 established for the analytics token's blast
radius. An owner and a viewer on the same site, and the viewer's
developer preference turned **on** - deliberately, because a gate you can
open by ticking your own box is not a gate. The owner gets 200 with a
fingerprint on the page; the viewer gets 404 with none. Both halves in
one test, because checking only the refusal passes against a handler that
refuses everybody.

### Two bugs, both quiet, both found by measuring

**The same mistake in two places.** `request()` and `detailData` each
hardcoded the beacon summary. That was correct for all six D2 breakdowns
and D2 left a note beside it saying what to do when it stopped being -
which D3 did. The failure is silent: every row and every count draws
fine, and only the share column empties to dashes, because a summary
nobody asked for comes back a legitimate zero rather than an error. Both
call sites now go through one `summaryFlags`, since they had already
proved they drift.

**An unresolved ASN is "0", not "".** The API selects `asn::text` from an
INTEGER column defaulting to 0; the country column is TEXT defaulting to
''. Both mean "never determined" and only one looks like it. Sharing one
decoder drew the unresolved addresses as a group named 0 - which reads as
a real network number. It took a real database to notice, so there is now
a unit test too.

### The browser measurement, and a wrong conclusion I had to withdraw

A JA4 fingerprint is fifty characters of unbroken hex that wrap nowhere -
the longest unbreakable string this product puts in a table cell. Worth a
browser test, since nothing in Go can answer it.

It passed immediately, so I tried to break it. Removing `.tablo-kaydir`'s
`overflow-x: auto` changed nothing. Removing `th.ad`'s `overflow-wrap:
anywhere` changed nothing. I concluded the measurement was broken - that
`scrollWidth` on a section with `overflow: visible` cannot report an
overflowing child - and that the D2 test had been vacuous all along.

That was wrong, and forcing the issue is what showed it: a table pinned
to `width: 3000px` **with the scroll container also removed** reports
`overflowing: 3` and `page_scrolls_sideways: true`, and the test fails
loudly. The measurement has teeth.

The real explanation is that there are two independent defences, either
of which is sufficient: the wrap rule keeps the fingerprint narrow, and
the scroll container keeps anything wide inside its own box. Each of my
single-removal experiments was defeated by the other layer. The lesson is
about the experiment rather than the code - removing one guard proves
nothing when a second one covers the same failure, and "I could not break
it" is not the same measurement as "it cannot break".

### What H2 bought, one phase later

The gate built in the previous phase ran against this one's code and
reported no new findings. That is the first evidence that the ordering
argument was right: D3 is the first phase written with the scanner
already in place, and C7.3's SMTP - the one that will try to hand it an
`InsecureSkipVerify` - is still ahead.

## G2: the package, and three lines that looked right and did nothing

The phase existed because of one measurement: KURULUM.md told operators to
build all five binaries with `-X main.version=$VERSION`, and the symbol
existed in one of them. The Go linker does not warn when `-X` names a
symbol that is not there - it silently does nothing - so four of the five
iterations of a documented command had no effect.

By the end the phase had found two more of exactly that shape.

### The version stamp, and what the binary already knew

Adding `var version string` to four mains closes the finding, but a stamp
nothing reads is the "setting that does nothing" pattern this project
removed in A5.2. So all five got a `-version` flag: the question support
asks, answered from a shell, without waiting for B7 to put it on a health
page.

Writing that turned up something better than the plan asked for. Go
already embeds the VCS revision and a dirty flag into anything built from
a working tree, with no flags at all. The old comment in cmd/panel said an
unstamped build shows nothing because "empty is honest rather than a
made-up default" - but empty was not the honest answer available. The
binary knew exactly which commit it came from. So the stamp wins when
there is one, the embedded revision answers when there is not, and
`unknown` is left for the case where the binary genuinely knows neither.

### The reproducibility claim was false, and the obvious test would have missed it

Two builds from the same directory produced identical checksums. Two
builds from *different* directories produced five different binaries.

The cause was not `-trimpath`, which was working in both - it was the VCS
embedding I had just added. And the reasoning that settles it is about who
the property is for: the point of a reproducible release is that somebody
who downloaded the source can rebuild it and get the same bytes. That
person has a tarball, not a repository. A build that embeds git metadata
is one they can never match, which makes the claim untestable by exactly
the person it exists for. Hence `-buildvcs=false` in the release build,
where the version comes from `-X` anyway.

The test builds from an export with no `.git` in it, and asserts the
export really has none. A test that built twice in one directory would
have passed the whole time.

### A check that could only see its own cp lines

The scope note promised the "never ship this" list would be checked by a
machine rather than by care. It was, inside build.sh - and writing the
test that plants forbidden files exposed two problems at once.

The small one: build.sh clears its staging directory before it starts, so
files planted beforehand were deleted before the check could see them. The
first version of that test planted three files and watched all three
vanish.

The larger one: a check that runs only inside the build can only ever
inspect files the build itself copied. That is a check on the script's
`cp` lines, not on the package. Extracted into `release/verify.sh`, it can
be pointed at any directory - a package somebody else built, a tarball
unpacked from a download - and it is the half a person can run without a
Go toolchain, which is who receives a package rather than makes one. Six
forbidden file kinds are planted in the test and each one has to be
refused.

### systemd disagreed with my own validator

The units carried `StartLimitBurst` and `StartLimitIntervalSec` in
`[Service]`. systemd moved those to `[Unit]` in v229 and *ignores* them in
the wrong section rather than refusing the file, so the restart rate
limiting I had written did nothing.

I checked the units first with a hand-written list of directive names, and
it passed - the names are real, only the section was wrong. `systemd-analyze
verify` found it in one run. The check is now that tool rather than my
list, because the only reliable reader of a systemd unit is systemd.

Three findings in one phase, all the same shape as the one the phase was
opened for: a line that looks right, is accepted by the thing reading it,
and has no effect. None of them fails loudly, and none of them could be
found by reading carefully - only by asking the thing that consumes the
line what it thinks the line says.

### What is not done

The package has not been opened on a clean VM and installed by following
KURULUM.md. That cannot be done from this container, and pretending
otherwise would be the kind of unverified claim this phase spent its time
removing. It is real remaining work, and F2 - the install script - is the
phase that automates the thing that check would be testing.

## D3, finished: the measurement the product exists to make

The three breakdowns were the mechanical half. The other half is the one
that justifies the whole architecture.

An analytics tool that relies on a snippet in the page can only ever
count visitors that ran it. That is not a limitation it can report,
because the population it missed never touched it - there is no number
to be short by. This collector sits in the connection path, so it sees
both, and the difference is a number: five addresses reached the server,
none of them ran the beacon, and a beacon-only tool would have said zero
visitors while this page lists five.

That is the crossover section, and it is why the two address lists exist
beside it: the silent ones nothing else can see, and the bots that did
run JavaScript and would be counted as people.

### A number that is not a measurement

`beacon_only_ips` counts addresses the beacon heard that the collector
never saw. In a correct deployment it is zero, because a browser that
loaded the page necessarily connected through the collector first. A
non-zero value means the collector is not in the path, or the beacon's
`trusted_proxies` is wrong and it is recording a proxy's address.

So it is not drawn beside the other three. It appears only when it is
non-zero, in its own sentence, saying what it means. Drawing "0" next to
the counts would invite a reader to treat a fault indicator as a
measurement - the same mistake as reporting an unreachable API as zero
traffic, which this panel refuses everywhere else.

### Two findings, both from tests that already existed

**An inline style, and a comment claiming it was allowed.** The histogram
drew its bars with `style="width: N%"`, and the comment beside it said
the Content-Security-Policy permits style attributes and forbids only
inline `<style>` and `<script>`. It does not: the policy is
`style-src 'self'` with no `unsafe-inline`, so the attribute is blocked
and the bars would have rendered at zero width. The structural test that
forbids inline styles said so before a browser ever saw it. Eleven width
classes now, at ten per cent resolution, which is plenty for a shape read
at a glance.

Worth noting what the failure would have looked like without that test: a
page that renders, with headings, and bars of zero length. Nothing
errors.

**A browser invariant that quietly stopped covering everything.** The
browser test asserted that every section either draws a table or says why
it does not. True for nine breakdowns; the histogram draws a list of
bars, which is a third shape. The measurement said 11 sections, 4 tables,
6 explanations - one unaccounted for.

The tempting fix is to relax the invariant. The right one is to widen the
measurement, because "every section draws something" is the property
worth having: a heading over blank space reads as a fault. So the script
counts bar lists too, and there is now a separate check that the count is
not zero - because zero bar lists is exactly what a blocked inline style
would have looked like.

### What is left, and why it is not mine to finish

Raw export. `Snapshot` carries an `IP` on every row, and while the
database never holds a whole address, an export produces a *file that
leaves the panel*: outside any retention policy, outliving the session,
forwardable to anyone.

That is a decision of the same class as the IP masking one, which went to
a lawyer. It now has an owner's answer - masked or not is a setting behind
the developer password, alongside `privacy.ip_storage` - and that setting
already exists behind that gate, so what remains is the export itself
under a decision that has been made rather than assumed.

## F2: the authority no GRANT shows

The install script was meant to save an afternoon per customer. What it
was actually for is in the phase description: the role separation is half
this system's security foundation and was typed by hand, and a wrong
GRANT does not fail. It produces an installation that works, serves
customers, and quietly does not have the property the design rests on.

Three such silences turned up while building it, and all three were found
by running the thing rather than reading it.

### Roles are cluster-wide

"If none of the four exist, create all four" skipped creating three of
them on a machine that already had a `collector` role - which this
project's own development cluster does. The GRANTs then failed on a role
that was never made. The same thing would happen to any operator whose
previous install got half way, or whose machine runs something else with
a role of that name. Each role is checked and created on its own now.

### `set -o pipefail` and a reader that stops early

Twice, from one cause. `psql ... | grep -q` exits as soon as grep
matches, psql dies of SIGPIPE, and the pipeline reports 141 - so the
script aborted on the *success* case, silently, right after printing that
a role already existed. `newsecret`'s `tr ... | head -c 32` was the same
shape.

Both replaced with forms that have no early-exiting reader. Any pipe
whose consumer can stop first is a failed pipeline under these settings,
and the failure looks like the script simply stopping.

### The one that matters: table ownership

A table's owner holds every privilege on it implicitly, for ever,
regardless of what was granted or revoked.

So schemas applied over a connection authenticated as `collector` leave
that role owning every table, and the isolation is void - while looking
perfect. The grants read correctly. A privilege listing reads correctly.
And the panel can read analytics.

This happened here. The first run of install.sh used a superuser DSN that
happened to be the collector role; every GRANT applied without error; and
the verification reported that the collector could touch `beacon_events`.
The verification was right and the installation was wrong, which is the
outcome that check exists for.

`verify.sql` now asserts that no service role owns a table, and that none
of the four is a superuser - the second because a superuser holds every
privilege by definition, so every negative assertion turns false for one,
and without a line naming that the failure is baffling.

### One file, and refusing rather than reporting

The GRANT block lived in KURULUM.md. It now lives in
`release/sql/grants.sql`, with its reasoning, and the document points at
it. A test fails if a `GRANT` line reappears in the document, because two
copies drift in one direction every time: the script gets fixed because
it runs, and the document keeps telling the next operator to grant
something else.

The verification is thirteen assertions and eight of them are negative,
because a GRANT block that ran without error proves the statements were
accepted and says nothing about what was *not* granted. A single false
stops the install.

And it was shown refusing, not passing. Each isolation property is broken
in turn - the panel given read on analytics, the API given write, the
audit log made erasable, a service role made superuser - and the script
has to exit non-zero naming the assertion that fell.

### The IP key check compared its own output with itself

The plan singles this out: the key goes into two configuration files, the
services never read each other's, and preflight can only see one - so it
can check presence and not sameness. Different keys do not fail; the
crossover join matches nothing, silently, and the view that proves this
product's whole claim reads zero and looks like a quiet week.

The first version generated a key, wrote it to both files, and compared
them. That compares the script's output with itself and can never
disagree - a hand-edited `beacon.toml` with a mistyped key passed it
without a word. Which is exactly the failure it exists to catch, since
the key is copied by hand.

It reads both files first now, and refuses when they differ, reporting
hashes rather than the keys: a mismatch is worth printing, the secret is
not. A key already in place is never rotated, because rotating it would
disconnect the pseudonym of every row already stored from everything
written after.

### What is still not done

The package has still not been unpacked on a clean VM and installed by
following KURULUM.md end to end. install.sh is now the thing that check
would exercise, and it is tested against a real database - but "a fresh
machine, from the tarball, by the document" remains untested, and saying
otherwise would be the kind of unverified claim these two phases spent
their time removing.

## C7.3 — e-posta, ve net/smtp'nin göndermediğimiz şifresi

Bu fazın çıktısı "panel e-posta gönderebiliyor" değil. Panel zaten
e-postasız çalışıyordu ve öyle kalması gerekiyordu: davet bir bağlantı
(C7.1), parola sıfırlama kurtarma kodları (C7.2). Çıktı şu: **e-posta
çalışmadığında kuran kişi tek ekranda tam olarak neden çalışmadığını
görüyor.**

Aradaki fark somut. "Kimlik doğrulama başarısız" ile "sağlayıcınız
2023'te SMTP parolalarını kapattı, uygulama parolası üretmeniz gerekiyor"
arasındaki mesafe, beş dakikalık bir kurulumla terk edilmiş bir kurulum
arasındaki mesafedir.

### Kaynağı okumak üç gerçek hata çıkardı

Tasarımı yazarken `net/smtp`'nin kendi kaynağını okudum. Üçü de yazdığım
kodda vardı, üçü de sessiz.

**PlainAuth şifreyi açık bağlantıda gönderir.** `auth.go`'daki
`isLocalhost`: sunucu adı `localhost`, `127.0.0.1` ya da `::1` ise
şifreleme koşulu düşüyor. Savunulabilir bir muafiyet — baytlar makineden
çıkmıyor — ama görünmez, ve yapılandırma dosyasındaki bir dizeyle
kararlaştırılıyor, paketlerin nereye gittiğiyle değil. Daha kötüsü:
`DiagNoTLS`'in "şifre gönderilmedi" iddiasını, insanların en çok yanlış
kuracağı yerel relay senaryosunda yalana çeviriyordu.

Karar artık `net/smtp`'den önce burada veriliyor: şifre varsa ve bağlantı
şifresiz ise AUTH komutu hiç yazılmıyor. Muafiyet yok. Kimlik bilgisi
olmayan hesap (yerel relay) hâlâ çalışıyor, çünkü ortada açığa çıkacak
bir sır yok.

Bunu kanıtlayan test **kendi kontrol grubuyla** geliyor. Sunucu
127.0.0.1'de, yani tam olarak kütüphanenin muafiyet uyguladığı adres. Alt
test aynı sunucuya düz `net/smtp` ile bağlanıyor ve şifrenin açıkta
gittiğini gösteriyor: `AUTH PLAIN AHBhbmVsAGdpemxp`. Kontrol olmadan
yeşil sonuç, "bizim kodumuz hiçbir şey yapmıyor ama kütüphane de zaten
göndermedi" ile ayırt edilemezdi.

**Başarısız TLS el sıkışması "kimlik reddedildi" diyordu.** Sertifika
doğrulamasını hiçbir yerde kapatmadığımız için kendi imzalı bir posta
sunucusu buraya düşer — ve o kişiye doğru şifresini tekrar tekrar
yazdırmak yerine sebebini söylemek gerekir. `DiagTLSFailed` eklendi.

**587'ye implicit TLS ile bağlanmak "ulaşılamıyor" diyordu.** Oysa TCP
sorunsuz bağlanmıştı; yalnız port yanlıştı. `tls.DialWithDialer` ikisini
birden yaptığı için hangisinin başarısız olduğunu söyleyemiyor. Artık
önce TCP, sonra el sıkışma — ve `tls.RecordHeaderError` tipiyle "düz
metin porta TLS" ayrı bir tanı.

### Sunucunun cevap metni hiç okunmuyor

İlk taslak göndereni alıcıdan ayırmak için sunucunun cevabında "sender"
ve "recipient" kelimelerini arıyordu. Türk bir sunucu reddini Türkçe
yazar; cevap o an güvenle yanlış olurdu. Ayrım artık hangi komutu
yazdığımızdan geliyor (`StageSender` / `StageRecipient` / `StageData`) ve
TLS tarafında hata **tipinden**. `TestRefusalDiagnosisDoesNotDependOnReplyWording`
Türkçe cevaplarla bunu sabitliyor.

### Yedi korumayı bozdum, altısı yakalandı

Mutasyon testi: her korumayı tek tek kaldırıp testin kırmızıya döndüğünü
ölçtüm. Altısı yakalandı. Yedincisi — `FromName`'deki satır sonu
temizliği — hayatta kaldı. Ölçünce sebebi çıktı: `mail.Address.String`
adı zaten base64'e çeviriyor, satır sonu dahil. Yani temizlik hiçbir şey
korumuyordu ama kodda "bu gerekli" diyordu. Silindi. `sanitizeHeader`
yalnız `Message-ID`'de kaldı — elle kurulan tek başlık orası.

### Kendi yorumumu bir ölçümle çürüttüm

`decodeKey`'in yorumu, base64 bir anahtarın hex sanılabileceğini iddia
ediyordu. 32 baytın base64'ü daima `=` ile biter ve `=` hex basamağı
değil — yani imkânsız. On bin rastgele anahtar, sıfır karışma. Yanlış
iddia sessizce silinmedi, yorumda kayıtlı: **var olmayan bir tehlikeyi
anlatan yorum, sonraki okuyucuya doğru yorumlara da inanmamayı öğretir.**

### Şifre: hash'lenemeyen tek sır

Bu veritabanındaki her kimlik bilgisi bir özet — parolalar argon2id,
davetler ve jetonlar ve kurtarma kodları SHA-256 — çünkü hiçbirinin
aslına ihtiyaç yok. SMTP şifresi her gönderimde karşı sunucuya verilmek
zorunda, yani geri okunabilir olmak zorunda.

Soru "hash'lensin mi" değil, **"veritabanının bir kopyası ne işe yarar"**.
Cevap: yapılandırma dosyası olmadan hiçbir şey. `internal/sealed`,
AES-256-GCM'i `cipher.NewGCMWithRandomNonce` üzerinden kullanıyor — bu
yapı nonce'u kendi çekiyor ve dışarıdan verileni **panic** ile
reddediyor. GCM'in sahada bozulduğu yer nonce tekrarı; hiçbir kodun
nonce'a dokunmaması bunu ulaşılamaz kılıyor.

Neyi korumadığı da paket yorumunda yazılı: panel sürecini ya da dosya
sistemini ele geçireni durdurmuyor. İkinci cümleyi yazmamak daha rahat
olurdu. **Fazla geniş ifade edilmiş bir güvenlik özelliği, hiç olmayandan
kötüdür — çünkü birisi kapsamadığı durumu düşünmeyi bırakır.**

### panel_smtp neden kendi tablosunda

Tek bir GRANT satırı yüzünden. `collector` ve `beacon_writer`,
`panel_settings` tablosunu okuyabiliyor — yenileme aralığı için doğru,
posta şifresi için felaket. `verify.sql` artık dört yeni iddia taşıyor,
üçü olumsuz, ve gerçek TimescaleDB'ye (16.13 / 2.17.2) karşı ölçüldü:
`collector`'a `panel_smtp` üzerinde `SELECT` verildiğinde kurulum
reddediyor ve hangi iddianın düştüğünü söylüyor.

Şema ayna testi tabloyu eklediğim anda yakaladı ve ne yapılacağını
söyledi: `panelTables` listesi, GRANT bloğu, KURULUM.md matrisi.

### Şifrenin tarayıcıya ulaşamaması: dikkat değil, yapı

İki okuma var. `MailAccount` her sayfanın kullandığı şey ve içinde şifre
alanı **yok** — boş değil, maskeli değil, yok. `MailConfig` düz metne
giden tek yol. Tek bir struct kusursuz çalışırdı ve canlı SMTP şifresini
dikkatsiz bir şablon döngüsüne bir adım uzağa koyardı.

Bunu koruyan test kaynaktan alan adlarını okuyor. İlk hâli hiç konmamış
bir sırrı arıyordu — asla kırmızıya dönemeyecek bir kontrol — ve atıldı.
`MailAccount`'a `PasswordMasked` eklediğimde yeni hâli adıyla bağırdı.

### İki yönlü ayna, iki paket arasında

`internal/panel/ui` alan bilgisi taşımıyor: elle yazılmış listeyle ölü
katalog anahtarı arıyor. `internal/panel/web` ikisini de gördüğü için
gerçek `mail.Diagnosis` sabitlerini kaynaktan okuyup her tanının **her
dilde** cümlesi ve önerisi olduğunu kontrol ediyor. Yeni bir tanı
eklediğimde dört satırla bağırdı: iki dil, iki anahtar ailesi.

Bu testin yakaladığı hata en pahalısı: bir tanıya ancak postası zaten
çalışmayan biri ulaşır, ve o kişi sayfanın boş bir satır gösterdiğini
görür.

### DNS sorgusu render'dan çıktı

Form çizilirken iki DNS sorgusu, kutuya yazmak isteyen birinin üç
saniyeye kadar beklemesi demekti — üstelik sayfayı belki yalnız
göndermeyi kapatmak için açmıştır. Sınama sonucuna taşındı. Ölçüm:
10.7s → 2.6s.

DKIM denetlenmiyor ve bu sayfada açıkça yazıyor. Panel posta gönderiyor,
hiç almıyor — gönderdiği bir iletinin başlıklarını okuyamaz. Alan adının
DKIM kaydı olup olmadığı da sorulamıyor: seçici sağlayıcının belirlediği
bir değer. **Çalışamayacak bir kontrol, hiç olmayan bir kontrolden
kötüdür, çünkü birisi ona inanır.**

### sastdiff'e -accept

Tek bir üçlenmiş bulguyu taban çizgisine eklemenin yolu yoktu: ya sha256
elle hesaplanacaktı ya da `-init` 21 gerekçeyi silecekti. Üçüncü seçenek
insanların gerçekten yapacağı şey — gosec'in `--exclude-rules`'u, yani bu
paketin var olma sebebi olan mekanizma. **Doğru şeyi yapmanın zorluğu,
doğru şeyin yapılıp yapılmayacağının bir parçasıdır.**

`-accept` mevcut gerekçelere dokunmuyor ve sıfırdan farklı çıkıyor:
yazdığı girişin gerekçesi henüz boş, ve dosyanın amacı gerekçelerdir.

## H5 — veritabanı yüzey denetimi: aranmayan yerde bulundu

Soru şuydu: *TimescaleDB'lerde yapay zekâlar arka kapı bırakıyor
diyorlar, kontrol eder misin.* Genel bir iddia. Kabul de etmedim
reddetmedim — ölçtüm: gerçek TimescaleDB'ye (16.13 / 2.17.2)
`install.sh` uygulandı ve her rolün gerçekte nereye ulaşabildiği tek tek
soruldu.

### Kod tarafı temiz çıktı

Aradığım şeylerin hiçbiri yoktu. SQL'e giden her değer bağlı parametre.
İki yerde tanımlayıcı enterpolasyonu var — Postgres'te sütun adı için yer
tutucu yok — ve ikisi de kapalı tip arkasında. Rollerde `SUPERUSER`,
`CREATEDB`, `CREATEROLE`, `BYPASSRLS`, `REPLICATION` yok. `public`
şemasında `CREATE` yok. `pg_authid`, `pg_shadow`, `pg_read_file`,
`COPY TO file` — hepsi reddediliyor. `pg_stat_activity` başka rolün
sorgu metnini `<insufficient privilege>` gösteriyor.

Chunk'lar da doğru: `beacon_writer`, kendi yazdığı satıra hypertable
üzerinden ulaşamadığı gibi chunk'a doğrudan giderek de ulaşamıyor — ve
denetim sırasında **sonradan** oluşturulan chunk da yetkileri doğru
miras aldı. TimescaleDB'nin en çok endişe edilesi yolu bu ve kapalı.

### Açık, kimsenin vermediği yetkilerdeydi

Üç bulgunun üçü de bir `GRANT` listesinde görünmüyor — çünkü hiçbiri
`GRANT` ile verilmemiş. Varsayılan olarak açık geliyorlar. Yetki
matrisiniz kusursuz okunabilir ve bunların hiçbiri orada yazmaz.

**1. Arka plan işi zamanlama.** TimescaleDB `add_job()` üzerindeki
`EXECUTE`'u `PUBLIC`'e veriyor. Ölçüm: `panel_user` — panel dışında
hiçbir tablo yetkisi olmayan, hiçbir yerde `CREATE` edemeyen, superuser
hiçbir şeyi olmayan rol — `add_job('pg_sleep','1 hour')` çağırdı ve iş
1000'i aldı. Sahibi kendisi, saatlik.

Yetki yükseltmesi değil: iş sahibi olarak çalışır, o rolün psql'de
yapamayacağını yapamaz. Ama **oturumdan, bağlantı havuzundan ve
uygulamanın yeniden başlatılmasından sağ çıkar.**

> Bir dakikalığına ele geçirilmiş bir süreç, aylarca çalışan, kimsenin
> bakmadığı bir yerde duran, servisi yeniden başlatmanın kaldırmadığı bir
> şey bırakabilir. Taşıdığı yetki ne olursa olsun, arka kapının şekli
> budur.

**2. Telemetri.** `telemetry_level = basic`, 24 saatte bir
`telemetry.timescale.com`'a rapor: sürüm, uzantı listesi, işletim
sistemi, hypertable ve chunk sayıları, satır sayıları. İçinde ziyaretçi
verisi yok — ve mesele o değil. Bu ürünün önermesi müşterinin trafiğinin
kendi makinesinden çıkmaması; altındaki veritabanının günlük olarak üçüncü
tarafa bağlantı açması, yükün içeriği ne olursa olsun o önermeyle
çelişir.

**3. `PUBLIC` veritabanına bağlanabiliyor.** PostgreSQL her yeni
veritabanında `CONNECT`'i `PUBLIC`'e verir. Tek başına zararsız görünür —
bağlanan yabancının hiçbir tabloda yetkisi yok. TimescaleDB'nin kataloğu
tasarım gereği herkese okunabilir olduğu için zararsız görünmeyi bırakır:
bağlanan yabancı hypertable'ları, chunk adlarını ve kapsadıkları zaman
aralıklarını sayabilir. Tek satır yetkisi verilmemiş birine kurulumun
haritası verilmiş olur.

### Betiğim kendi kuralını kanıtladı

`harden.sql`'in başına şunu yazmıştım: *"çalışan bir REVOKE ile etkili
olan bir REVOKE farklı olgulardır."*

İlk çalıştırmada döngü `run_job`'da durdu — o bir **procedure**, ve
`REVOKE ... ON FUNCTION` procedure'ü kabul etmiyor. `DO` bloğu ilk hatada
durur, yani listedeki sonraki adlar sessizce açık kaldı. Betik başarıyla
çalıştığını bildirdi. `verify.sql` yakaladı. `ON ROUTINE` ikisini de
kapsıyor.

### Kodda iki düzeltme

**`SaveMailAccount`'ta SQL birleştirmesi.** Üç sabit değişmezden biri
seçilip sorguya ekleniyordu. Enjekte edilemezdi — kullanıcı girdisi
ulaşmıyor — ama aynı depoda başka yerde kapalı tip kullanılan bir tehlike
için hiçbir koruma yoktu, ve aceleyle eklenen dördüncü dal bir şey
enterpole eden dal olur. `CASE` ifadesine çevrildi: tek ifade, üç dal
SQL'de yazdıkları sütunun yanında görünür, her değer parametre.

**Aynı desen iki farklı savunulmuş.** `countDistinct` hem kapalı tip hem
beyaz liste kullanıyor, üstelik yorumunda beyaz listeye "belt and braces"
diyor. `beaconBreakdown` yalnız tipe güveniyordu. Bir tehlikeye iki
standart, zayıf olanın incelemeden geçmesinin yoludur. Güçlü olan ikisine
de uygulandı — ve elle yazılan listenin sabitlerden kaymayacağını garanti
eden ayna testi eklendi, çünkü elle yazılan liste kayar.

### Kapatılmayan, raporlanan: bağlantı şifrelemesi

Dört DSN'in hiçbirinde `sslmode` yok. Hepsi `localhost` gösterdiği için
bugün doğrular. Ama libpq'nun varsayılanı `prefer`'dır: TLS'i dener,
sunucu sunmuyorsa **sessizce şifresiz devam eder.** DSN'i uzak bir
veritabanına çeviren kurulum, parolayı ve her analitik satırı ağdan açık
geçirir — ve yapılandırmada bunu gösteren hiçbir şey olmaz, hata da
vermez.

Zorunlu kılmadım. Tek makinelik kurulum TLS'siz doğrudur, ve bunu devri
bloke eden bir kontrol yapmak insanlara kırmızı bir satırı görmezden
gelmeyi öğretir. Bunun yerine ölçülüyor, DSN dizesinden tahmin edilerek
değil: `pg_stat_ssl` bağlantının gerçekten şifreli olup olmadığını,
`inet_server_addr()` sunucunun gerçekten uzak olup olmadığını söylüyor.

Bu kontrolün ilk hâli her loopback bağlantısına "uzak sunucu" diye uyarı
veriyordu. `inet_server_addr()::text` maskeyi koruyor — `127.0.0.1/32` —
ve `netip.ParseAddr` bunu çözemiyor. En yaygın kurulumda yanlış alarm:
yani tam olarak kontrollerin görmezden gelinmesini öğreten şey. `host()`
ile düzeltildi, ve testi yazan ben olmasam fark edilmezdi çünkü kontrol
"çalışıyor" görünüyordu.

### Kalan

RLS kullanılmıyor: siteler arası ayrım uygulama kodunda, `site_id` ile.
`analytics_reader` her sitenin satırını okuyabilir; jetonun hangi siteleri
görebileceği API'de zorlanıyor. Bu bilinçli bir tasarım ve denetimde
değişmedi — ama veritabanı seviyesinde bir garanti değil, ve öyleymiş gibi
sunulmamalı.

## B4/B7 — sağlık sayfası, ve collector'dan panele ilk kanal

Fazın öncülü yarı yanlış çıktı. B4 "bunların hepsi bugün zaten içeride
ölçülüyor, hiçbiri yüzeye çıkmıyor" diyordu. Beacon için doğruydu.
Collector'da **hiç sayaç yoktu** — ve daha önemlisi, collector'dan panele
**hiçbir kanal yoktu.**

Collector'ın HTTP sunucusu yok ve olmamalı: saldırgan baytına dokunan
süreç o, ve üzerinde dinleyen bir soket karşılığı olmayan bir yüzey.
Yazdığı tabloyu da panelin rolü okuyamıyor — ki bu sistemin güvenlik
temelinin yarısı. Yani panel, operatörün en basit sorusunu
cevaplayamıyordu: collector hâlâ yazıyor mu?

### `/healthz` neden yetmiyor

Farklı bir soruyu cevaplıyor. `/healthz` "bu süreç şu an ayakta" der; bir
yük dengeleyicinin ihtiyacı olan budur. Operatörün ihtiyacı olan ise "son
yazma başarılı oldu, 14:02'de".

Bir müşteriye bir haftalık veri kaybettiren arıza, **ayakta olan, cevap
veren ve salıdan beri her yazması başarısız olan** bir collector'dır.
Canlılık kontrolü bunu göremez.

Kanal bir satır oldu: her servis dakikada bir kendi satırını yazıyor —
sürüm, başlangıç, sayaçlar, son hata.

### RLS: `GRANT`'in söyleyemediği tek kural

Dört yazan, tek tablo. Bu şemadaki her diğer tablo tam olarak bir yazana
verilmiş, yani `GRANT` kuralın tamamı. Burada "yalnız kendi satırın"
`GRANT` ile ifade edilemiyor.

Satır düzeyi güvenlik ediyor, `current_user` üzerinden. Projede ilk kez —
H5 denetimi RLS'in hiç kullanılmadığını not etmişti; kullanılmamasının
sebebi ihtiyaç olmamasıydı, ve bu tablo ihtiyacı yarattı.

Olmasaydı: ele geçirilmiş bir beacon, collector'ın satırına "sağlıklı,
şimdi" yazıp kesintiyi tam da onu göstermek için yapılmış sayfadan
gizleyebilirdi. Küçük bir delik. Bu projenin alışkanlığı deliğin küçük
olduğunu savunmak değil.

Servis adı yapılandırmadan gelmiyor, **bağlantıdan** geliyor: reporter
veritabanına `current_user`'ı soruyor. RLS'in karşılaştırdığı değer o
olduğu için başka bir kaynak, tek bir olgu için ikinci bir kaynak
olurdu — ve servisi "yapılandırılmış görünüp hiçbir şey yazmama" durumuna
sokmanın yolu.

### Testim gerçek bir kusur çıkardı

İlk hâl adı `Run` içinde bir kez çözüyor, başarısız olunca dönüyordu.
Yani açılışta veritabanı hazır değilse servis **ömrü boyunca** izlenmez
kalıyordu. systemd bu süreçleri PostgreSQL ile paralel başlatır; o
pencere istisna değil, normal durum. Artık her atışta yeniden deneniyor.

### Şemadaki yorumum ölçümle çürüdü

"WITH CHECK olmasa servis kendi satırını başkasınınkine çevirebilirdi"
yazmıştım. Kaldırıp ölçtüm: yeniden adlandırma yine reddedildi —
`WITH CHECK`'i olmayan bir politika `USING` ifadesini yeni satır için de
kullanıyor. Yanlış iddia sessizce düzeltilmedi, yorumda kayıtlı.

`WITH CHECK` yine de açık yazılıyor, ama **gerçek** gerekçesiyle:
`USING` hangi satıra dokunulabileceğini, `WITH CHECK` neye
dönüşebileceğini söyler. Bugün aynı cevabı veriyorlar. `USING`'i
genişleten bir düzenleme — bir servisin komşusunun satırını okumasına
izin vermek gibi — örtük geri düşüş yüzünden yazma iznini de sessizce
genişletirdi.

Bu, bu oturumda üçüncü kez bir yorumumun var olmayan bir tehlikeyi
anlatması. Deseni not ediyorum: **bir korumanın gerekçesini yazarken,
gerekçenin kendisi de ölçülmeli.**

### Sayfanın tek kuralı

**Her bölüm kendi başına düşer.** Bütün bölümleri aynı anda kararan bir
sağlık sayfası, tam ihtiyaç duyulduğu anda hiçbir şey söylemez — ki
okunduğu tek an odur.

Üç kaynak, üç bağımsız arıza: servisler kalp atışı tablosundan, depolama
ikinci bir sorgudan, okuma API'si bir HTTP isteğinden. Doldurulamayan bir
bölüm, satırların olacağı yerde sebebini yazıyor; sayfayı yanına almıyor.

Bu yüzden depolama doğrudan veritabanından okunuyor, API üzerinden değil.
Sayfanın söylemesi gereken **ilk** şey "okuma API'sine ulaşılamıyor";
ikincisi ise o doğruyken hâlâ yararlı olan her şey. Tek kaynak bunu
yapamaz.

### Boyut gösteriliyor, satır sayısı değil — ve bu yazıyor

Panel `traffic_snapshots`'ın satırlarını sayamıyor. Sayabilseydi yalıtım
yoktu. Ama **boyutunu** görebiliyor: `pg_total_relation_size` yetki
istemiyor.

Boyutu satır sayısı gibi sunmak çıkarımı olgu gibi göstermek olurdu, o
yüzden sayfa ne gösterdiğini ve ne göstermediğini yazıyor. Test o
cümlenin doğru kaldığını `has_table_privilege` ile doğruluyor — cümle
okuyucuya verilmiş bir söz.

*(O testin ilk hâli yanlış şeyi ölçüyordu: kendi bağlantısından
`SELECT count(*)` deniyordu, ve takım `collector` olarak bağlanıyor —
o rolün `traffic_snapshots` üzerinde meşru `SELECT`'i var. Doğru kodda
başarısız oldu.)*

### Kimler görür, ve neden posta sayfasından farklı

Sahip ve geliştirici. Posta sayfasında geliştirici reddediliyor, çünkü
giden posta sunucusunu kontrol eden kişi her parola sıfırlama bağlantısını
alır — "postayı ayarlayabilir" ile "herhangi bir kullanıcı olabilir"
arasındaki mesafe çok kısa.

Sağlık sayfasını okumak hiçbir şey vermiyor: bir yapı numarası, bir bayt
sayısı, bir çalışma süresi. Üstelik geliştiricinin kendi teşhis aracı, ve
"hiçbir şey göremiyorum" diye başlayan destek çağrısı tam da bu sayfanın
önlemek için var olduğu çağrı.

### Mevcut bir değişmez yeni dosyamı yakaladı

`TestOnlyTheAnalyticsPagesTalkToTheAnalyticsAPI`, `health.go`'nun okuma
API'siyle konuştuğunu görüp listeye girmediğini söyledi — ve doğru soruyu
sordu: *"getirme başarısız olunca ne gösterdiğine karar ver."*

Karar verilmişti: bölüm ulaşılamadığını, taşıma hatasını ve bunun
panelin hangi kısımlarını etkileyip etkilemediğini yazıyor. Hiçbir
analitik nicelik çizmiyor, yani bir kesintinin sıfıra çevirebileceği bir
sayı yok.

### Yedi gosec bulgusu, taban çizgisine girmeden kapatıldı

Sayaç dönüşümleri (`uint64` → `int64`) yedi G115 üretti. Yedi giriş
eklemek yerine dönüşüm tek yere alındı ve **doyuran** hâle getirildi:
9,2 kentilyon satır gerekiyor ve gelmeyecek, ama "sessizce negatif olmuş
yanlış bir sayı", "en yüksek değerde takılı kalmış bir sayı"dan daha
kötü bir arıza — ve kontrol dakikada bir çalışan bir yolda bir
karşılaştırma.

## Uçtan uca kanıt: ürünü kendi paketinden kurup çalıştırdık

Bu fazın amacı tek bir cümleydi:

> gerçek bir istek panoda bir sayıya dönüşüyor mu?

Depodaki her test bir parçayı ölçüyordu. Entegrasyon paketleri bir paketi
gerçek veritabanına karşı koşuyor; `release/` kurulum betiğinin iddia
ettiği yetki matrisini üretip üretmediğine bakıyor. Hiçbiri müşterinin
ilk sorduğu soruyu cevaplamamıştı. İki yarı ayrı ayrı geçiyordu, ve **ayrı
ayrı geçen iki yarı çalışan bir zincir değildir.**

Zinciri koştuk. `e2e/e2e_test.go`: tarball'ı derliyor, temiz bir
veritabanına paketin kendi `install.sh`'ıyla kuruyor, dört yapılandırmayı
bir operatörün doldurduğu gibi dolduruyor, üç süreci başlatıyor, gerçek
bir HTTPS isteğini collector üzerinden gerçek bir origin'e geçiriyor, ve
sonra satırı `traffic_snapshots`'ta, JA4'ü satırda, sayıyı okuma API'sinde
ve panonun çizdiği rakamı sayfada arıyor.

Geçti. Ama geçmeden önce **beş gerçek kusur** çıkardı, ve beşinin de aynı
kökü vardı.

### Kök: test ortamı üretimden daha yetkiliydi

Geliştirme veritabanını `collector` kurmuştu. PostgreSQL'de tabloyu kuran
rol onun sahibidir ve sahibe her şey açıktır. Yani on entegrasyon paketi,
kendini "rol ayrımını sınayan testler" diye tarif ederken, hiçbir kurulumun
hiçbir servise vermediği bir yetkiyle koşuyordu.

Bu tek cümle beş kusuru birden açıklıyor. Hepsi eksik bir `GRANT`'ti ve
hiçbiri o kurulumda görünemezdi.

**1. Saklama politikası kurulu hiçbir sistemde çalışmamış.** Collector her
açılışta uyguluyor; TimescaleDB ise **yetkiye değil sahipliğe** bakıyor:

```
ERROR:  must be owner of hypertable "traffic_snapshots"
```

`add_retention_policy` üzerinde `EXECUTE` açıkça verilmiş bir veritabanında
ölçüldü. Yani sorun grant değil, sahiplik. Ve site başına kırpma `DELETE`
istiyor — `grants.sql` onu da hiç vermemiş. Sonuç: iki hipertablo da
sonsuza kadar büyüyordu, müşterinin sitesini de sunan makinede. `internal/
retention` paketinin kendi yorumunun "önlemek için var" dediği sonuç tam
olarak bu.

Üstelik `harden.sql`'in yorumu "bu üründe hiçbir şey iş zamanlamaz"
diyordu. Yanlıştı, yazıldığı gün yanlıştı. Bir dosyanın içinden sistemin
geri kalanı hakkında yapılan ve hiçbir şeye karşı doğrulanmayan iddia,
doğru okunan bir `REVOKE`'un bir özelliği nasıl kapattığının hikâyesidir.

Çözüm üç `SECURITY DEFINER` sarmalayıcı (`internal/retention/schema.sql`),
her biri yerine geçtiği yetkiden dar: `ca_set_retention` yalnız iki adlı
tablodan birine, yalnız 1..730 aralığında bir saklama aralığı isteyebilir —
`add_job` keyfî bir fonksiyon adı alır, bu bir aralık alır. İş yine kuran
süperkullanıcıya ait çıkıyor (ölçüldü), yani `harden.sql`'in "bu karar
oraya ait" cümlesi artık gerçekten orada oluyor.

Kalan risk açıkça: ele geçirilmiş bir collector kendi tablosunun saklama
süresini kısaltıp geçmişi yok edebilir. Tavanı aşamaz, diğer servisin
tablosuna dokunamaz, kendi işini zamanlayamaz, tablo düşüremez. Bu artık
zaten her yeni satırın ne diyeceğine karar veren bir süreç, ve alternatif
hiç çalışmamış bir özellik.

**2. Kurulum sihirbazı doğru kurulmuş her sistemde "tablo eksik" diyordu.**
Bunu tek başına bir gösterici kusur sayıyorum: `preflight`, analitik
tabloları `information_schema.tables`'a soruyordu. O görünüm **yalnız
şu anki rolün bir yetkisi olduğu tabloları** listeler. Panel `panel_user`
ile bağlanır ve `traffic_snapshots` üzerinde bilerek hiçbir yetkisi
yoktur — ürünün üstünde durduğu izolasyon budur. Yani kontrol
"Eksik tablo: traffic_snapshots, beacon_events" diyor, zorunlu bir kontrol
başarısız oluyor, ve **başarısız zorunlu kontrol devir teslimi bloke
ediyor.** Geliştirici kurulumu müşteriye hiç veremezdi.

Tuzağın açıklaması aynı dosyada, iki fonksiyon aşağıda, `roleHasPrivilege`
yorumunda zaten yazılıydı. Yazan biliyordu; sorgu yine de öyle yazılmıştı.

**3. `ip_asn_ranges` ve `ip_country_ranges` için hiç `GRANT` yoktu.**
Collector ve beacon ikisi de bu tabloları tazeliyor (`TRUNCATE` + `COPY`)
ve ikisinin de tek bir yetkisi yoktu. Arıza tasarım gereği sessiz —
"failed to set up ASN/country lookup, continuing without it" loglanıp devam
ediliyor, çünkü coğrafyayı kaybetmek trafik yolunu düşürmemeli. Yani her
kurulumda ASN ve ülke sütunları kalıcı olarak boştu ve tek belirti sessiz
bir hafta gibi görünen bir kırılım sayfasıydı.

**4. `install.sh`, DSN verildiğinde `--db`'yi yok sayıyordu.** `psql_db`
`SUPERUSER_DSN`'i olduğu gibi geçiriyordu: betik adlandırılan veritabanını
yaratıyor, boş bırakıyor, ve bütün şemaları, grant'leri ve REVOKE'ları
DSN'in gösterdiği veritabanına uyguluyordu. Sonra başarı bildiriyordu,
çünkü `verify.sql` `current_database()` soruyor — yani yanlışlıkla
sertleştirdiği veritabanını kontrol ediyordu.

Neden görünmedi: buradaki her test `dsnFor(superuserDSN(t), db)` geçiriyor,
yani DSN zaten hedefi adlandırıyor. İkisinin anlaşmazlığa düşemeyeceği tek
düzen. Bir düzine `install.sh` koşusu yanlış olan tek satır hakkında hiçbir
şey kanıtlamamıştı. `sudo -u postgres` yolu hep doğruydu — yani hata elle
kurulmuş bir sunucuda hiç görünmez, yalnız **konteynerin kullandığı yolda**
görünür.

**5. Örnek yapılandırmalarda 8080 çakışması.** Collector'ın `backend_addr`'ı
(müşterinin kendi sitesi) ve okuma API'sinin `listen_addr`'ı ikisi de
`127.0.0.1:8080`. İkisini de değiştirmeyen operatör trafik vekilini
analitik API'sine yöneltir: site her ziyaretçiye JSON cevaplar. En kötü
türden yanlış yapılandırma — iki dosya, her biri kendi başına doğru,
yalnız birlikte okunduğunda yanlış, ki kimse dört yapılandırma dosyasını
yan yana okumaz. `KURULUM.md`'in şeması bile çakışmayı çiziyordu.

### Sonuç: ortamı üretime benzettik

Beş kusurun kökü tekse, çözüm de tek olmalı.

- `docker-compose.yml` artık şema yüklemiyor ve süperkullanıcı `postgres`.
  Geliştirme veritabanı `release/install.sh` ile kuruluyor — dört rol,
  bütün şemalar, grant'ler, sertleştirme, ve iddiayı doğrulamadan bitmeyi
  reddeden bir kontrol.
- CI aynısını yapıyor. Konteynerin süperkullanıcısı artık `collector`
  değil; iş şemaları elle uygulamıyor, kurulumu koşuyor.
- On entegrasyon paketi kodun koştuğu role geçti. `internal/testdb` bunu
  tek yerde tutuyor: `Pool(t, role)` servis rolü, `Admin(t)` şema sahibi —
  DDL ve analitik tablolarından satır silme için, ki hiçbir yazanda `DELETE`
  yok ve olmamalı.

Bu geçiş kendi başına iki şey daha ortaya çıkardı: heartbeat testinin
temizliği hatayı yutuyordu ve doğru kurulmuş veritabanında **sessizce
hiçbir şey yapmıyordu**; sağlık sayfası testi kalp atışını panelin
havuzundan yazıyordu, yani satırın servis adı `panel_user` oluyordu — panel
kendine ayakta olduğunu söylüyordu, collector'a değil. İkisi de yalnız iki
paket aynı rolle bağlandığı için doğru görünmüştü.

### Kalıcı hâle gelenler

- `e2e/e2e_test.go` — zincir, kendi etiketiyle, birleştirme kapısında
  değil. Saklama politikasının gerçekten kurulduğunu da orada doğruluyor:
  kırılan şey `internal/retention`'ın kendi paketinden görünmez.
- `release/ports_test.go` — iki varsayılan aynı portu istemesin, ve panelin
  API'yi gösterdiği port API'nin dinlediğiyle aynı olsun.
- `release/schemalist_test.go` — her şema build.sh'a, install.sh'a ve
  paket içerik listesine ulaşsın. `internal/heartbeat/schema.sql` iki
  listeye girip ikisine girmemişti; kimse fark etmemişti çünkü heartbeat
  testleri tablo yokken atlıyor.
- `release/install_test.go`'da `TestInstallHonoursTheDatabaseItWasGiven` —
  var olmayan test. DSN bir veritabanını, `DB_NAME` başkasını adlandırıyor.
- `verify.sql`'de yedi yeni doğrulama: sarmalayıcılar kurulu, PUBLIC onları
  çağıramıyor, hepsi `search_path` sabitliyor, hiçbir yazanda `DELETE` yok,
  adres aralıklarını iki yazan tazeleyebiliyor, panel onları göremiyor.

Hepsi mutasyonla sınandı: her biri sınamak için var olduğu bozulmada
kırmızıya döndü.

### Bu fazın öğrettiği

**Üretimden daha yetkili bir test düzeneği üretimi test etmez.** Beş
kusurun tamamı aylarca görünmezdi ve hiçbiri ince değil. Yeşil bir paket,
neyin altında koştuğuna bakılmadan okunduğunda bir cümle söylemez.

Ve ikincisi, üçüncü kez: **var olmayan bir deliği uyaran yorum, var olan
delikleri anlatan yorumlara inanmamayı öğretir.** `harden.sql` "hiçbir şey
iş zamanlamaz" diyordu; `preflight` `information_schema` tuzağını iki
fonksiyon aşağıda anlatıp yine ona düşmüştü. İkisi de doğru okunuyordu.

### Altıncı kusur: beacon kendi örnek dosyasından hiç başlamamış

Zincire beacon bacağını eklerken çıktı:

```
beacon: parse .../conf/beacon.toml: toml: line 100 (last key "limits"):
expected a top-level item to end with a newline, comment, or EOF,
but got 'd' instead
```

`beacon.example.toml` **geçerli TOML değildi.** A5.1'de bir paragraf
cümlenin ortasına yapıştırılmış, ikinci yarısının `#`'i düşmüştü:

```
# Bounds this process's own resource use, exactly as the collector's
# # Also moved to the panel in A5.1, ...
[limits] does. Zero or absent means no limit for that dimension.
```

`install.sh` bu dosyayı operatörün dizinine olduğu gibi kopyalıyor. Yani
beacon'ı çalıştıran her kurulum "config error" alıp süreç hiç
başlamıyordu — aylardır.

Neden kimse görmedi: beacon, kendi örnek dosyasından hiç başlatılmamıştı.
Diğer üç servis bir yolla başlatılmıştı, beacon kimsenin koşmadığıydı. Ve
depodaki en ucuz test eksikti: `release/examples_test.go` — dört örnek
yapılandırma, servislerin kullandığı ayrıştırıcıyla, ayrıştırılıyor mu.
Dosyanın *değerleri* mantıklı mı sorusu her servisin kendi config
testinde; *dosya ayrıştırıcının kabul edeceği bir dosya mı* sorusu bu, ve
bir operatöre bir akşam kaybettiren arıza da bu — çünkü hata bir yorum
satırının numarasını veriyor.

Beacon bacağı eklendikten sonra pano artık altı kartın altısında da sayı
gösteriyor, ve test bunu böyle istiyor: bir kartın hâlâ "ölçüm gelmedi"
demesi artık başarısızlık. Zayıf hâli ("bir kartta sayı var") dört beacon
kartı boşken geçiyordu — müşterinin ilk baktığı dört kart.

## Docker: erteleseydik boşa emek olurdu — ve altı şey daha çıktı

Kullanıcının gerekçesi şuydu: dağıtım modeli müşteri başına konteyner,
ve konteyneri sonraya bırakırsak aradaki fazları yanlış varsayımla
yazarız. Ölçtüm, haklıydı — ama beklediğim yerlerden değil.

Kontrol ettiklerim: **kayıtlar** sorun değilmiş (`logging.Config`'te boş
`dir` zaten "yalnız stderr" demek, yani konteyner sözleşmesi bugün de
destekleniyor). **Ortam değişkeni desteği** hiç yokmuş — bütün
yapılandırma TOML dosyalarından geliyor, tek istisna tarayıcı testleri.
**systemd bloğu** zaten `[ -d /etc/systemd/system ]` ile korumalı, yani
konteynerde hiç çalışmıyor.

Asıl mesele bunların hiçbiri değildi. Konteyner, **kimsenin elle
düzeltmediği bir kurulum** olduğu için, altı gerçek kusuru ortaya
çıkardı — ve altısı da konteyner kusuru değil, her yerde kusurdu.

### 1. install.sh GNU sed'e bağımlıymış, ve BusyBox sessizce hiçbir şey yapıyor

```
# alpine'de:
sed -i -E "0,/^#[[:space:]]*key[[:space:]]*=.*/s||key = \"NEW\"|" /tmp/t
# sonuç: dosya değişmedi, çıkış kodu 0
```

`0,/re/s||…|` adres biçimi GNU'ya özgü. BusyBox kabul edip yok sayıyor.
Yani init "ip_hash_key üretildi", "anahtarı collector.toml'a yazdım",
"iki dosyada eşleşiyor" diyor ve **hiçbir şey yazmamış** oluyordu.

### 2. "İki dosyada eşleşiyor" boş dizenin hash'iymiş

Eşitlik kontrolü iki boş değeri karşılaştırıyordu, ve `e3b0c442...` —
boş dizenin SHA-256'sı — kanıt diye ekrana basılıyordu. Bu projenin
kuralı: **yeşil hâli "hiçbir şey yazmadık" anlamına gelebilen bir
kontrol, olmaması gereken bir kontroldür.** Artık boşluk önce
reddediliyor, ve preflight GNU sed yoksa kurulumu hiç başlatmıyor.

### 3. `ip_hash_key` yanlış TOML tablosuna yazılıyormuş

En sinsisi. `privacy.ip_hash_key` collector'ın okuduğu alan; yorum satırı
hâlindeki yer tutucu ise `[retention]` başlığının **altındaydı**. TOML'da
bir başlıktan sonraki her anahtar o tabloya aittir — yani install.sh'ın
yorumu kaldırması anahtarı `[retention]`'a koyuyordu, her kurulumda.

Maskeli modda kimse fark etmiyor. Hash'li modda servis açılmayı
reddediyor, elinde içinde gözle görülür bir `ip_hash_key` bulunan bir
dosyayla. Ve install.sh boyunca "yazdım" ve "eşleşiyor" diyor, çünkü
anahtarı tablo kavramı olmayan bir regex'le arıyor.

### 4. Panelin `secret_key`'i de aynı şekilde

`[developer_gate]`'in seksen satır altında. Panel açılıyor, anahtarı
görmüyor, tek satır log basıp devam ediyor — ve giden posta hesabı
kaydedilemiyor. C7.3'te yazılan bütün mekanizma, hiçbir kurulumda
çalışmıyormuş.

Bunu yakalayan test artık var: her yorumlu yer tutucuyu tek tek açıp
servisin **gerçek Config struct'ına** decode ediyor ve
`MetaData.Undecoded()` boş mu diye bakıyor. Yazdığım anda **üç tane daha**
buldu — ki testin değerini bundan iyi anlatan bir şey yok.

### 5. install.sh, rolleri önceden var olan bir makinede DSN'in veritabanı adını düzeltmiyormuş

`write_role_password` yalnız parola *ürettiğinde* çalışıyor, ve veritabanı
adını da o düzeltiyordu. Rolleri zaten olan bir makinede `--db ca_docker`
ile kurulum, dört dosyada `analytics` yazılı DSN bırakıyordu. Her servis
açılıyor, bağlanıyor, tablo bulamıyor.

### 6. Okuma API'sinin jetonu hâlâ elle kopyalanıyormuş

İki dosyaya giden üçüncü sır: jeton `panel.toml`'a, SHA-256'sı
`analytics-api.toml`'a. `ip_hash_key`'in aldığı bütün özeni almamış.
Örnekler boş jetonla altmış dört sıfırlık bir hash'i eşleştiriyordu, yani
ikisini de değiştirmeyen bir kurulumda panel hiçbir sayı gösteremiyordu.
Artık install.sh üretiyor, ikisine de yazıyor, geri okuyup yeniden
hash'liyor — ve elle yazılmış uyuşmayan bir çifti bulursa kurulumu
durduruyor.

### Ne kuruldu

Tek imaj, beş giriş noktası (`collector`, `beacon`, `analytics-api`,
`panel`, `devpass`) artı `init`. Beş ayrı imaj değil: bu ürün müşteri
başına tek yığın olarak satılıyor ve dört servis aynı şemayı, aynı
`ip_hash_key`'i paylaşıyor. Sürümleri kayan beş etiketin arızası çökme
değil — beacon'ın yazdığı takma adı collector'ın bulamaması, yani ürünün
varlık sebebi olan görünümün sessizce sıfır göstermesi.

Sırlar kalıcı bir birimde, tek seferlik `init` yazıyor. Ve o init aynı
`release/install.sh`'ı koşuyor: konteyner için ikinci bir kurulum betiği
yok, çünkü kimsenin koşmadığı ikinci betik zamanla birincisinden ayrılır —
bu fazın bulduğu altı kusurun tamamı tam olarak bunun kanıtı.

### Konteynerin tersine çevirdiği tek güvenlik kararı

Sunucuda `listen_addr = "127.0.0.1:8082"` kontroldür. Konteynerde
`127.0.0.1` o konteynerin kendisidir — panel bir sonraki konteynerdeki
API'ye ulaşamaz, yani servis korunmuş değil, kullanılamaz olur. Bu yüzden
entrypoint konteyner-içi adrese bağlıyor, ve loopback'in yerini **compose
ağı** alıyor: yalnız collector ve beacon yayımlanıyor.

Bu, gözden geçirmeye bırakılamayacak kadar tek satırlık bir mesafede
duruyor, o yüzden `release/ports_test.go` dosyayı okuyup doğruluyor.
Dosyayı okumak, ayakta bir yığında port taramaktan daha keskin: tarama
"o an bir şey dinlemiyordu" der, okuma müşterinin gerçekten kurduğu şeyi
söyler.

### Kanıt

`e2e/docker_test.go`: imajı derliyor, `docker/compose.yml`'ı ayağa
kaldırıyor, gerçek bir HTTPS isteğini collector konteynerinden geçiriyor,
bir pageview atıyor, ve panonun **altı kartının altısında da** sayı
olmasını istiyor. Geçti.

Tarball E2E'siyle aynı soruyu iki farklı kuruluma soruyorlar, ve bu fazın
öğrettiği tam olarak şu: **iki kurulum aynı soruya aynı cevabı vermiyor,
ve fark her seferinde ürünün lehine değil.**

---

## H4 + B6 — "biri unutunca" ve "üç müşteri, tek makine"

*(2026-08-30)*

İki faz aynı gün yapıldı çünkü aynı şeyin iki yüzü: bir kural N yerde
tek tek hatırlandığı için doğru, ve N+1'inci yer onu hatırlamadığında
kimse fark etmiyor.

### H4 — yapısal değişmezler

`internal/invariants/` üretim kodu içermiyor, hiçbir yerden import
edilmiyor. İçinde yalnız kaynak ağacını okuyup **hiçbir dosyanın kendisi
hakkında söyleyemeyeceği** şeyleri doğrulayan testler var.

Neden var: aynı öğleden sonra bulunan iki delik aynı şekildeydi.

* `internal/api`, `internal/beacon` ve `internal/panel/web` dört
  zaman aşımını da kuruyordu. `internal/fullproxy` hiçbirini kurmuyordu.
* `net/http` üstünde koşan her şey bağlantı başına `recover`'ı bedava
  alıyordu. `internal/proxy` — `http.Server` kullanmayan tek paket —
  almıyordu, yani bir bağlantıdaki tek panik collector'ı, onunla birlikte
  **müşterinin web sitesini** düşürüyordu.

İkisini de bir insan buldu. Bu paket, bir dahakine o insanın orada
olmasına bel bağlamama denemesi.

Dört test var, hepsi **iki yönlü ayna**: bir taraf kaynağın gerçekte ne
yaptığı (sözdizimi ağacı yürüyerek okunuyor), öteki taraf yanına gerekçesi
yazılmış elle tutulan bir liste. Hangi taraf tek başına oynarsa test
düşüyor ve hangisinin oynadığını söylüyor.

Tek yönlü bir tarama — "bulabildiğim her sunucunun zaman aşımı var" —
taramanın tanımadığı bir şekilde sunucu kurulduğu gün sessizce geçerdi.
**Listeyi zorunlu kılan şey, birinin bakmasını zorunlu kılıyor.**

Üç mutasyonla ölçüldü, üçü de yakalandı: zaman aşımını kaldır, `recover`'ı
kaldır, listeye girmemiş yeni bir sunucu ekle.

`internal/proxy/server.go`'daki `ctx.Done()` kapatıcısı meşru bir istisna,
ve dosyada `// no-recover: <gerekçe>` satırıyla yazılı. İstisna mekanizması
gerekçeyi **zorunlu tutuyor** — çıplak bir muafiyet kabul edilmiyor.

### B6 — üç müşteri, tek VDS

Müşterinin kendi cümlesi: *"Tek VDS'te 3 farklı müşteri 3 farklı web
sitesi olabilir ama hepsi ayrı kendi içinde olacak."*

Bugün bu ayrım **tek bir şeye** dayanıyor: panelin kendi üyelik kontrolü.
Panelin tuttuğu API jetonu makinedeki her siteyi okuyor; altında bir
veritabanı sınırı yok ve olması da planlanmıyor. Sınır
`panel_site_members`. Tek kontrole dayanan bir sınırın **kapı başına bir
testi** olmalı.

#### Asıl iddia

"Yabancı sayıları okuyamaz" değil — o zaten iki rota için test ediliyordu.
Keskin olan, ve hiçbir şeyin ölçmediği:

> **Yabancı, var olan bir siteyi var olmayandan ayırt edemez.**

İki handler'da tam olarak bunun için 403 değil 404 döndürdüklerini söyleyen
yorumlar duruyordu. **Hiçbiri kontrol edilmemişti.** 403, farklı bir gövde,
farklı bir uzunluk, tek bir kelime — herhangi biri o URL'yi, makinedeki her
müşteriyi **oradaki herhangi bir hesaptan** sayabilme yoluna çeviriyor;
birine bir öğleliğine verilmiş deneme hesabı dahil.

`internal/panel/web/isolation_integration_test.go` bunu her site-kapsamlı
rota için soruyor ve **durum değil gövde eşitliği** istiyor. Tek kelimeyle
ayrılan iki 404 hâlâ bir orakldır.

Altı mutasyonla ölçüldü, altısı da yakalandı. Üçü kayda değer:

| mutasyon | yakalayan |
|---|---|
| red 403 olsun | durum kontrolü |
| var olana 403, olmayana 404 | durum + eşitlik + gövde |
| **ikisi de 404, gövde bir kelime farklı** | **yalnız gövde eşitliği** |

Üçüncüsü, gövde karşılaştırmasının neden orada olduğunun cevabı.

#### API tarafı: sekiz rota hiç sorulmamış

Aynı soruyu API'ye sorunca çıkan şey plandan büyüktü. `siteHandler`
sarmalayıcısı doğru; risk hiç sarmalayıcıda değildi. Riski taşıyan şey,
bu sınırı test eden iki listenin de **elle yazılmış** olmasıydı:

```
server.go + server_beacon.go 34 site-kapsamlı rota kaydediyor
server_test.go'nun listesi         9
server_beacon_test.go'nun listesi 17
                                  --
hiç sorulmamış:                    8
```

Sekizi — `beacon/titles`, `beacon/refs`, `beacon/click-sources` ve beş
`utm-*` — bugün **doğru davranıyor**. Mesele bu. Doğrular çünkü onları tek
bir `for` döngüsü sarıyor; on altıncı kırılım o döngünün dışında
kaydedildiği gün, ya da biri özel bir durum için döngüden çıkarıldığı gün,
takımdaki hiçbir şey tek kelime etmezdi.

`internal/api/isolation_test.go` bu yüzden liste tutmuyor: kayıtları
kaynaktan okuyor, on beşini üreten döngüyü açıyor, ve **hepsini deniyor.**
Okuyamadığı bir şekilde kaydedilmiş rota sessizce atlanmıyor, testi
düşürüyor — korunmaya çalışılan hata zaten "kimsenin bakmadığı rota".

#### Kendi testimdeki delik

İlk taslak `s.siteHandler(...)` **sarmalayan** kayıtları arıyordu. Bu,
dosyanın kaldırmak için yazıldığı kusurun ta kendisiydi: sarmalayıcısız
kaydedilmiş rota tam olarak aranan hatadır, ve sarmalayıcıya bakmak o
rotayı taramaya görünmez yapıyordu.

Mutasyon testi bunu ortaya çıkardı. Tarama artık **desene** bakıyor — bu
URL bir site kimliği taşıyor, peki başkasının kimliğiyle ne yapıyor? —
ve bu soru kaydın sarmalayıcısını hatırlayıp hatırlamadığından bağımsız
cevaplanabiliyor.

Düzeltilmiş halin mutasyonu:

```
--- FAIL: TestEveryPerSiteRouteRefusesAnotherSitesToken/beacon/click-sources
    reading another customer's site -> 200, want 403.
    This route reached a handler without the token's grant being checked
```

**200.** Gerçek bir müşteriler-arası okuma, adıyla söylenmiş.

#### API neden 403, panel neden 404

İkisi farklı ve fark bilerek: jeton sahibi kendi yapılandırmasını okuyan
bir işletmeci, anonim bir tarayıcı değil. Ona "jetonun bunu kapsamıyor"
demek, yanlış yapılandırılmış bir jetonu eksik bir site gibi göstermekten
iyidir.

Ama bu savunma yalnız 403 **site var olsa da olmasa da aynı** kaldığı
sürece geçerli. Değişseydi, uç nokta panelin olmamak için özen gösterdiği
sayma orakline dönerdi — üstelik geçerli jeton tutan her müşteriye açık.
`TestARefusedRouteSaysNothingAboutWhetherTheSiteExists` bunu 34 rotanın
hepsi için soruyor.

### Bu fazın öğrettiği

Elle yazılmış bir liste yanlış olduğu için değil, **eksildiği** için
tehlikeli. Yanlış liste kırmızı olur; eksik liste yeşil kalır ve
kapsamadığı şey hakkında hiçbir şey söylemez. İki listeye de tek tek
bakılmıştı; ikisinin **birlikte** neyi kaçırdığına kimse bakmamıştı.

---

## L1 — Veritabanı artık kaçıncı sürümde olduğunu söylüyor

*(2026-08-30)*

Bugüne kadar hiçbir şey bilmiyordu. `internal/panel/migrate.go` ayar
göçüdür (TOML→DB), şema göçü değil; binary'ler bilerek DDL çalıştırmaz;
ve açılışta yapılan tek kontrol `Ping`'dir.

### Önce ölçüm, sonra tasarım

Faz yazılmadan önce ölçüldü, çünkü restart'ın gerekli olup olmadığı buna
bağlıydı. Gerçek TimescaleDB:

| durum | sonuç |
|---|---|
| **A** — şema yeni, binary eski | `yazılan=1 hata=nil failed=0` |
| **B** — binary yeni, şema eski | `Ping geçti, açılış sessiz` · `SQLSTATE 42703` · `written=0 failed=3` · tabloda 3'ün **0**'ı |

Durum A, göçün restart istemediğini kanıtlıyor: eski binary tanımadığı
sütunu olan tabloya sorunsuz yazıyor. Durum B ise **sessiz** — süreç
sağlıklı görünerek ayağa kalkıyor ve her satırı kaybediyor.

**Ping, veritabanının cevap verdiğini kanıtlar; şemanın uyduğunu değil.**

### Neden iki değer

İki farklı soru var ve tek alan ikisini birden cevaplayamıyor:

```
Version      "binary veritabanından yeni mi?"   sıralanabilir olmalı
Fingerprint  "uygulanan şey gerçekten bu mu?"   yalan söyleyememeli
```

Tam sayı sıralanabilir ama **yalan söyleyebilir** — biri şemayı değiştirip
sürümü yükseltmeyi unutur. Özet yalan söyleyemez ama **sıralanamaz** — iki
farklı özetten hangisinin yeni olduğu anlaşılmaz.

Tam sayıyı dürüst tutan şey iki yönlü ayna testi: şema dosyaları diskten
okunup yeniden hashleniyor, sabitle uyuşmazsa test düşüyor ve sürümün de
yükseltilmesini istiyor. Şema değiştirip bu testle karşılaşmamak mümkün
değil.

Paket **dosya sistemine dokunmuyor.** `FingerprintOf` içeriği argüman
olarak alıyor, böylece sabit ile yeniden hesaplama gerçekten aynanın iki
yüzü oluyor. Kendi dosyalarını `init`'te hashleyen bir paket kendisiyle
sonsuza kadar uyuşur ve hiçbir şey kanıtlamaz.

### Değer neden SQL'de olamazdı

Parmak izi şema dosyalarının hash'i. İçlerinden birine yazılacak bir
literal, ifade etmesi gereken hash'i değiştirirdi — **hiçbir değer doğru
olmazdı.** Go sabiti hashlenen şeyin dışında duruyor, ve install.sh oraya
`panel -schema-version` ile ulaşıyor.

install.sh iki yarısını da **yazmadan önce** doğruluyor. Boş bir parmak
izi satırı yazar, sağlık sayfası onu çizer, ve hiçbir şemayı tarif etmez —
bu projenin daha önce bir kez ısırıldığı şekil.

### Üç mutasyonun bana öğrettiği

**Birincisi yöntemsel:** M4'te panele `UPDATE` yetkisi verdim, test
**geçti**. Sebep: hiçbir Go dosyası değişmediği için Go testi
önbellekten döndü. **Veritabanı durumu değişiklikleri Go'nun test
önbelleğine görünmez.** Entegrasyon mutasyonu `-count=1` ister; onsuz
harness yalan söyler. `-count=1` ile doğru yakalıyor.

**İkincisi gerçek boşluktu.** Testimin her durumda "yanlış olan cümle
yok" kontrolü tek bir dizeydi ve hep uyarı cümlesini arıyordu. Oysa dört
durumun üçünde bulunmaması gereken cümle **güven veren** olandı, ve onu
kimse sormuyordu. Liste yapıldı.

**Üçüncüsü en öğreticisi.** `Matches()`'ı kayıtsız durumda `true`
döndürecek şekilde bozdum; sağlık sayfası testleri **geçmeye devam
etti.** Sebep: `healthSchema` kayıtsız durumda erken dönüyor ve
`Matches()`'ı hiç çağırmıyor. **Sayfa, yüklemin doğru olmasıyla değil,
akışın oraya uğramamasıyla korunuyordu.**

Yüklem yine de bozuktu, ve onu çağıracak olanlar henüz yazılmamış
olanlar: L2 ilk yazmadan önce, L3 yükseltmeyi önermeden önce soracak.
İkisi de kayıtsız bir veritabanı için `true` alacaktı — yani *sessizliği
mutabakat sayacaktı.*

Durum makinesi bu yüzden kendi biriminde ayrıca test edildi. Kırılan
branch'a uğramayan bir sayfadan geçen yeşil, güvenilecek yeşil değil.

### Kapsam dışı bırakılan

`applied_by` bugün `install.sh` yazıyor. L3 geldiğinde yükseltme
uygulayıcısı olacak, ve `GRANT UPDATE` o zaman **bu satırın yanına
eklenecek**, onu değiştirerek değil. Sahiplik kararı tablo yaratılırken
verildi; sonradan verilseydi `grants.sql`, `verify.sql` ve `install.sh`
ikinci kez elden geçerdi.

---

## CI her push'ta kırmızıydı — ve sebebi üç kez aynı şekildi

*(2026-08-30)*

Kullanıcı bildirdi: *"github habire run failed veriyor her güncellemede."*
Sebep tek satırdı ve betiğin en sonundaydı:

```
== systemd units
install: cannot create regular file
  '/etc/systemd/system/crucible-analytics-api.service': Permission denied
```

Üç kusur çıktı, üçü de **kodun sahip olmadığı bir özelliği iddia eden
bir kontrol ya da yorum**.

### 1. Muhafız yanlış soruyu soruyordu

```sh
if [ -d /etc/systemd/system ]; then     # "dizin var mı"
```

Sorulması gereken *"yazabiliyor muyum"*. Dizin her Ubuntu makinesinde
var — CI runner'ında da. Orada olmayan şey root.

### 2. Betik işin %90'ını yapıp sonra ölüyordu

Dört rol, yedi şema, dört yapılandırma dosyası, üç sır — hepsi
üretildikten *sonra*. Root olmadan çalıştıran müşteri **yarım kurulmuş
bir makineyle** ve ne yapacağını söylemeyen bir izin hatasıyla kalıyordu.

Karar artık preflight'ta, hiçbir şey yaratılmadan önce. Aynı gerekçeyle
şema sürümünü okuyacak binary/Go kontrolü de oraya taşındı — o da kendi
aşamasında bulunsaydı şemalar çoktan uygulanmış olurdu.

### 3. Test takımının yorumu doğru değildi

`runInstall`'ın yorumu *"buradaki hiçbir şey makinenin systemd'sine ya da
kullanıcılarına dokunmaz"* diyordu. `LOG_DIR` ile `STATE_DIR`'i
yönlendirmek iki dizin taşır, başka bir şey yapmaz.

Ölçüldü — bu makinede tam olarak şunlar duruyordu:

```
/etc/systemd/system/crucible-analytics-api.service
/etc/systemd/system/crucible-beacon.service
/etc/systemd/system/crucible-collector.service
/etc/systemd/system/crucible-panel.service
uid=996(crucible) gid=995(crucible)
```

Her koşuda, o yorumun yazmadığını söylediği şeyi yazan bir takım
tarafından. Geliştirici makinesinde sessizce başarılı oluyor; CI'da
başarısız oluyor — **fark edilme sebebi buydu.**

İddia artık yorumda değil testte: `TestNoSystemdWritesNoUnitFiles` hem
birim dosyası sayısının değişmediğini hem de betiğin *atladığını
söylediğini* kontrol ediyor. Sessizce atlanan adım, koşan adımdan ayırt
edilemez.

### Bir de israf: her push iki tam koşu

Eşzamanlılık anahtarı `github.ref`'ti, ve o iki olay için iki farklı
dize: push `refs/heads/x`, pull request `refs/pull/N/merge`. PR açıkken
her push **iki tam koşu** başlatıyordu, her biri kendi TimescaleDB'sini
ayağa kaldırarak. `head_ref || ref_name` ikisi için de aynı dize.

### Mutasyon: kusuru talep üzerine geri getirmek

Root kontrolü kaldırıldığında **orijinal CI hatası birebir geri geldi**,
ve `ca_m3` veritabanı ortada kaldı — tarif edilen yarım kurulmuş
makinenin kendisi. Bir düzeltmenin gerçekten düzelttiğinin kanıtı, onu
kaldırdığında arızanın aynı cümleyle dönmesi.

### Kendi hatam, iki kez

Mutasyon harness'ım `git checkout -- <dosya>` ile geri alıyordu. İzlenen
ama **commit'lenmemiş** bir dosyada bu, mutasyonu değil *düzeltmeyi*
siliyor. Bir kez `tr.toml`'da oldu, sonra `install.sh`'ta tekrar — ikinci
seferinde M2 ve M3 sonuçları geçersizdi ve fark etmem birkaç tur sürdü.

Kural: **önce commit, sonra mutasyon.** Temiz bir taban olmadan geri alma
güvenilir değil.

---

## L2 — Sessiz kayıp yerine başlamayan servis

*(2026-08-30)*

L1'de ölçülen sessizlik kapandı. O ölçüm şuydu: bir sütunu eksik olan
collector ayağa kalkıyor, `Ping`'i geçiyor, sağlıklı görünüyor, ve eline
verilen her satırı kaybediyor — `written=0 failed=3`, tabloda üçün sıfırı.

**Ping veritabanının cevap verdiğini kanıtlar; cevabın hangi şekilde
olduğunu değil.** Şekil artık açılışta bir kez doğrudan soruluyor.

Aynı deney, `asn_org` düşürülmüş gerçek bir veritabanında, gerçek
`storage.NewWriter` ile:

```
AÇILIŞ REDDEDİLDİ: traffic_snapshots is missing 1 column(s) this build
writes: asn_org. ... Refusing to start rather than writing rows that
would be dropped.
```

Başlamayan bir collector, dakikalar içinde onarılan bir arıza. Başlayıp
satır düşüren bir collector, sayılar sorulana kadar kimsenin görmediği
bir arıza — ve o noktada trafik gitmiş olur, kurtarılacak bir şey kalmaz.

### Asimetri ürünün kendisi

Eksik sütun ölümcül, **fazladan sütun değil** — ve asla olmamalı.
Fazladan sütun, doğru bir yükseltmenin içinden geçtiği durumdur: şema
önce gider, binary'ler sonra, ve arada çalışan her binary hiç duymadığı
sütunları olan bir tabloya bakar.

Aynı veritabanına iki fazladan sütun eklendikten sonra:

```
açılış: kabul edildi
yazılan=1 hata=<nil>  written=1 failed=0
```

L1'deki Durum A ölçümüyle birebir aynı sayı. Bunu ölümcül yapmak,
yükseltmenin **güvenli yarısını kesintiye çevirir** ve veri kaybettiren
sırayı dayatır.

### Kendi kuralımı yine unuttum

Mutasyon turunda `pg_catalog` yerine `information_schema` koydum ve
**bütün testler geçti.** Sebep tanıdıktı: testlerim süperkullanıcı
olarak koşuyor, ve süperkullanıcı iki katalogda da her sütunu görüyor.

Bu projenin en eski dersi: **üretimden daha yetkili bir test düzeneği
üretimi test etmez.** Bu oturumda üçüncü kez.

Sonra ölçtüm, ve ölçüm yorumumun **abartılı** olduğunu gösterdi:

```
rol            tablo               information_schema   pg_catalog
collector      traffic_snapshots                   14           14
beacon_writer  traffic_snapshots                    0           14
panel_user     traffic_snapshots                    0           14

collector      traffic_snapshots (kendi)           14
beacon_writer  beacon_events     (kendi)           30
```

Yani **bugünkü iki çağrı noktasında fark yok** — her yazıcının kendi
tablosunda grant'i var. Yorumu buna göre düzelttim; olmayan bir deliği
uyaran yorum, olan delikleri uyaran yorumlara inanılmamasını öğretir.

Ama farkın gerçekten önemli olduğu bir durum var ve o test edilebilir:
**yapılandırma dosyasında yanlış rolü gösteren DSN.** `pg_catalog` ile
cevap sütunlar hakkında olur; `information_schema` ile *"bu tablo yok"*
olur — var olan bir tablo hakkında, ve okuyanı zaten uygulanmış bir
şemayı yeniden uygulamaya gönderir.

Test o rolü `testdb`'den türetiyor, kendi ortam değişkeninden değil:
kimse bir değişkeni kurmadığında atlanan test, yanlış sebeple yeşildir.
Mutasyon artık ölüyor.

---

## CI'ın ikinci kırmızı sebebi, ve benim yaptığım kötüleştirme

*(2026-08-30, aynı gün, birincisinden sonra)*

`install.sh`'ın root varsayımını düzelttikten sonra CI **hâlâ**
kırmızıydı. Sebep bağımsız bir ikincisiydi ve daha eskiydi:

```
--- FAIL: TestConnectionEncryptionPassesLocally
    a loopback connection gave warn: Veritabanı uzak bir sunucuda
    (172.18.0.2) ve bağlantı şifresiz...
```

**Kontrol doğruydu, test yanlış varsayıyordu.** Testin yorumu *"bu
takımın bağlantısı localhost'a"* diyordu — belirtilmiş, hiç kontrol
edilmemiş bir varsayım. CI'da veritabanı bir Docker servis konteyneri,
yani `172.18.0.2`. Kontrol doğru uyarıyor, test doğru düşüyordu.

Bu, bugün tekrar tekrar çıkan şeklin ters yönü: **düzeneğin varsaydığı
ortam, koştuğu ortam değil.**

### Neden fark edilmedi

Üç cevaplı bir kontrolün bir dalı **geliştirici makinesinde
ulaşılamaz**: başka bir makinedeki veritabanı gerekiyor, ve yerel
PostgreSQL çalıştıran bir dizüstünde öyle bir şey yok. Yani o dal yalnız
CI'da koşuyordu — doğru davranıyordu, ve *onu test eden şey* yanlış
davranıyordu.

Karar saf bir fonksiyona (`encryptionVerdict`) çıkarıldı. Artık yedi
durum da her makinede test ediliyor: TLS'li uzak, TLS'li yerel, unix
soketi, loopback, IPv6 loopback, **şifresiz uzak (CI'ın durumu)**, ve
ayrıştırılamayan adres.

Sonuncusu bilerek: `host()` böyle bir şey vermemeli, ama okunamayan bir
adres *sessizce yerel* sayılmamalı — açık tarafa düşen yön odur.

### Düzeltmeyi düzeltmek

İlk turda eşzamanlılık anahtarını dal adına çevirmiştim, ki push ve
pull_request koşuları tek koşuya insin. **Daha kötü oldu.**
`cancel-in-progress` tekilleştirmez, *iptal eder* — ve pull_request
koşusu yaklaşık iki saniye sonra başladığı için her seferinde push
koşusunu öldürdü. Ölçüldü: arka arkaya üç commit'in push koşusu
`cancelled`.

**İptal edilmiş koşu, mükerrer koşudan kötüdür.** Hiçbir şey raporlamaz,
ve dala şöyle bir bakan biri için başarısızlık gibi görünür.

İkisi artık kendi gruplarında. Aynı olay içinde yeni push eskisini yine
iptal ediyor — istenen buydu. Mükerrer koşu kalıyor, ve israf değil:
`pull_request` birleşme commit'ini, `push` dalın ucunu derliyor, ve taban
kımıldadığı anda ikisi ayrışıyor.

### Bu turun dersi

Bir kırmızıyı düzeltip yeşili doğrulamamak, düzeltmemekle aynı şey.
Birinci sebebi kapattım ve "CI düzeldi" demeye hazırdım; ikinci sebep
oradaydı, ve benim düzeltmem üçüncü bir belirti üretmişti.

---

## D4a — Ayar sayfası: üç fazı bekleten tek eksik

*(2026-08-30)*

Arka ucun tamamı hazırdı ve hiç yüzeye çıkmamıştı:

| var olan | ne yapıyordu |
|---|---|
| `SettingsView` | her ayarı değeri, erişim seviyesi, kilit gerekçesi ve kaynağıyla döndürüyor |
| `ApplySetting` | yetki → önkoşul → kapı → yazma → denetim kaydı, bu sırayla |
| `SettingAccess` | writable / gated / locked / read_only |

Eksik olan sayfaydı, ve o eksik **A2, B3 ve L3'ü birden** bekletiyordu.

### İki karar

**Sayfayı izleyici de görüyor** — üyeler sayfasının tam tersi, ve
bilerek. Her satır değeriyle ve kontrol yerine gerekçesiyle çiziliyor.
Müşterinin kendi kurulumunun neye ayarlı olduğunu öğrenmek için birine
sorması gerekmesi kabul edilebilir değil. Yazmayı reddeden şey sayfa
değil, sunucu.

**Bir ayar bir gönderim.** Yirmi ayarı birden kaydeden bir form yirmi
sonuç bildirmek zorunda kalırdı, ve başarısız olan kimsenin okumadığı
olurdu.

B6'nın aynası yeni kapıyı **anında** yakaladı ve listeye eklenmesini
istedi — o test bunun için yazılmıştı. Eklendikten sonra yalıtım
testleri ilk denemede geçti.

### Test yazarken çıkan üç kusur

**Parola eksikken cevap 200 dönüyordu.** Ayar değişmiyordu ama durum kodu
"tamam" diyordu. Durum kodu betiğin, ekran okuyucunun ve erişim kaydının
ilk okuduğu şey; hiçbiri cümleyi okumuyor.

**Doğrulayıcının İngilizce iç mesajı Türkçe panele geçiyordu** —
`must be between 1 and 3650, got 999999`. Sınır zaten tanımda; mesaj
artık tanımdan Türkçe kuruluyor, ham Go hatası yalnız loga gidiyor.

**TOML tuzağı yine ısırdı.** Regex ilk `gecersiz` satırını buldu, o da
giriş hataları tablosundaydı: üç ayar anahtarı yanlış tabloya girdi *ve*
giriş mesajı ezildi. Bu oturumda ikinci kez, projede en az üçüncü.

### Mutasyon turu: üç kusur daha, biri testin kendisinde

**Kapsam mutasyonu hayatta kalmıştı** çünkü testim `site=""`
gönderiyordu — boş değer iki halde de doğru siteye düşüyor. Saldırı
*dolu* bir değer. Yeni testle birlikte mutasyon gerçek bir
müşteriler-arası yazma gösteriyor:

```
a form wrote into another site's row: ayar-komsu went '' -> 'komsunun satirina yazildi'
```

Kod doğruydu; **onu doğrulayan bir şey yoktu.**

**Eylem yönlendirme mutasyonu başka sebeple hayatta kaldı: yorumum
yanlıştı.** Doğru parolayla yazdığım test *düzeltilmemiş kodda* düştü —
parola doğru, ayar doğru, fazladan alanlar zaten yok sayılıyor; ortada
reddedilecek bir şey yok. Ulaşmaya çalıştığı iddia gerçek ama zaten
kanıtlanmış, ve doğru yerde:
`TestGuardedSettings_AnAuthorizationForOneSettingDoesNotWriteAnother`.
`SetGuardedSetting` yetkiyi yazacağı anahtara karşı kontrol ediyor.

**Handler'daki türetme ikinci katman, birinci değil** — yorum düzeltildi,
test doğru iddiaya çevrildi.

### Olumlu testin ortaya çıkardıkları

Korumalı yol hiç çalışmıyor göründü. Sebep kodda değildi: **korumalı
ayarlar `Superadmin` istiyor**, ve o bayrak üretimde yalnız *sahibin
onayladığı* geliştirici bağlantısının kullanılmasıyla geliyor —
`CreateUser`'ı `true` ile çağıran hiçbir üretim yolu yok. Yani müşterinin
kendine veremeyeceği tek şey.

Model kullanıcının tarif ettiği gibi çalışıyor: *geliştiriciye iş
çıkarabilen şey yetkiyle korunamaz.*

İkinci bulgu bendeydi: `settingErrorText` **sınıflandıramadığı her
hatayı doğrulama hatası gibi gösteriyordu.** `ip_hash_key` olmayan bir
kurulumda `privacy.ip_storage=full` reddedilince ekran şunu diyordu:

> Değer şunlardan biri olmalı: full, masked

— zaten onlardan biri olan bir değer hakkında. **Okuyanı doğru olan şeyi
kontrol etmeye gönderen mesaj, belirsiz olandan kötüdür**: bakar, bir şey
bulamaz, ve sayfaya inanmayı bırakır. `ErrPreconditionUnmet` artık kendi
cümlesini alıyor, ve onu koruyan bir test var.

### Test kirliliği

Testlerim genel bir ayarı yazıp bırakıyordu, ve **üç paket ötede**
`TestSettings_DefaultsApplyBeforeAnythingIsStored` düşüyordu. Site
kapsamlı satırlar sorun değil — testin uydurduğu site kimliğiyle
anahtarlanıyorlar — ama genel satır her şeyi paylaşıyor. `restoreGlobal`
ile temizleniyor.

### Bu fazın öğrettiği

Olumlu test olmadan bütün "doğru reddediyor" testleri yeşil kalırdı ve
özellik hiç çalışmazdı. **Bir şeyin reddettiğini kanıtlamak, çalıştığını
kanıtlamaz.**

---

## B1+B2 — Bir sütun iki fazı birleştirdiği zaman

B1 (`panel_logs`) ile B2 (`panel_operations`) planda ayrı iki fazdı.
Ayrı yapılamazlar, ve sebebi tek bir sütun: `panel_logs.operation_id`
içindeki değer `panel_operations.id`'dir.

Sırayla denemenin iki hâli de kötü:

- Önce B1: sütun kimsenin dolduramadığı boş bir söz olur, ve boş bir
  sütun "henüz kullanılmıyor" ile "yanlış dolduruluyor" arasında ayrım
  yapmaz.
- Önce B2: kimliğin bağlanacağı satır yoktur, yani kimliğin işe yarayıp
  yaramadığı ancak B1 geldiğinde anlaşılır — hata bulunduğunda B2 çoktan
  "bitmiş" sayılmıştır.

Bir sütun iki tablonun arasındaysa, o iki tablo tek bir fazdır.

### Veritabanı asla istek yolunda değil

`internal/proxy` saldırgan baytlarına dokunup müşterinin kendi sitesine
iletiyor. PostgreSQL'i bekleyen bir kayıt yazımı ziyaretçiyle site
arasına bir veritabanı gidiş-dönüşü koyardı, ve **yavaşlayan veritabanı
yavaşlayan web sitesi** olurdu.

Bu yüzden tamponlu kanal, tek yazıcı goroutine, ve tampon dolunca
düşürme. Düşürmek doğru davranış: alternatifi tam da önemli olan anda
daha kötü — yük altındaki servis, en çok satır üreten ve en az
bloklanması gereken servistir.

Olmaması gereken şey *sessizce* düşürmek. Düşen sayılıyor, ve sağlık
sayfası sayacı okuyor: "panelin kaydında bir saat eksik" cevabı olan bir
soru.

Ölçüldü: tampon 1, 5000 satır, **2.7 ms**. Aynı kod bloklamaya
çevrildiğinde **1.88 saniye**.

### rolled_back üç durumlu, ve bu fazın asıl alanı

Planın kendi vurgusu: *son alan en önemlisi. "Bir şeyi ayarlarken hata
olmuş" ancak yarım uygulanmış bir değişiklik yarım uygulanmış olarak
kaydedilirse cevaplanabilir.*

- `true` — uygulandı, sonra geri alındı
- `false` — uygulandı, olduğu gibi duruyor
- `NULL` — hiçbir şey uygulanmamıştı, geri alınacak bir şey yoktu

`NULL`'u `false`'a çökertmek, hiç yapılmamış bir değişikliğin geride
bırakıldığını iddia etmek olurdu — bu tablonun cevaplamak için var
olduğu tek sorunun yanlış cevabı.

Sonuç da üç değerli, aynı sebeple: `refused` bir arıza değil, sistemin
tasarlandığı gibi çalışması. Reddedilmeleri arızaların arasına gömmek,
gerçek bir arızanın uzun bir listede gözden kaçma yoludur.

### İşlem kimliği neden Go'da üretiliyor

Bütün amaç kimliği işlemin *birazdan üreteceği* satırlara iliştirmek.
Veritabanının atadığı bir kimlik ilk yazmaya kadar var olmaz, ve o zamana
kadar ilginç satırlar kimliksiz çıkmıştır.

Rastgele, sıralı değil: kimlik müşterinin görebildiği bir satıra ulaşıyor
ve bir sayaç kurulumun şimdiye kadar kaç işlem çalıştırdığını söylerdi.
Küçük bir sızıntı, ama yalnızca benzersiz olması gereken bir değer için
kabul etmenin hiçbir sebebi yok.

### Elle ölçüm kapı değildir

Satır düzeyi politikalarını psql isteminde elle ölçtüm, çalıştıklarını
gördüm, ve teste bağlamadan geçtim sandım. Testler `CA_SUPERUSER_DSN`'den
tek havuz açıp bütün satırları `postgres` adına yazıyordu — ve
**PostgreSQL süper kullanıcıyı satır düzeyi güvenlikten tamamen muaf
tutuyor**. Üstelik yazma politikası `service`'i `current_user` ile
karşılaştırdığı için, postgres adına postgres olarak yazmak politikayı
kazara sağlıyordu.

Dört test de yeşildi ve hiçbiri bir politikayı sınamıyordu.

`internal/testdb` tam olarak bunu bitirmek için yazılmıştı; paket yorumu
ilk seferinde gizlediği üç gerçek hatayı sayıyor. Kuralı yine unuttum ve
kendi paketimde yeniden kurdum. **Üretimden daha yetkili bir test
düzeneği üretimi sınamıyor** — bu oturumda dördüncü kez.

Şimdi iki havuz: collector yazıyor, panel okuyor. Üretimdeki şekli bu —
collector yalnızca INSERT tutuyor, kendi satırlarını geri okuyabilen bir
sink hiçbir kurulumun vermediği yetkiyle çalışıyor olurdu.

### Süpürme politikası ölçülerek taşıyıcı çıktı

`panel_logs_sweep`, zaten izin veren bir politikası olan tabloya ikinci
bir politika — okununca fazlalık gibi duruyor. Ölçüldüğünde değil: o
politika olmadan `panel_user`'ın `DELETE`'i **1 satır** sildi (kendi
satırını) ve collector'ınkini bıraktı.

İzin veren politikalar OR'lanıyor, ama `FOR ALL` yazma politikası çapraz
rol `DELETE`'e izin vermiyor — kendi `FOR DELETE` politikasını
gerektiriyor. Bu, çalışıyor gibi görünüp çalışmayan bir saklama işinin
şekli, ve budayamadığı tablo büyümesi disk dolma hatası olan tablo.

### Mutasyon turu: dokuz mutasyon, biri hayatta kaldı

Hayatta kalan: `errorChain`'in bütün gövdesini `return err.Error()` ile
değiştirmek bütün testleri yeşil bıraktı.

`operations.go`'nun yorumu bunun bilinçli bir karar olduğunu söylüyor —
"zincir son halkası olarak değil bütün olarak saklanıyor, çünkü en içteki
sebep çözümü adlandıran halkadır" — ve hiçbir şey onu korumuyordu.
Koruyor sandığım entegrasyon testi zincirin `"3650"` içerdiğine
bakıyordu, ve **en dıştaki halka onu zaten taşıyordu**: iddia tek
halkalık bir zincirde de geçiyordu.

Bu, bu oturumda tekrar eden bir şeklin bir başka örneği: *iddia doğru
olan bir şeye bakıyordu, ama yanlış olabilecek şeye değil.*

Yeni test deponun kendi `wrapStoreError`'ıyla kuruluyor ki üretimdeki
sarma şekli değişince test de değişsin. Sayılan şey **satır sayısı**:
iki halka iki satır demek, ve `Contains` iddiaları tek satırda da geçer
çünkü `fmt.Errorf`'un `%w`'si sebebin metnini dış hatanın içinde bırakır.

### Neredeyse boş bir eşik

Sink'i bloklamaya çevirmek `TestTheSinkDropsRatherThanBlocks`'u düşürdü —
ama kararı veren `dropped == 0` iddiasıydı. Zamanlama iddiası 2 saniyeydi
ve bloklama 1.88 saniye sürdü.

Yani tavan, yakalaması gereken hatanın **hemen üstünde** duruyordu. Daha
hızlı bir veritabanında altından geçerdi ve geriye tek iddia kalırdı.
500 ms'ye çekildi: doğru davranışın ölçülen maliyeti 2.7 ms, hâlâ 150 kat
pay var.

**Geçen bir mutasyon testi, dar farkla geçen bir mutasyon testiyle aynı
şey değil.** Mutasyonun kırmızıya döndüğünü görmek yetmiyor; hangi
iddianın döndürdüğüne ve ne kadar payla döndürdüğüne bakmak gerekiyor.

### B6'nın ertelenmiş maddesi hâlâ ertelenmiş, ama başka yere

B6 `panel_logs` filtresi testini "B1'e bağlı" diye bırakmıştı. Tablo
artık var — ama ertelemenin gerçek sebebi parantez içindeydi: *log
sayfası henüz yok*. O sayfa D4b. Filtreyi sınayacak sorgu henüz
yazılmadı, ve olmayan bir fonksiyonun yalıtımı sınanamaz. PLAN'da
bağlılık D4b'ye taşındı.

---

## B1'i erken ✅ işaretlemişim — yazıcı fişe takılı değildi

Fazı kapattıktan sonra "okuma yüzeyi yok" diye bıraktığım maddeye
bakınca çıktı: dört `main.go` da `logging.Setup` ile ağaç logger'ını
kuruyordu, ve `logsink` `cmd/` içinde **hiç geçmiyordu**.

Yani gerçek bir kurulumda `panel_logs` kalıcı olarak boştu. Tablo,
grant, satır düzeyi politikaları, altı entegrasyon testi, yazıcı — hepsi
gerçek ve hepsi doğru, ve hiçbir şey yazmıyor. README'ye "the writers
are in place and every setting change is recorded" yazmıştım; operasyon
yarısı doğruydu, log yarısı üretimde hiç ateşlenmiyordu.

**Bitmiş bir faz, çalışan bir sistem değildir.** Bir bileşenin doğru
olduğunu kanıtlayan bütün testler, o bileşenin *çağrıldığını*
kanıtlamaz.

### Araştırınca daha büyüğü çıktı

Süpürmeyi nereye koyacağıma bakarken deponun kendi emsallerine baktım:

```
PurgeOldLoginAttempts (90 gün) — hiçbir yerden çağrılmıyor
PurgeOldDevAccess     (30 gün) — hiçbir yerden çağrılmıyor
```

İkisi de yazılmış, belgelenmiş, doğru, ve testler dahil hiçbir yerden
çağrılmıyor. Sınırladıkları tablolar yazıldıkları günden beri sınırsız
büyüyor, ve bütün takım bu süre boyunca yeşil.

Sebep tek ve yapısal: **panelin hiç periyodik işi yoktu.** collector ile
beacon'ın saklama ticker'ı var, o yüzden onlar için yazılan bir
süpürmenin çağrılacağı bir yer var. Panel için yazılanın yoktu. Üçüncü
bir süpürme eklemek üçüncü ölü kodu üretecekti — o yüzden önce giriş
noktası, sonra süpürmeler.

### Bir süpürmenin kusuru dönüş değerinden görünmez

Hiç silmeyen ve her şeyi silen iki süpürme de aynı şeyi döndürür: bir
satır sayısı ve sorunsuz bir `nil`. Bu yüzden testler sınırın **iki**
yanına satır ekiyor ve hangisinin sağ kaldığını soruyor; çağrının
başarılı olduğunu değil.

### Mutasyon turu: on mutasyon, üçü hayatta kaldı

**1. Kendi yapısal testim, korumaya çalıştığı kusurun bir alt katmanına
kördü.** `TestHousekeepingIsCalledBySomething` main.go'da
`".Housekeeping("` dizesini arıyordu. `go runHousekeeping(...)`
satırını sildim — test yeşil kaldı, çünkü o dize `runHousekeeping`'in
*kendi gövdesinde* de geçiyor. Yani süpürme döngüsü tanımlı, eksiksiz
ve hiç başlatılmamış bir panelde test geçerdi.

Bu, bu oturumda dördüncü kez aynı şekil: *iddia doğru olan bir şeye
bakıyordu, yanlış olabilecek şeye değil.* Artık AST'den çağrı zinciri
yürüyor ve iki halkayı da ayrı ayrı soruyor.

**2. İki koruma aynı özelliği tutuyordu, o yüzden hiçbiri tek başına
ölçülemiyordu.** `logging.Tee` her çocuğun `Enabled`'ını soruyor, ve
`logsink.Handle` kendi seviyesini yeniden kontrol ediyor. Birini
kaldırınca davranış değişmiyor — diğeri hâlâ tutuyor. İkisi de tek tek
hayatta kaldı, ve bu doğru: korunması gereken şey korumalar değil,
**bileşim**. İkisini birden kaldıran mutasyon `TestAnInfoLine...`'ı
düşürüyor.

Bundan çıkan kural: *bir mutasyon hayatta kaldıysa, önce testin mi
yoksa mutasyonun mu anlamsız olduğunu sor.* Yedekli iki koruma için tek
tek mutasyon anlamsızdır; ölçülecek şey özelliktir.

**3. Yorumum tersini söylüyordu ve test onu kopyalamıştı.**
Operasyon süpürmesini `finished_at`'e çevirdim — geçti. Çünkü
`NULL < now() - interval` **NULL**'dır, `true` değil: yani `finished_at`
anahtarlı bir süpürme bitmemiş operasyonları erken silmez, **hiç
silmez**. Yeniden başlatmayla kesilen her operasyon tabloda sonsuza
kadar kalır.

Testim yalnızca *yeni* bitmemiş satır koyuyordu, o da her iki halde de
sağ kalıyor. Eksik olan **eski ve bitmemiş** satırdı. Yorumu da testi de
düzelttim.

### Yakalanan yedi mutasyon

Housekeeping'in bir süpürmeyi çağırmayı bırakması; main'in döngüyü
başlatmaması; döngünün süpürmeyi çağırmaması; süpürme sınırının ters
çevrilmesi; `PanelLevel`'ın WARN tabanının kaldırılması; ayrıntılı
anahtarın tabloya ulaşmaması; servis adının yanlış çözülmesi (RLS
reddediyor).

---

## "Yıllarca fark edilmeyebilir" sınıfını sistematik aramak

Ölü kodu iki kez tesadüfen bulmuştum. Aynı sınıfı tarayınca daha çıktı,
ve tarama artık kapıda.

### Neden bu sınıf özel

**Ulaşılmayan kod başarısız olamaz.** Bir süpürme çağrılmıyorsa hata
vermez, tablo büyür, takım yeşil kalır. İlk belirti dolu bir disktir.
Hiçbir test bunu göremez, çünkü test kodun *doğru* olup olmadığını
sorar; buradaki soru *çalışıp çalışmadığı*.

`deadcode` bir kez elle koşturulunca üç gerçek kusur çıktı — ikisi
zaten bildiğim süpürmeler, üçüncüsü benim `LinkAudit`'im.

### LinkAudit: çağrılamaz bir metot

`panel_operations.audit_id` oluşturulduğu günden beri her satırda NULL.
`Operation.LinkAudit` doğru yazılmış ve **çağrılması imkânsız**: denetim
yazımı yalnızca `error` döndürüyordu, yani ihtiyacı olan id hiçbir
çağıranın erişebileceği yerde yoktu.

Bu ölü kod değil, B2'de bıraktığım bir eksik. Bağlandı: `Record`'un
yanına id döndüren bir yol eklendi (otuz çağıranın bir değeri yok
saymaya başlamaması için ayrı giriş noktası), `ApplySetting`/
`ClearSetting` operasyonu parametre olarak alıyor ve bağlamayı id'nin
var olduğu tek yerde yapıyor. Testi de var artık.

### CSRF testi kısaydı, ve kısalık yeşil kalır

`TestEveryPostRouteRefusesARequestWithNoToken` elle yazılmış on rota
tutuyordu. `acceptPost` çağıran on üç yer vardı: kurtarma formu, posta
sayfası ve D4a'da benim eklediğim ayarlar sayfası hiç sorulmuyordu.

Yani **unutulan rotayı yakalamak için var olan tek test, üç rotayı
unutuyordu.** B6'da tam aynısı olmuştu. Artık liste tutmuyor: hangi
handler'ın `acceptPost` çağırdığını kaynaktan okuyup `server.go`'nun
kaydettiği desenle eşliyor.

### Ama asıl soru sorulmuyordu

Yukarıdaki test korunan rotaların reddettiğini kanıtlıyor. Göremediği
şey daha önemli: **hiç `acceptPost`'a uğramayan bir handler.** Öyle bir
handler bütün CSRF testlerini geçer — hiçbirinde olmayarak.

`TestEveryRouteIsEitherGuardedOrDeliberatelyNot` iki yanı da yazıyor:
bir yan kaynaktan, öbür yan gerekçeli muafiyet listesi. `devAccessHandler`
gerçekten korumasız ve bu doğru — tıklanan tek kullanımlık bir bağlantı,
jetonun kendisi yetki — ama artık bu bilinçli bir kayıt, sessiz bir
boşluk değil.

`safeMethod` de buradan çıktı: hiç çağrılmayan, "GET'ler muaf" diyen bir
yardımcı. Var olmayan bir ara katmanın kanıtı gibi okunuyordu, ve buna
inanan biri yeni bir POST handler'ı yazıp korunduğunu sanabilirdi.
Silindi, yerine neden olmadığını anlatan bir yorum kondu.

### İki kayıt, birbirini tutması gereken

`web.technicalLists` ile `analytics.addressLists` aynı kümeyi tutmak
zorunda. `analytics.KnownAddressList` tam da bunu korumak için yazılmış
("panel bilinmeyen bir segmenti URL'ye ulaşmadan reddedebilsin diye") ve
hiçbir yerden çağrılmıyordu — sürüklenmeye karşı yazılan muhafızın
kendisi sürükleniyordu. Ayna testi ikisini bağladı.

### Yanlış yorumlar

İkisi de "var olmayan bir şeyi var diye anlatan" cinsten:

- `PurgeOldOperations`: yorumu `finished_at`'in etkisini **tam tersi**
  anlatıyordu. Testi düzeltmiştim, kaynağı düzeltmemişim.
- `logging.Deadline`: "query ve access kategorileri kullanıyor"
  diyordu. Hiçbir şey kullanmıyor.

**Var olmayan çağıranları sayan bir yorum, okuyana bir sonrakine daha az
güvenmeyi öğretir.**

### Kapı: gerekçeli muafiyet listesi

Bir kez elle koşturulan tarama, bir sonraki sefere kadar hiçbir şey
korumaz. `internal/sast/cmd/deadcodediff` gosec'in `sastdiff`'iyle aynı
şekilde çalışıyor ve CI'da: rapor ile `deadcode_allowlist.txt`
karşılaştırılıyor.

İki yönlü: açıklanmamış yeni ölü kod kırmızı, **ve** artık ölü olmayan
bir muafiyet de kırmızı — bayat bir muafiyet, aynı adı taşıyan gelecek
bir fonksiyonun kimsenin onun hakkında vermediği bir kararı miras
almasının yoludur. Gerekçesiz girdi de reddediliyor; gerekçe olmadan bu
dosya bulguları susturma yeri olur.

Kalan beş girdi bilinçli: ileriye dönük yüzeyler (`RevokeOwnerClaim`,
`ListUsers`) ve tip yüzeyinin parçaları, her biri hangi grubu beklediği
yazılı.

---

## Muhattap bir marka değil, bir kişi — ve 124 commit'in yazarı

Belgelerde üç yerde "somebody and somebody" arasında olması gereken bir
ilişki vardı ve üçünde de bir tarafta kimse yoktu:

- `SECURITY.md` açık bulanı "CrucibleLAB'a doğrudan yazın" diyordu; adres
  yoktu.
- `CLA.md` §1 lisansı "Crucible Analytic ve sahibi CrucibleLAB"a
  veriyordu. **Tüzel kişi olmayan bir markaya verilen lisans kimseye
  verilmiştir.** Sözleşmenin tek varlık sebebi hibenin *tutması*.
- `NOTICE`, Apache 4(d) gereği her yeniden dağıtımla seyahat eden dosya,
  atfı yapılacak kişiyi adlandırmıyordu.

Üçü de artık **Fırat Coşkun \<kettipcimm@gmail.com\>** diyor.

### Kişi adlandırmanın açtığı delik: devir

Sözleşmeyi bir kişiye bağlamak, sözleşmenin açık tutmak için var olduğu
kararı kapatma riski taşıyor: sahibi bir şirket kurduğu gün, lisanslar
şirkette değil kişide kalır. Bunun için §8 (Assignment) eklendi —
sözleşme ve §2/§3 hibeleri, projenin mülkiyetiyle birlikte devredilebilir.

**Bir belgeye kişi adı yazmak, o belgede devir maddesi olmasını
gerektirir.** Aksi hâlde belge, engellemek için yazıldığı sonucu kendisi
üretir.

### Commit yazarlığı: 124 evet, 1 hayır

Bütün geliştirme commit'leri `Claude <noreply@anthropic.com>` yazarıyla
duruyordu. Hepsi GitHub hesabının noreply adresiyle yeniden yazıldı:
`Fırat Coşkun <166753316+cruciblelab@users.noreply.github.com>`. Gerçek
e-posta 125 commit'in içinde kalıcı olarak halka açık olmuyor, GitHub
commit'leri hesaba bağlayıp katkı grafiğinde sayıyor.

**Kök commit (`Initial commit`) kasten dokunulmadı.** Zaten aynı GitHub
hesabına ait ve `main` ile paylaşılan **tek** ata. Onu da yeniden yazmak
hash'ini değiştirir, dal ile `main`'in ortak atası kalmaz ve iki dal
karşılaştırılamaz hâle gelir. Bir tabloyu düzeltmek için ortak atayı
yakmak yanlış taraftan bakmaktır; `CLA-SIGNATURES.md` yerine iki satır
taşıyor ve neden taşıdığını yazıyor.

`Co-Authored-By` fragmanları **korundu**. Doğrular, `internal/docs/cla_test.go`
onlara dayanıyor ve dürüstlükleri `aiAssistantReason`'da gerekçelendirilmiş
durumda. Değişen sadece yazar/committer kimliği.

### Sonuç: bir yerel kopyası olan herkes sıfırlamalı

Geçmiş yeniden yazıldığı için 124 commit'in hash'i değişti. Bu dalın bir
kopyasını tutan biri `git pull` ile iki tarihi birleştirmeye çalışır ve
her commit'i iki kere görür. Doğrusu:

```
git fetch origin claude/analytics-collector-mvp-7kec32
git reset --hard origin/claude/analytics-collector-mvp-7kec32
```

Şu an bu dalın başka bir kopyası olmadığı için maliyeti sıfır — ve
maliyetin sıfır olduğu an, bu işi yapmanın tek doğru anı.

---

## L3 kalanı — ve kurulumun hiç koşulmamış aşaması

Yükseltme düğmesi ile uygulayıcı L3'ün başında yazılmıştı. Eksik olan,
uygulayıcıyı bir makinede *çalıştıran* şeydi: systemd birimi, zamanlayıcı,
`upgrader.toml`, Docker giriş noktası, ve PLAN'ın iddia edilmesini
yasakladığı ölçüm.

### Önce başka bir şey çıktı: install.sh çalışmayan bir kurulum üretiyordu

`upgrader.toml`'un izinlerine karar vermek için diğer dördününkine baktım
ve şunu ölçtüm — paneli, `install.sh`'in yazdığı yapılandırma ile
`crucible` olarak başlatarak:

```
panel: config error: stat /etc/crucible-analytic/panel.toml: permission denied
```

Betik `crucible` hesabını açıyor, o hesap olarak koşan dört birim kuruyor,
ve yapılandırma dizinini `root:root 0750` bırakıyordu. Hesap dizine
giremiyordu bile. **Betiğin ürettiği kurulumda hiçbir servis
başlamıyordu.**

Neden yıllarca durabilirdi, ve bu kısmı asıl kayda değer olan:
`install_test.go`'daki her test `--no-systemd` geçiyor. `runInstall`'ın
yorumu o bayrağa "load-bearing" diyor ve haklı — bir test takımı koştuğu
makineye servis kurmamalı. Fark edilmeyen, bayrağın *neyi* taşıdığıydı.
Arkasındaki dal hesabı açan ve birimleri kuran daldı, ve bu depoda ona
hiçbir şey girmemişti.

**Test edilemeyen tek yer, bozuk olan tek yerdi.** Ve neden test
edilemiyordu: `PREFIX`, `CONF_DIR`, `LOG_DIR`, `STATE_DIR` hepsi
yönlendirilebiliyordu; birimlerin gittiği yer yönlendirilemiyordu. Ailenin
eksik üyesi `SYSTEMD_DIR` eklendi ve kapı açıldı.

### İki hesap, ve `0751`'in son hanesi

`upgrader.toml`, dağıtımdaki DDL koşabilen tek DSN'i taşıyor. Panel
`crucible` olarak koşuyor. Aynı hesap olsaydı panel, okuması bile yasak
olan veritabanını yeniden yazan kimliği okuyabilirdi.

İkinci hesap: `crucible-upgrader`. Ama o zaman dizinin kendisi sorun
oluyor — `0750 root:crucible` ise ikinci hesap içeri giremez. `0751`:
`r`siz `x`, "adını biliyorsan açabilirsin"dir. Alternatif, yükselticiyi
`crucible` grubuna koymaktı; bir geçiş sorununu çözmek için ona dört
servis yapılandırmasının tamamına okuma hakkı vermek olurdu.

### Ölçüm: hiçbir servis durmuyor

Bu, düğmenin dayandığı iddia. Müşteri sitesi trafik alırken basıyor ve
kendisine örtük olarak "bunu şimdi yapmak güvenli" deniyor.

Süreçler hakkında bir soru değil — dördü yeniden başlamıyor, yeniden
bağlanmıyor, olup biteni fark bile etmiyor. Soru **kilit**: `ALTER TABLE`
ACCESS EXCLUSIVE alır ve o tablonun her okuyucusunu ve yazıcısını
bekletir, ve bekleyen bir kilit isteği arkasına geleni de bekletir.

Dört servis kendi rolüyle kendi sorgusunu döngüde koşarken gerçek
uygulayıcı gerçek şemayı uyguladı:

```
yükseltmenin kendisi     35ms, 35ms, 86ms
pencere içinde en kötü   2.3ms .. 9.9ms
boştayken en kötü        5.0ms .. 83.5ms
```

Yükseltme sırasındaki en kötü sorgu, hiçbir şey olmazken görülenden
*hızlı*. Sebep uygulayıcıda değil, şema dosyalarında: her `CREATE`
`IF NOT EXISTS`, yani yeniden uygulama işini yapılmış buluyor.

**Özellik SQL'e ait, koda değil — testin gerekçesi tam da bu.** Bir
sonraki şema dosyası bir `ALTER` uzaklıkta ve o gün kimse bu ölçümü
hatırlamayacak.

### Testin kendi ilk hatası: adındaki büyüklüğü ölçmemek

İlk sürüm tek bir koşan maksimum tutuyor ve onu yükseltmenin maliyeti
diye raporluyordu. İlk koşusu şunu dedi:

```
the upgrade took 141ms ... worst 346ms
```

Sebep olduğu iddia edilen şeyden büyük bir sayı. Çoğu havuz ısınmasıydı,
DDL başlamadan önce.

**Adındaki büyüklüğü ölçmeyen bir test, sayısız bir testten kötüdür;
çünkü sayı alıntılanır.** Her sorgunun başlangıç/bitişi kaydedilip
pencereyle *örtüşme*ye göre ayrıldı — kapsanmaya göre değil: DDL'den önce
başlayıp sürerken devam eden sorgu, kilidin engellediği tam da odur, ve
kapsama testi en kötü örneği "kapsam dışı" diye atardı.

### Eşik seçimi: mutlak tavan yetmiyor

2 saniyelik mutlak tavan CI'ın soğuk önbelleği için gerekli ama gevşek —
hızlı bir makinede bir şey dört kat yavaşlayıp yine de geçebilir. İkinci
eşik koşuyu kendisiyle karşılaştırıyor: aynı makinedeki boş hâlin 4
katından **ve** 250ms'den kötüyse kırmızı. İkisi birden, çünkü tek başına
oran kullanılamaz (2ms tabanda 9ms zaten 4.5 kat) ve tek başına taban
zaten mutlak tavanın küçüğüdür.

Mutasyon şemaya 1sn ve 3sn ACCESS EXCLUSIVE tutan `DO` bloğu ekleyerek
yapıldı. 1sn karşılaştırmalı yarıyı, 3sn mutlak tavanı kırmızıya çevirdi —
yani duyarlı yarı gerçekten çalışıyor. Ve **yalnız kilitlenen tabloya
dokunan problar durdu**; beacon ile panel probları etkilenmedi. Ölçümün
tek tip bir yavaşlama değil, gerçek olduğunun kanıtı bu.

### Yan bulgu: iki paket aynı satırı yazıyordu

Tam entegrasyon takımı kırmızı, tek başına yeşil. `schema_version` tüm
veritabanı için tek satır; `internal/panel/web` onu dört duruma sokup
sağlık sayfasının ne dediğini kontrol ediyor, `internal/applier` uygulayıp
üstüne yazıyor.

Yarış yeni değildi. Uygulayıcının takımı hep sürüm kaydediyordu; pencere
birkaç milisaniyeyken görünmedi. **1,5 saniyelik yük onu olası olmaktan
çıkarıp kesin yaptı — bulunmasının tek sebebi bu.** `testdb.SchemaVersionLock`
eklendi, iki taraf da alıyor, kilit sırası yazılı (aynı çifti ters sırada
alan iki takım kilitlenir ve kilitlenmiş bir test takımı hata gibi değil
donmuş makine gibi görünür).

### Bir de yanlışlıkla geçen bir kontrol

`TestEveryUnitCarriesTheHardening` her birimde `User=crucible` arıyordu —
`strings.Contains` ile. `User=crucible-upgrader` bunu sağlıyor. Yani
dizindeki *kasten farklı hesapla koşan tek birim*, hepsinin aynı hesapla
koştuğunu doğrulamak için yazılmış bir kontrolden geçecekti.

`key=value` satırının alt dizgi testi, kimsenin sormadığı bir soruyu
cevaplar. Direktif artık ayrıştırılıyor, ve beklentiler iki yönlü bir
haritada gerekçeleriyle duruyor: listede olmayan birim de kırmızı, dosyası
olmayan liste girdisi de.

---

## D4c — 28 ayar yedi bölüme, ve testin bulduğu iki şey

Ayar sayfası tek düz listeydi. Artık `<details>/<summary>` ile yedi
kategori: görünüm 4, toplama 2, bot 4, gizlilik 7, sınırlar 8,
tanılama 2, bakım 1.

Native, betik değil. CSP'de ne `unsafe-inline` var ne `unsafe-eval`;
tıklama işleyicisiyle kurulan bir akordeon paketlenmiş bir betik ve bir
istisna isterdi. `<details>` ikisini de istemiyor ve klavye + ekran
okuyucu davranışıyla geliyor.

### Kapalı gelmenin güvenli olmasını sağlayan şey

Fazın asıl riski çirkin bir sayfa değil: **hiçbir bölümün çizmediği bir
ayar.** O ayar hata vermez, log yazmaz, eksik görünmez — müşteri onun
olmadığı sonucuna varır.

Ve ikinci yarısı: reddedilen kaydın bulunduğu bölüm açık geliyor.
Olmasaydı, reddedilen bir kayıt sayfanın tepesine kırmızı bir bant
çizip sebebini kapalı bir başlığın arkasında bırakırdı.

**İkinci yarı olmadan birinci yarı, özellik kılığında bir gerileme.**

Bunu yapan `Focus` alanı, daha önce üç yerde yazılıp hiçbir yerde
okunmayan bir alandı. `go vet` böyle bir alanı görmez; ölü kod kapısı da
struct alanlarını saymıyor.

### Testin bulduğu iki şey

**1. Sekiz limit ayarı hiç kategorisizdi.** `limitDefinitions` onları
üretiyor (collector ve beacon için dörder tane), ben kaydı elle
tarayınca yalnız 20 literal tanımı gördüm. Kategorisiz tanım testi
sekizini de saydı. **Elle sayılan bir liste, üretilmiş olanı görmez.**

**2. Üç kampanya ayarı gizlilik ayarıymış.** Geliştirici parolasının
arkasındalar — bu projede hukuki ağırlığın işareti bu. Kendi
`GateReason`'ları söylüyor: *"ham tıklama kimliği ... reklam ağının
kayıtlarıyla eşleştirilebilen kalıcı bir tanımlayıcıya dönüşür."*

Ben onları "Kampanya" diye ayrı bir kategoriye koymuştum, çünkü adları
öyle. Test itiraz etti ve haklıydı: **bir ayar, adına göre değil,
sakladığı şeye göre bir bölüme aittir.** Kampanya kategorisi kaldırıldı,
üçü de gizliliğe taşındı.

Muafiyet listesine yazıp susturmak seçenek değildi — o liste bulguları
susturma yeri değil, ve `KeyUpgradeLocked` orada duruyor çünkü gerçekten
veri saklamıyla ilgisi yok, gerekçesi yazılı.

### Bir mutasyon boşa gitti ve bunu fark etmek gerekti

Üç mutasyondan üçüncüsü — hata yolundan `Focus`'u çıkarmak — "geçti"
dedi. Aradığım metin `Focus:   string(key)` (üç boşluk, gofmt
hizalaması), dosyadaki `Focus: string(key)` (tek boşluk). Değişiklik
hiç uygulanmamıştı.

**Mutasyonun uygulandığını doğrulamayan bir mutasyon testi, testin
kendisi kadar boştur.** Tekrar edildi, bu sefer `assert old in s` ile,
ve gerçekten kırmızıya döndü.

### Yan bulgu: faz kodlarının bir kısmı hiç sayılmıyormuş

`plan_test.go`'nun `phaseHeading` deseni `^#### ([A-Z]I?[0-9]+...) — `
diyordu. `D4c` bu desene uymaz — "D4"ü eşler, sonra hemen em dash
bekler, "c" araya girer. Yani **harfle biten her faz kodu, tablonun
dürüstlüğünü koruyan kontrole görünmezdi.**

Bunu bulan şey kontrolün kendisi oldu: D grubunu 5/9 yaptım, test 4/8
dedi — belge hakkında haklıydım, desen hakkında değil.

Ve aynı desen `version_test.go`'da kullanılıyor, yani `+D4c` etiketli
bir yapı "var olmayan bir fazı gösteriyor" diye reddedilirdi. **İkinci
bir kopya yazmamanın karşılığı: tek düzeltme iki yeri birden düzeltti.**

---

## Üretilen bir parola nereye gider — ve biri hiçbir yere gitmiyordu

Kullanıcı sordu: *"rastgele parola dedin, bu kullanıcıya bildiriliyor
değil mi? Yapılan hatalardan biri parola üretilir, ayarlanır, ama
kullanıcıya gösterilmez — kaydetme fırsatı verilmez."*

Tasarım aslında doğruydu ve gerekçesi yazılıydı: parolalar basılmıyor,
çünkü yapılandırma dosyalarına yazılıyorlar ve terminaldeki bir parola
scrollback'teki bir paroladır. Operatörün ezberlemesi gerekmiyor —
bunlar servis kimlikleri, insan kimlikleri değil.

**Ama bir istisna var ve o istisna bozuktu.** Betik, kendisinin
yazmadığı bir yapılandırma dosyasının üstüne asla yazmaz (o dosya
çalışan bir parola ya da yeniden üretilemeyecek bir site kimliği
tutuyor olabilir). O durumda parolayı *insanın* yerleştirmesi gerekir,
yani gösterilmesi şart.

O yol dört rol için çalışıyordu. Beşinci için çalışmıyordu.

### Ölçüm

Temiz bir küme kuruldu (5433, hiçbir rol yok), operatörün elle bıraktığı
gibi bir `upgrader.toml` konuldu, kurulum koşturuldu:

```
schema_admin rolü oluşturuldu, parolası üretildi
upgrader.toml'a yazılmadı   ← doğru, betik onu yazmamıştı
ekrana basılmadı            ← kusur
kurulum "başarılı" dedi
son mesaj: "The four database passwords were generated and written
            into the configuration files"
```

Sonra `pg_hba.conf` `scram-sha-256`'ya çevrilip gerçek bir kurulum gibi
denendi:

```
panel, kendi dosyasındaki parolayla:      bağlandı
yükseltici, upgrader.toml'daki DSN ile:   FATAL: password authentication failed
```

Parola yalnızca veritabanının içinde, hash olarak vardı. Geri getirilemez.

### `trust` tuzağı — ilk denemem boşa gitti

İlk bağlantı denemem `change-me` parolasıyla **başarılı** oldu, çünkü
küme `-A trust` ile kurulmuştu. Betiğin kendi yorumu bu tuzağı zaten
anlatıyor ("pg_hba `trust` diyor, psql her parolayla bağlanır"), ve ben
tam da ona düştüm. `scram`'a çevirmeden ölçüm hiçbir şey söylemiyordu.

**Kimlik doğrulamayı sınayan bir ölçüm, önce kimlik doğrulamanın açık
olduğunu doğrulamalı.**

### Kök sebep: aynı eşleme dört yerde

Rol → dosya → DSN anahtarı eşlemesi dört ayrı yerde yazılıydı: yaz,
veritabanına yönlendir, kontrol et, ve *insana bildir*. L3 beşinci rolü
üçüne ekledi, dördüncüsüne eklemedi — ve geride kalan, bir parolayı
insana söyleyen taraftı.

Tek bir `ROLE_CREDENTIAL` tablosu oldu, dördü de onu geziyor. Ve
`TestEveryRoleTheInstallerCreatesCanBeReported` betiğin iki yarısını
karşılaştırıyor: yarattığı roller ile hesabını verebildiği roller. İki
mutasyonla ölçüldü.

### Mesajlar da yalan söylüyordu

"The four database passwords" — beş üretiliyordu, ve cümle *her üretilen
parolanın yazıldığını* iddia ediyordu, bir tanesinin yazılmadığı bir
koşuda. Sayı artık sayılıyor (`${#ROLE_PW[@]}`), yazılmıyor.

**Bir mesajdaki sayı bir iddiadır, ve kimsenin yeniden hesaplamadığı bir
iddia güven verici yönde bayatlar.**

Elle yerleştirme mesajı da güçlendirildi: bunların gösterildiği tek an
olduğu, başka hiçbir yerde bulunmadıkları, ve terminal kapanmadan
kopyalanmaları gerektiği artık açıkça yazıyor.

---

## M1 — Kaynak kütüphanesi: iki listenin çakıştığı yer, ve altıncı elle yazılmış liste

*(2026-09-01)*

Faz basit görünüyordu: kaynakları bir tabloya koy, panel oradan okusun.
İki şey çıktı — biri tasarımda, biri hiç bakmadığım bir testte.

### Önce ölçüm: hangi kaynaklar gerçekten gönderilebilir

Plan bilerek isim saymamıştı ("şimdi isim saymak, doğrulanmamış bir
iddiayı plana yazmak olurdu"). O yüzden aday veri kümeleri **indirildi**,
okunmadı.

Beşi geçti: `user-country`, `server-country`, `iptoasn-country`,
`origin-asn`, `iptoasn-asn`. Hepsi PDDL 1.0, hepsi mevcut ayrıştırıcıya
uyuyor — ülke üç sütun, ASN dört, IPv4 ve IPv6 aynı biçim. Fazın en
büyük riski buydu ve gerçekleşmedi: yeni ayrıştırıcı borcu yok.

İkisi bilerek dışarıda kaldı. DB-IP Lite CC BY 4.0, GeoLite2 MaxMind'in
kendi şartlarıyla geliyor. İkisinin de yükümlülüğü *bizde* değil
*kurulumda*: taşınacak bir atıf, kabul edilecek şartlar. Bu yazılımın
kendiliğinden indirdiği bir dosyanın, müşteriye avukatının okuması
gereken bir metin bırakması kabul edilebilir değil. Karar kaydedildi —
bedeli olan bir kaynak eklenebilir, ama bedeli `Why` alanında ve
THIRD-PARTY.md'de yazılı olarak.

Bu arada THIRD-PARTY.md'nin kaynak sağlayıcısını yanlış yazdığı da
çıktı: belge ipinfo.io diyordu, kod sapics/ip-location-db'den indiriyor.
Lisans metni bir *iddia*dır, ve indirilen dosya onu doğrulayana kadar
sadece iddiadır.

### Tasarım: planın kendi kuralı projenin başka bir kuralıyla çakıştı

Plan "kütüphane `internal/asnlookup/sources.go`'da" diyordu ve gerekçesi
sağlamdı: **ikinci liste yok.** Ama projenin başka bir kuralı da var ve o
da sağlam: panelin ayar kaydı trafik yolundaki paketleri import etmez.
Aşırı yük politikası sabitleri tam bu yüzden elle aynalanmış, üstünde iki
listenin uyuştuğunu iddia eden bir testle.

İkisi aynı anda tutulamıyordu. Üçüncü seçenek kuralı değil yerleşimi
değiştirdi: `internal/ipsources`, bağımlılığı olmayan bir yaprak paket.
Hem `asnlookup` hem `panel` onu import ediyor, liste bir tane kalıyor, ve
panel adres çözen hiçbir şeyi içeri çekmiyor.

**İki kural çakıştığında üçüncü bir yer aramak, birini feda etmekten
neredeyse her zaman ucuz.**

### `local_csv_path`: ayna bir kaynak değil, bir taşıma

Plan onu kütüphaneye "birinci sınıf giriş" olarak taşımayı söylüyordu.
Yazıldı, ve yazılınca cevaplanamayan bir soru çıktı: *yereldeki hangi
veri kümesi?* Cevap "hangisini seçtiysen o" — yani ayna kaynak
seçiminin bir alternatifi değil, seçilen kaynağın nereden okunacağı.

Bu yüzden her kaynak kendi dosya adlarını taşıyor, ve ayna dizininden
okuma seçimi izliyor. Kaynağını değiştiren bir kurulum, aynadan da
farklı dosyayı okuyor. Kütüphaneye bir "yerel" girişi konsaydı, seçimi
değiştiren ayna kullanıcısı sessizce eski dosyayı okumaya devam ederdi.

### Üçüncü ölçüt: mutasyon

Fazın "kütüphaneye eklenen bir kaynak panelde ek değişiklik olmadan
görünüyor" ölçütü, iddia edilerek değil kırılarak doğrulandı:
kütüphaneye `deneme-ulke` eklendi, panelde tek satır değiştirilmedi, ve
testin gördüğü enum `[user-country server-country iptoasn-country
deneme-ulke]` oldu.

### Ve sonra: altıncı elle yazılmış liste

Kapı kırmızıya döndü, ama benim yazdığım bir testte değil — yıllardır
yeşil duran birinde:

```
"sources.asn" is marked Live but no service reads it, so the panel
promises an immediate effect that never happens
```

Üçü için de aynı satır. Oysa collector üçünü de okuyordu; kodu az önce
ben yazmıştım. Testin `readByServices` listesi **elle yazılmıştı**, ve
üstündeki yorum bunu açıkça söylüyordu: *"internal/settings/live.go'nun
bildirdiği isimler, kopyalanmış."*

Bu sezon aynı kusurun **altıncı** görülüşü:

| # | liste | nasıl kısaydı |
|---|---|---|
| 1 | ayar kategorileri | üretilen sekiz limit ayarı hiç sayılmamış |
| 2 | `phaseHeading` düzenli ifadesi | harfli faz kodlarını (`D4c`) hiç eşleştirmiyor |
| 3 | CI'ın veritabanı rolleri | beşten dördü — `schema_admin` yok |
| 4 | paketlenmiş binary'lerin `-version` kontrolü | altıdan beşi |
| 5 | `install.sh`'ın bildirdiği parolalar | beşten dördü — biri hiçbir yere gitmiyordu |
| 6 | `readByServices` | üç yeni anahtar eksik |

Altısında da doğru hamle aynı: **ismi ekleme, listeyi türet.**

Liste artık servislerin kaynağından çıkıyor: test dışı her `.go`
dosyasında `settings.Key*` göndermeleri toplanıyor, isimler
`internal/settings` ayrıştırılarak değerlerine çözülüyor.

**Değiştirdiği şey sadece bakım değil, sorunun kendisi.** Eski liste
`live.go`'nun *bildirdiklerini* aynalıyordu; bildirilip hiçbir yere
bağlanmamış bir sabit onu memnun ederdi. Yenisi, hata mesajının zaten
sorduğunu iddia ettiği soruyu gerçekten soruyor: *bunu kim okuyor?*

### Testin kendi tuzağı: test dosyaları sayılmamalı

Tarama test dosyalarını dışarıda bırakıyor, ve bu satır dekor değil.
`internal/beacon`'ın canlı ayar takımı dört anahtarı adıyla anıyor. Test
dosyaları da sayılsaydı, servisin okumayı bıraktığı ama testin hâlâ
andığı bir anahtar "okunuyor" görünürdü — yani kontrol, tam olarak
üretimde hiçbir şey yapmayan ayar için yeşil kalırdı.

Ölçüldü: beacon'ın `LiveTrustedProxies`'inden canlı okuma silindi.

- Dışlama yerindeyken: **kırmızı** — `beacon.trusted_proxies` okunmuyor.
- Dışlama kaldırılınca: **yeşil**, aynı bozuk kodla.

Takma adlı import da aynı şekilde ölçüldü: collector'ın `settings`
importu `set` diye adlandırıldı — test yeşil kaldı; `importedAs`
kırıldı — on bir anahtar birden kırmızıya döndü.

**Bir taramanın görmediği şey, o taramanın dayandığı testi sessizce
boşaltır.** O yüzden görmediği şey de ölçülmeli.

### İki ölçüm daha, ve ikisi de gerçek bulgu çıkardı

**Beş kaynağın hiçbiri gerçekten indirilerek sınanmıyordu.** Fazın
testleri "hangi kaynak seçildi"yi ölçüyordu; "o kaynak var mı"yı hiçbir
şey ölçmüyordu. Dördü yalnız *seçen* kurulumun eriştiği adresler, yani
taşınmış bir URL müşterinin kendi sunucusundaki tek bir uyarı satırı
olarak görünür ve buraya hiç ulaşmazdı.

`internal/asnlookup/live_test.go` (`network` etiketi, gecelik):
on dosyanın hepsi çekiliyor, önek ayrıştırılıyor, ve **tamamının**
sha256'sı alınıyor. Ölçüm, 2026-09-01, ~124 MB, 4,7 saniye.

Digest'in tamamı üzerinden alınmasının sebebi ölçülerek çıktı.
`user-country-ipv4.csv` ile `server-country-ipv4.csv` ilk **330.696
byte** boyunca birebir aynı — düşük adres uzayında barındırma ülkesiyle
kullanıcı ülkesi zaten örtüşüyor. İlk yazdığım örnek 262.144 byte'tı,
yani farkın *öncesinde* bitiyordu. Tam dosyalarda: 8.796.182 ve
8.425.916 byte, 286.082 ve 274.801 satır, **11.461 satır farklı**.

Yani bir önek *biçim* hakkında kanıttır, "bunlar farklı kaynaklar"
hakkında hiçbir şey söylemez. İki kimlik aynı dosyayı gösterseydi panel
olmayan bir seçim sunar, "ne zaman tercih edersin"i açıklar, ve seçimi
değiştiren müşteri aynı veriyi geri alırdı. Mutasyonla ölçüldü:
`server-country`'nin IPv4 adresi `user-country`'ninkine çevrildi, test
"byte-identical" diye kırmızıya döndü.

**Ve sonra: hiç koşmamış bir test takımı.** Ağ işini gecelik iş akışına
eklerken komutun elle yazılmış tek bir dizin olduğunu gördüm —
`./internal/loadtest/`. Oysa `internal/asnlookup` üç tane `loadtest`
etiketli test taşıyor, biri **`local_csv_path` ağa hiç dokunmuyor**
kanıtı. Hiçbir iş akışında hiç koşmamışlar.

Geçiyorlar; mesele o değil. Kapı her etiketi `vet`'liyor, yani
derleniyorlar; elle koşturan herkeste yeşiller. **Hiç koşmayan bir
takım, geçmeyi bıraktığı günü bildiremez.**

`internal/invariants/suites_test.go` bunu iki yönlü bir değişmez yaptı:
etiketler test kaynaklarından, komutlar iş akışı dosyalarından okunuyor.
Bir etiketli takım hiçbir işte adı geçmiyorsa kırmızı; `gatedTags`'te
duran bir etiketi taşıyan dosya kalmamışsa da kırmızı (o iş, hiçbir şeyi
sınayarak geçer — ve özet sayfasında bu, geçmekle aynı görünür).

**Testin kendisi ilk koşusunda kendi hatasını yakaladı.** İş akışı
satırını ayrıştıran düzenli ifadenin karakter kümesi `[a-z,]+`'ydı —
rakamsız. `e2e` etiketi `e` diye yakalandı, e2e işi hiçbir dosyanın
taşımadığı bir etiketi koşuyor göründü, ve e2e takımı koşulmuyor
göründü. Yani düzenli ifade, testin tam olarak yakalamak için var olduğu
hatayı yaptı, ve test onu yakaladı.

---

## CI'ın haftalardır kırmızı olan yarısı gerçek bir üretim kusuruydu

*(2026-09-01)*

M1 gönderildikten sonra iş akışı geçmişine bakıldı: `cea19ec` **main'de
başarılı, dalda başarısız** — aynı commit. Başarısız olan adım, entegrasyon
takımının *ikinci* koşusu, yani aynı veritabanına karşı tekrar.

İki hata vardı ve ikisi de aynı ana denk geliyordu:

```
--- FAIL: TestNoServiceStopsWhileTheSchemaIsApplied
    panel read waited 1.020331078s during the upgrade against 17.299366ms at rest

--- FAIL: TestTheCollectorsLimitsAreNotTheBeacons
    storing collector.limits.max_concurrent: ERROR: deadlock detected (SQLSTATE 40P01)
```

**1,0203 saniye** — `deadlock_timeout`'un varsayılanı 1 saniye. Yavaş bir
sorgu değil, kilit döngüsünün çözülmesini bekleyen bir sorgu.

### Yerelde üretildi, ve sunucu günlüğü tam çevrimi yazdı

```
Process 8143 waits for RowShareLock on relation 68294; blocked by process 8142.
Process 8142 waits for ShareLock on relation 213724; blocked by process 8143.
Process 8143: INSERT INTO panel_operations (...)
Process 8142: -- Schema for the management panel...
```

`68294 = panel_users`, `213724 = panel_operations`. Yani:

- Panelin yazması `panel_operations`'a yazıyor ve yabancı anahtarı için
  `panel_users`'ı kilitliyor.
- Uygulayıcı, panel şema dosyasının içinde, `panel_operations` için
  `ShareLock` istiyor — ve `panel_users`'ı çoktan tutuyor.

Ters sırada iki taraf. PostgreSQL çevrimi birini öldürerek çözüyor, ve
kurbanı seçen o. **Kurban müşterinin yazması olabilir** — oysa yükseltme
düğmesi ona "siteniz trafik alırken basmak güvenli" demişti.

### Yazılı olan açıklama yanlıştı, ve tehlikeli olan yarısı buydu

`downtime_integration_test.go`'nun başındaki yorum şöyle diyordu:

> her CREATE, IF NOT EXISTS olduğu için yeniden uygulama işini yapılmış
> bulur ve **ağır bir kilit almaz**.

Sayılar doğruydu (yükseltme sırasındaki en kötü sorgu 2,3–9,9 ms).
Mekanizma yanlıştı. Doğrudan ölçüldü:

```
CREATE INDEX IF NOT EXISTS lockprobe_id_idx ON lockprobe (id);
NOTICE:  relation "lockprobe_id_idx" already exists, skipping
SELECT mode FROM pg_locks WHERE relation = 'lockprobe'::regclass ...
 ShareLock | t
```

**İşi atlıyor, kilidi değil.** ShareLock varlık kontrolünden *önce*
alınıyor ve işlemin sonuna kadar tutuluyor — bir şema dosyası da tek bir
örtük işlem. Yani yeniden uygulama, dosyası koştuğu sürece indeksli her
tablonun her yazıcısını gerçekten bloke ediyor. Milisaniye sürmesi
dosyaların hızlı olmasıyla ilgili bir olgu, kilitlemeyle ilgili değil.

**Yanlış bir mekanizma, koca bir hata sınıfını imkânsız gösterir.** Sayılar
doğruyken yanlış olan açıklama, yanlış sayıdan daha tehlikeli.

### Düzeltme: yükseltme yol verir, trafik vermez

`lock_timeout = 250ms`, uygulayıcının kendi bağlantısında. Seçim
`deadlock_timeout`'a (1 s) göre yapıldı: PostgreSQL kilit döngüsü aramaya
ancak bir süreç o kadar bekledikten sonra başlar. Uygulayıcının beklemesi
onun altında kalırsa **hiçbir dedektör koşmaz**, çevrimi yükseltmenin geri
çekilmesi kırar, ve seçilecek bir kurban olmaz. Trafik her zaman kazanır.

Bedeli açık: kilidini alamayan bir yükseltme başarısız olur. Ama sabırla
bekleyen bir uygulayıcı, tam da önlemek için var olduğu kesintinin
kendisidir — ACCESS EXCLUSIVE için sıraya giren bir ifade, arkasına gelen
her şeyi de bloke eder.

### İkinci kusur, birincisini düzeltirken çıktı

Kilit zaman aşımı isteği **`failed`** olarak kaydediyordu. Yani müşteri,
kendiliğinden geçen bir durum için düğmeye tekrar basacaktı.
`upgrade.Requeue` eklendi: satır `pending`'e döner, sebep satıra yazılır,
sonraki tik tekrar dener. `applier.ErrBusy` çağıranın ikisini ayırt
etmesini sağlıyor.

Ve sağlık sayfası: aynı sütun iki farklı şey taşıyor. `failed` satırında
birinin okuması gereken bir hata, `pending` satırında "tablo meşguldü, yol
verdim". İkincisine **"Hata"** demek, çalışan bir sistemin gece üçte
yeniden başlatılma sebebidir. Başlık artık duruma göre: *Hata* / *Son
deneme*.

### Ölçümler

| | |
|---|---|
| Uygulayıcı yol verdi | **261–264 ms** (1 s eşiğinin altında) |
| Panelin işlemi | commit etti, dokunulmadı |
| İstek durumu | `pending`, notu satırda |
| Dört paket birlikte, 7 koşu | temiz — öncesinde 6'da 3 |
| Entegrasyon, aynı veritabanına iki kez | temiz |

Mutasyonlar: `lock_timeout`'u 5 s yap → eşik testi kırmızı ("5,01 s
bekledi, PostgreSQL 1 s'de aramaya başlıyor"). Requeue dalını kapat →
"istek 'failed', 'pending' olmalı".

### Üçüncüsü: testin kendi yalıtımı

`TestRecovery_AWrongCodeAndAnUnknownAddressAnswerIdentically` "yanlış kod
401, bilinmeyen adres 429 — aradaki fark bir kâhin" diyordu. Bir güvenlik
bulgusu gibi okunuyor; aslında test yalıtımı kusuru: giriş kısıtlaması hem
e-postaya hem **adrese** göre sayıyor, bu paketteki her test aynı loopback
adresinden konuşuyor, ve o sayaç birikiyor.

Aynı belirti için yazılmış bir temizlik zaten vardı — ve **yalnız e-postaya
göre** temizliyordu. Kısıtlama ikisinden biri dolduğunda engelliyor, yani
düzeltme yavaşlatılmış bir hatadan ibaretti. İki yarı da temizleniyor artık.

**Sonucu kendisinden önce kaç testin koştuğuna bağlı olan bir test,
adındaki şeyi ölçmüyordur.**

### Etiketler oturunca çıkan tutarsızlık

Üç etiket itildikten sonra `v0.11.0+M1` `492c4bb`'yi gösteriyordu, ama o
girdinin notu kilit düzeltmesini de anlatıyordu — düzeltme `a522b50`'de,
bir commit sonra. **Sürüm notu, etiketli ağacın içermediği bir şeyi
iddia ediyordu.**

Kuran kişi için sonucu net: `v0.11.0+M1`'i kurar, notunda "yükseltme
artık trafiğe yol veriyor" yazdığını okur, ve o ağaç hâlâ kusuru taşır.

Not bölündü: düzeltme `v0.11.1` oldu. Faz kodu **yok**, ve bu eksiklik
değil — faz kodu "hangi fazın tamamlanmasıyla çıktı" demek, bir düzeltme
hiçbir fazı tamamlamıyor. `v0.11.1+M1` yazmak, aynı adı taşıyan iki
farklı ağaç üretirdi.

**Ve kural teste bağlandı.** Prozayı hiçbir test okuyamaz, ama ailesi
okunabilir: `TestEveryReleaseNoteHasATagAndEveryTagHasANote`. Notu olup
etiketi olmayan bir sürüm kurulamaz ama yayımlanmış görünür; etiketi olup
notu olmayan bir sürüm kurulur ve kuran kişi "ben ne yapacağım"ın cevabını
hiçbir yerde bulamaz. En yeni girdi muaf, çünkü `VERSIONING.md`'nin kendi
sırası notu etiketten önce yazdırıyor — yazılı sırayı izlemenin kapıyı
kırmızıya çevirmesi olmaz.

İki yönlü de mutasyonla ölçüldü: bir etiketi sil → "notu var, etiketi
yok"; karşılığı olmayan bir etiket ekle → "etiketli, günlük sessiz".

---

## M2 — Çekim kaydı: planın kendi içinde çelişen iki cümlesi

*(2026-09-01)*

Faz basit görünüyordu: her yenileme bir satır yazsın. İki şey çıktı — biri
planın metninde, biri yazarken yaptığım bir ölçümde.

### Dosya başına, yenileme başına değil

Plan "her yenileme denemesi bir satır" diyordu. Kod öyle çalışmıyor: bir
yenileme, bir veri kümesinin IPv4 ve IPv6 dosyalarını **ayrı** çekiyor,
bunlar ayrı düşüyor, ve `storeCountry` düşen ailenin eski tablosunu
koruyup çalışanı değiştiriyor.

Yenileme başına tek satır, "IPv6 güncel, IPv4 bir aylık" durumunu tek bir
`outcome` değerine sıkıştırmak zorunda kalırdı — ve o değerin dürüst bir
karşılığı yok. Ne `succeeded` doğru, ne `failed`.

Dosya başına yazınca yedek sıralaması da bedavaya geldi: seçilen kaynak
düşüp sıradaki çalıştığında **ikisi de** kayıtta, sırasıyla. "Neden verim
iptoasn'dan geliyor" sorusunun cevabı fazladan hiçbir alan eklemeden
ortaya çıktı.

**Bir kaydın tanesi, kaydettiği şeyin gerçekten ayrı düşebildiği en küçük
parça olmalı.**

### Bayt sayısı: başlık değil, gerçekten okunan

`Content-Length` sunucunun *göndereceğini söylediği* şeydir. İlgi çekici
başarısızlık, daha azını gönderdiğidir.

Ve bu ürün için sessiz: iki ayrıştırıcı da bozuk bir satırda **durup
okuduğunu saklıyor**. Yani yarıda kesilmiş bir dosya hatasız ayrışıyor,
tabloya yazılıyor, ve geriye internetin yarısı eksik bir aralık tablosu
kalıyor. Hiçbir hata, hiçbir uyarı. Tek fark bayt sayısında.

O yüzden sayaç gövdenin üstüne sarılıyor, ve testi "> 0" değil **dosyanın
gerçek boyutu** ile karşılaştırıyor. Sıfırdan farklı olan bir sayı,
kimsenin kontrol edemeyeceği bir sayıdır.

### Planın iki cümlesi aynı anda tutulamıyordu

> "collector yazar, panel okur"
>
> "kendi saklama süresi olacak ve `internal/panel/housekeeping.go`'ya
> bağlanacak"

Süpürme `DELETE` ister. Panelin yazmadığı bir tabloda `DELETE`
yetkisinin başka hiçbir kullanımı yok, yani ikinci cümle birincisini
bozuyor.

Yazan süpürüyor. Zamanlayıcısı zaten koşan bileşen o; tablo yalnız o
koşarken büyüyor; kapatılmış bir collector'dan artakalan satırlar haftada
bir avuç. Alternatif, panel yetkisiz silebilsin diye tek bir `DELETE`
için `SECURITY DEFINER` fonksiyon eklemekti — byte'larla ölçülen bir
kazanç için gerçek bir yüzey.

**Planın asıl koruduğu şey dosya değildi, özellikti**: "yazılıp
çağrılmayan süpürme". `TestEverySweepIsReachableFromRun` paketin çağrı
grafiğini kaynaktan çıkarıp `Run` → `sweep` → `PurgeOldFetches`
ulaşılabilirliğini arıyor. Doğrudan çağrı değil **ulaşılabilirlik**,
çünkü ara adım bilinçli; doğrudan çağrı isteyen bir test, davranışı değil
kodun şeklini dayatır.

### Yolda çıkan kusur: kimsenin kontrol etmediği üç yetki

Yeni tabloya `GRANT USAGE ON SEQUENCE ip_range_fetches_id_seq` yazarken
gerekip gerekmediğini ölçtüm. Gerekmiyordu:

```
CREATE TABLE idprobe (id BIGINT GENERATED ALWAYS AS IDENTITY ..., note TEXT);
REVOKE ALL ON idprobe FROM collector;  GRANT INSERT ON idprobe TO collector;
-- collector olarak:
INSERT INTO idprobe (note) VALUES ('...');   ->  INSERT 0 1
```

PostgreSQL identity sekansını sütununun parçası sayıyor. Gerçek tabloda
da doğrulandı: `panel_upgrade_requests_id_seq` üzerindeki bütün yetkiler
geri alındıktan sonra `panel_user` satır ekledi ve **id 740** aldı.

Depoda aynı şekilde gereksiz **iki** tane daha vardı. Yani ben üçüncüsünü
ekliyordum.

`BIGSERIAL` sekansları farklı ve yetkilerine gerçekten ihtiyaç duyuyor —
ve iki tür `grants.sql` içinde **birbirinin aynısı görünüyor**. Ayıran tek
şey sütun bildirimi, ki o başka bir dosyada.

### Ve dosyadan silmek, veritabanından silmiyor

Bu, kaçırılması en kolay yarısıydı. `grants.sql` her kurulumda yeniden
koşuyor — ama olmayan bir satır yeniden *verilmiyor* sadece; verilmiş
olan duruyor. "O yetkiyi kaldırdık" cümlesi depo için doğru, her kurulum
için yanlış olurdu.

Açık `REVOKE` eklendi. Testin ilk koşusu bunu kendisi gösterdi: dosyadan
sildikten *sonra* koştu ve on yetkiyi hâlâ orada buldu.

### Testin kendi ilk hâli de eksikti

`information_schema.usage_privileges` yalnız `USAGE` bildiriyor. İlk
sorgu beş satır buldu; `aclexplode` ile bakınca **on** çıktı — her birinin
`SELECT` yarısı görünmüyordu.

**Bir yüzey denetimi, denetlediği yüzeyin tamamını göremiyorsa, bulduğu
şey kadar bulmadığı şeyle de yanıltır.**

### Ve asıl kusur: düğmeyle yükselten kurulum, kimsenin yazamadığı bir tablo alıyor

Yetkileri `grants.sql`'e yazdıktan sonra bir soru kaldı: **bir kurulum bu
şemaya iki yoldan ulaşıyor, ve ikisi aynı işi yapmıyor.**

```
install.sh          şema dosyaları, sonra release/sql/grants.sql
yükseltme düğmesi   şema dosyaları, ve başka hiçbir şey
```

`internal/schemafiles.InOrder` tam olarak `schema.sql` dosyalarının
listesi; yetkiler orada değil.

Bu projedeki her tablo yükseltme düzeneğinden **önce** var. Yani L1–L3
yazıldığından beri kimse yeni bir tablo eklememişti — ve bu ilki.
Ölçüldü:

```
psql -U collector -c "INSERT INTO ip_range_fetches ..."
ERROR:  permission denied for table ip_range_fetches
```

Ve arızanın şekli bu projenin klasiği: `recordFetch` bir uyarı yazıp
geçiyor, yenileme devam ediyor, coğrafya çalışıyor, çekim kaydı sonsuza
kadar boş kalıyor — **hiç yenilenmemiş bir kurulumdan ayırt edilemez
biçimde**. Yani fazın getirdiği tek şey, düğmeyle yükselten her müşteride
sessizce çalışmıyor olacaktı.

Çözüm deponun kendi kalıbı: yetkiler tablonun şema dosyasında, rol var mı
diye bakan bir `DO` bloğunun içinde — `internal/retention/schema.sql`
tam olarak bunu yapıyor. `grants.sql` yine listeliyor, çünkü "bu rol ne
yapabilir" sorusunun cevabı orada; iki kez verilen bir yetki
etkisizdir.

`TestTheUpgradePathAloneLeavesTheFetchLogWritable` düğme yolunu birebir
oynuyor: bütün yetkileri geri al, şema dosyasını **uygulayıcının kendi
rolüyle** uygula, sonra her servise işini yaptır. Mutasyonla ölçüldü —
`DO` bloğunu sil, test kusurun ilk hâlini kelimesi kelimesine geri
veriyor.

**Bir kurulumun iki farklı yoldan ulaşabildiği her durum için, iki yolun
da ölçülmesi gerekir.** Biri kurulumda koşuyor diye test ediliyorsa,
diğeri hiç koşulmamış demektir.

---

## M3 — Düğme, ve `Run`'ı boşaltmanın hiçbir şeyi kırmaması

*(2026-09-01)*

Plan "L3'ün deseninin aynısı, bedava geliyor" diyordu. Deseni gerçekten
aynı; bedava olmayan iki şey çıktı.

### L3'ten ayrılan tek gerçek yer: cevaplayan taraf olmayabilir

Upgrader paketle birlikte kurulur, hep oradadır. Resolver ise yalnız
`asn_lookup` açıksa vardır — **ve varsayılan kapalı.**

Yani L3'te "kimse almadı" bir kaza; burada çoğu kurulumun olağan hâli. Ve
tek-uçuş indeksi olduğu gibi kopyalansaydı sonucu şu olurdu: müşteri
düğmeye basar, satır yazılır, kimse almaz, ve indeks o günden sonra her
isteği reddeder. **İlk basış, o kurulumun kabul ettiği son basış olurdu.**

`ExpireStale` bu yüzden var, ve `DELETE` bu yüzden panelin: kimsenin
almadığı bir satır söz konusuysa hâlâ çalışan taraf odur.

`running` satırlara dokunulmuyor. Onları tutan servis hâlâ 124 MB
indiriyor olabilir, ve boşalan yuva ikinci bir yenilemeyi birincinin
üstüne başlatır — silmenin "temizlik" gibi görünüp iki eşzamanlı indirme
ürettiği yer tam burası.

### Parola yok, ve gerekçesi yazıldı

L3'ün düğmesinde kilit + geliştirici parolası var. Burada ikisi de yok, ve
bunu yazmadan bırakmak ileride birinin "tutarsızlık" diye kapatacağı bir
boşluk olurdu.

Kural şu: *geliştiriciye iş çıkarabilen* şeyler parolanın arkasında,
çünkü müşteri kendine her yetkiyi verebilir — `RoleOwner` üye yönetebilir,
yani kendini yönetici yapabilir. Yetki müşteriyi değil personelini
sınırlar.

Bu hiç kimseye iş çıkarmıyor: müşterinin kendi sunucusuna, kendi
hattından, iki kamuya açık dosyayı yeniden indiriyor. Parola koymak,
"parola önemsiz şeyler için de sorulur" diye öğretmek olurdu.

### Ve asıl bulgu: `Run`'ı boşaltmak hiçbir testi kırmıyordu

Fazın kodu bitmişti, testler yeşildi. Mutasyon olarak `Run`'ın istek
yoklama dalını boşalttım:

```go
case <-requests.C:
    // mutation: the poll does nothing
```

**Bütün depo yeşil kaldı.** Kuyruk testleri geçti, panel testleri geçti,
uçtan uca test geçti — çünkü hepsi `answerRequests`'i *doğrudan*
çağırıyordu. Düğme sessizce çalışmayı bırakır, hiçbir şey söylemezdi.

Bu, "yazılıp çağrılmayan süpürme"nin bir üst katı: **etkisi olmayan bir
yoklama.** Ve M2'de tam bu sınıf için yazdığım test onu kaçırdı, çünkü
yalnız `PurgeOld*` adlı metotları arıyordu.

İki şey eklendi:

**1. `Run` ilk tikten önce bir kez de yokluyor.** Hem doğru davranış —
düğmeye basıp sonra servisi yeniden başlatan biri (ki bir şey olmadığını
düşünen herkes onu yapar) boşuna otuz saniye beklemesin — hem de gerçek
giriş noktasını ölçülebilir kılan şey. `TestRunItselfAnswersAWaitingRequest`
artık `Run`'ı çalıştırıp isteğin cevaplandığını görüyor; hiçbir aralık
kısaltılmıyor, hiçbir şey taklit edilmiyor.

**2. `TestRunReachesEveryPeriodicDuty`**, `PurgeOld*` türetmesinin yanına
gerekçeli bir *görev* listesi koyuyor: her girdi, o görev koşmazsa neyin
durduğunu yazıyor. Liste elle, ve bu bilinçli — süpürmelerin ad şekli var,
diğer görevlerin yok, ve alternatif hiçbirini adlandırmamaktı; bu testin
başladığı ve kaçırdığı yer de tam orası.

**`Run` bir servisin sahip olduğu tek giriş noktası. Ona ulaşmayan bir
görev, kaç test doğrudan çağırırsa çağırsın, hiçbir kurulumun yapmadığı
bir görevdir.**

---

## Kararsız kapı: eşik ölçüldüğü makineye aitmiş

*(2026-09-01)*

`afd58ad` main'de yeşil, dalda kırmızı — aynı commit. Kırmızı olan
`TestNoServiceStopsWhileTheSchemaIsApplied`, ve **0,7 milisaniyeyle**:

```
api read waited 250.732758ms during the upgrade
against 41.663715ms at rest — 4x worse, and over 250ms.
```

O koşudaki bütün sondalar boştaki hâllerinin 5–8 katıydı, ve yükseltmenin
kendisi **639 ms** sürmüştü — geliştirme makinesinde 47 ms. Yani makine
on üç kat yavaştı. Hiçbir servis durmamıştı.

### Ama asıl bulgu CI'da değildi

Eşiği değiştirmeden önce kendi makinemde ısıtma sonrası üç ölçüm aldım.
İkincisi şuydu:

```
yükseltme 461 ms
collector insert   boşta 8,5 ms   sırasında 393 ms   → 46x
beacon insert      boşta 8,8 ms   sırasında 393 ms   → 45x
panel write        boşta 8,9 ms   sırasında 371 ms   → 42x
```

**Eski kural bunu da kırardı.** Yani sorun "CI yavaş" değildi; eşik
kendi geliştirme makinemde de yanlıştı, sadece o koşu denk gelmemişti.

Ve bu sayılarda yanlış olan bir şey yok: v0.11.1'de ölçtüğümüz gibi
`CREATE INDEX IF NOT EXISTS` **işi atlıyor, kilidi değil** — ShareLock
dosyanın sonuna kadar tutuluyor. Şema dosyası koşarken gelen bir yazıcı,
o dosyanın kalanı kadar bekler. 393 ms, 461 ms'lik pencerenin %85'i.

### Doğru sınır pencerenin kendisi

Bir sorgu, **yükseltmenin sürdüğünden uzun süre** yükseltme yüzünden
beklemiş olamaz. Pencerenin içindeki bir bekleyiş, bir şema dosyası
uygulamanın zaten kabul edilmiş bedeli; üstündeki bir bekleyiş başka bir
şeyi bekliyordur.

Taban artık `max(250ms, yükseltme süresi)`. Mutlak tavan (2 sn) yerinde
duruyor ve iş bölümü net:

| kontrol | neyi ayırt eder |
|---|---|
| 2 sn tavan | müşterinin fark ettiği bir durma — sebebi ikinci soru |
| pencere tabanı | "yükseltmenin arkasında sıraya girdi" ile "başka bir şey bekledi" |
| 4× oran | küçük mutlak sayıların rapor edilmemesi |

**Bedeli açıkça yazıldı:** yükseltmeyi ~400 ms yapan ve bir sorguyu o
kadar bekleten hafif bir gerileme artık tabanın *üstünde* değil
*üzerinde* durur. O bant öbür taraftan kapalı —
`TestTheUpgradeYieldsToTrafficRatherThanTheOtherWayRound` çekişmeyi
kurgulayıp mekanizmayı doğruluyor, zamanlamalardan çıkarım yapmıyor.

### Ve kural artık sayılarla sınanabiliyor

Karar `judgeStall`'a çıkarıldı, ve `TestTheStallRuleAgreesWithWhatWas
Measured` onu **gerçekten gözlenmiş** sayılara karşı koşuyor: geliştirme
makinesinin sıradan koşusu, 46 katlık sıraya girme koşusu, 0,7 ms ile
düşen CI koşusu, artı kurgulanmış bir gerileme ve iki sınır durumu.

Eskiden kural ölçümün içinde gömülü bir `switch`'ti — yani ne diyeceğini
öğrenmenin tek yolu o koşuyu üretmekti, ve önemli olan koşular bir
geliştirme makinesinde üretilemiyor.

**Kimsenin bir koşuyu şanslıca yakalamadan sınayamadığı bir eşik, kırmızı
bir yapıya bakan kişi tarafından ayarlanan bir eşiktir.**

Mutasyonlar: eski sabit tabana dön → iki gerçek gözlem kırmızı; tabanı
hepten kaldır → üç durum kırmızı; duyarlı yarıyı kaldır → iki durum
kırmızı.

---

## İki yükseltici aynı anda: ve ölçtüğünü sanan iki test

`go test -tags integration ./...` ikinci koşusunda kırmızıya döndü.
Düşen test `TestTheUpgradePathAloneLeavesTheFetchLogWritable`, hata
`applying the schema the way the upgrader does: ERROR: tuple concurrently
updated (SQLSTATE XX000)`.

Bu, CI'ın "aynı veritabanına karşı ikinci koşu" adımının var olma
sebebi: ilk koşuda görünmeyen, yalnız zaten kurulmuş bir şemaya ikinci
kez dokunulunca çıkan bir kusur.

### Ölçüm, tahmin değil

Şüpheli belliydi: M2'de yeni tablonun yetkilerini şema dosyasının içine,
rol korumalı bir `DO` bloğuna taşımıştım. Ama bu oturumda bir kez zaten
**mekanizmayı yanlış açıklamıştım** ("IF NOT EXISTS ağır kilit almaz" —
ölçüldü, yanlış), o yüzden bu sefer önce ölçtüm: üç eşzamanlı oturum,
her biri aynı, **zaten verilmiş** yetkiyi 300 kez veriyor.

    900 denemenin 93'ü:  ERROR: tuple concurrently updated

Yani: **hiçbir şeyi değiştirmeyen bir `GRANT` de yazıyor.** `GRANT`
hedefin ACL satırını içeriği aynı kalsa da yeniden yazıyor, ve tek bir
katalog satırını aynı anda yeniden yazan iki oturum sıraya girmiyor —
kaybeden XX000 alıyor.

Dosyadaki yorum bunun tam tersini söylüyordu: *"a grant issued twice is
idempotent"*. Sonucu doğru, yazması yanlış. Bu oturumun ikinci kez
karşılaştığı şekil: **sayılar doğru, açıklama yanlış — ve tehlikeli olan
yarısı açıklama.**

### Kusur benim sandığımdan genişti

Testi yazdım (üç uygulayıcı, bütün dosyalar, on ikişer tur) ve
öngörmediğim bir şey buldu:

    internal/retention/schema.sql     tuple concurrently updated (XX000)
    internal/asnlookup/schema.sql     tuple concurrently updated (XX000)
    internal/upgrade/schema.sql       deadlock detected          (40P01)
    internal/rangerefresh/schema.sql  deadlock detected          (40P01)

360'ta 17. Ve `internal/logsink` ile `internal/upgrade` hiç `GRANT`
içermiyor — yani ikinci, ayrı bir sebep vardı: `CREATE OR REPLACE
FUNCTION` gövde değişmese de `pg_proc` satırını yeniden yazıyor, ve her
dosyadaki `DROP POLICY IF EXISTS` + `CREATE POLICY` çifti her uygulamada
bütün politikaları yeniden yazıyor.

**Yedi şema dosyasından üçünde `GRANT` vardı; kusur yedisinin dördünde.**
Yalnız kendi eklediğim dosyayı düzeltmek, bu oturumun yedi kez
karşılaştığı hatanın tam kendisi olurdu: *bir liste yanlış olduğunda
tehlikeli değildir; kısa olduğunda tehlikelidir.*

### Politika çifti koşullu hâle getirilemez

`GRANT` için çözüm doğrudan: önce `has_table_privilege` /
`has_function_privilege` ile sor, gerekmiyorsa yazma. Yetki başına ayrı
ayrı sormak zorunda — virgüllü biçim "bunlardan **herhangi biri**"
demek, ve yalnız `SELECT` tutan bir rol tam sayılıp `INSERT`'ü hiç
almazdı.

`DROP POLICY` + `CREATE POLICY` için aynı numara **yapılamaz**. "Politika
zaten varsa atla" demek, ileride değişen her politikanın kurulu bir
veritabanına sessizce hiç ulaşmaması demek — düzeltilenden daha kötü bir
hata. Şema uygulamak doğası gereği tek yazarlık bir iş; o yüzden
uygulayıcı artık bütün uygulama boyunca tek bir danışma kilidi tutuyor.

### Ölçtüğünü sanan iki test

Buradan sonrası, testin kendisinin iki kez yalan söylemesi.

**Birincisi**, testin izolasyon için `SchemaApplyLock`'u kendisinin
tutmasıydı — yani ölçmeye çalıştığı kilidi. Üç uygulayıcının üçü de
teste yol verdi: "0 applied, 8 gave way". Tertemiz bir sonuç, ve tek
kanıtladığı şey testin kendini bloke edebildiği.

**İkincisi daha sinsiydi.** Test `RunOnce`'ı sürüyordu — dağıtımın
çağırdığı şeyi, ki doğru görünür. Yeşil geçti. Sonra kilidi kasten
bozdum: **yine yeşil geçti.** Sebep kuyruk: aynı anda tek bir istek
uçuşta olabildiği için yalnız bir uygulayıcı hak talep ediyor, diğer
ikisine "yapacak bir şey yok" deniyor — üçü şemanın içinde hiç
buluşmuyor. Test "8 applied, 0 gave way" diyordu ve **ikinci sayı bütün
cevaptı**, gözümün önünde duruyordu.

İki uygulayıcı ancak kuyruğun koruyamadığı yerde çakışıyor: bir hak
talebi on beş dakikada bayatlıyor ve başkası devralıyor, yani ölü değil
*yavaş* olan uygulayıcı hâlâ `apply` içinde. Bunu kuyruk üzerinden
üretmek on beş dakikalık bir test demek. O yüzden test artık doğrudan
`apply`'ı çağırıyor.

### Kilit sandığım şeyi yapmıyormuş

Ve düzeltilmiş test kilidi kaldırınca **hâlâ** kırmızıya dönmedi. Beş
koşu, tek bir XX000 yok.

Sebep: uygulayıcı zaten 250 ms'lik `lock_timeout` ile çalışıyor. Sıradan
tablo kilitlerinde erkenden yol verdiği için kataloğa çakışacak kadar
derine hiç inmiyor. **XX000'i engelleyen şey yeni kilit değil, zaten
orada olan `lock_timeout`'muş** — ilk 17/360 ölçümünü ham `Exec` ile,
yani `lock_timeout`suz yapmıştım.

Kilit başka bir şey yapıyor, ve o da gerçek:

    kilitle    24 uygulandı,  0 yol verdi
    kilitsiz    8 uygulandı, 16 yol verdi

İşin üçte ikisi boşuna kuyruğa dönüyordu. Kilit çakışmayı değil,
**anlamsız yeniden denemeyi** engelliyor: 25 ms süren bir uygulamayı
beklemek, isteği bir tur daha kuyrukta tutmaktan ucuz.

Test artık bunu ölçüyor (`busy > appliers` → kırmızı), ve mutasyon iki
yönde de doğrulandı. Ayrıca "hiç çakışma olmadı" durumu artık ayrı bir
kırmızı: en fazla kaç uygulayıcının aynı anda içeride olduğu sayılıyor,
ikiden azsa koşu hiçbir şey ölçmemiştir.

`dblock`'un yorumu bu ayrımı yazıyor — çünkü "kilit XX000'i engelliyor"
demek, doğru sayılarla yanlış bir mekanizma yazmak olurdu ve bu oturum
onun ne kadar pahalı olduğunu iki kez gösterdi.

### Kalan

Testler, elle şema uygulayan paketler için aynı anahtarı
`testdb.SchemaApplyLock` olarak alıyor — çünkü onların `lock_timeout`'u
yok ve gerçekten XX000'e varıyorlar. Kırmızıya dönen koşunun sebebi tam
olarak buydu.

**Bir kurulumun iki ayrı yoldan aynı duruma varabildiği her yerde, iki
yol da ölçülmeli** — bu oturumda ikinci kez.

---

## Gecelik: kapının hiç koşmamış yarısı, ve yalan söyleyen bir mutasyon

Gecelik hattın "bütün ürün, tarball'dan ve imajdan" işi düşüyordu — ve
`main` yeşilken düştüğü için kimsenin bakmadığı bir kırmızıydı. Sebebi
tek satır:

    install: systemd units need root, and this is running as runner.

`e2e`'nin `install()` yardımcısı `install.sh`'ı **`--no-systemd`
olmadan** çağırıyordu. Betik haklı: köke ihtiyaç duyan birim dosyalarını
yazamayacağını anlayıp hiçbir şey yaratmadan duruyor. Kapının kendi
entegrasyon işi bu bayrağı ilk günden veriyordu; `e2e` yolu sonradan
yazıldı ve vermedi.

**Bir kurulumun iki ayrı yoldan aynı işi yaptığı her yerde, iki yol da
ölçülmeli** — bu oturumda üçüncü kez, ve bu sefer ölçülmeyen yol aylardır
hiç koşmamış.

### Mutasyonun yalan söylediği yer

Düzeltmeyi yapıp `go test -tags e2e` koştum: yeşil. Sonra bayrağı geri
alıp tekrar koştum — **yine yeşil.** Yani mutasyon düzeltmenin gerekli
olmadığını söylüyordu.

Söylemiyordu. Bu konteynerde `id -u` sıfır, ve betiğin reddi
`elif [ "$(id -u)" -ne 0 ]` dalında. Kök olarak koşan bir makinede o dal
hiç çalışmıyor, yani mutasyon **başarısız olamayacağı bir ortamda**
koşmuştu.

Gerçek koşul kurulup ölçüldü — paket dünyaya açık bir dizine açıldı ve
`nobody` olarak koşturuldu:

    bayraksız, kök değil:   install: systemd units need root ...  (geceliğin aynısı)
    bayraklı, kök değil:    == preflight → == database analytics  (geçti)

**Ortamı, başarısızlığın koşulunu taşımayan bir mutasyon hiçbir şey
kanıtlamaz** — ve yeşil geldiği için kanıtladığını sanmak kolay. Bu
oturumda ölçtüğünü sanan üçüncü test bu oldu; ilk ikisi
`TestAppliersRunningAtOnceGiveWayInsteadOfColliding`'in iki sürümüydü.

---

## Sözlük: aynası olmayan tek belge, ve iki yönlü kontrolün tek yönü

"md dosyaları eskide kalmadı mı" diye soruldu. `CHANGELOG` güncel,
`PLAN` güncel (M1/M2/M3 ✅, C8 sırada), `README`'de eskiyecek bir sayı
yok. **`SOZLUK.md` değildi.**

Bütün **L1–L3 yükseltme makinesi** sözlükte hiç yoktu: şema sürümü,
parmak izi, yükseltme kuyruğu, uygulayıcı, `lock_timeout`. Sonradan
gelen M fazları girdi almıştı — kusuru görünmez kılan da buydu, çünkü
belge *en yeni* kısımları güncel olduğu için güncel görünüyordu.

İpucu zaten dosyanın içindeydi: yenileme kuyruğu girdisi
"`internal/upgrade` ile aynı desen, bir tablo ötede" diyor, yani okuru
sözlüğün hiç tanımlamadığı bir pakete, tanımlamış gibi gönderiyordu.

### Kontrol vardı ama tek yöne bakıyordu

Önce "hiçbir test SOZLUK'a bakmıyor" dedim. **Yanlıştı** — `release/`
altında dört test var, ve aramayı tek dizine daralttığım için görmedim.
Ama asıl mesele bu değil: o dörtlü, sözlüğün *andığı her şeyin var
olduğunu* soruyor. Yeniden adlandırmayı, düşen tabloyu yakalar.
**Tanımlanmamış bir atfı yakalayamaz**, çünkü paket ağaçta gerçekten
duruyor.

Yani kontroller yeşildi, ve bir faz grubu kadar boşluk aylarca yaşadı.
*Bir yüzeyi tek yönden ölçen kontrol, o yüzeyin öbür yarısı hakkında
sessizdir — ve sessizliği yeşil görünür.*

Eklenen kural ters yöne bakıyor, elle liste tutmadan: **sözlük bir paketi
anıyorsa, sözlükteki bir girdi onu tanımlamalı.**

### Kuralın ilk hâli fazla gevşekti, ve bunu mutasyon gösterdi

İlk sürüm, kalın bir terimle başlayan paragrafın *herhangi bir yerinde*
geçen paketi "tanımlı" sayıyordu. Mutasyon: başka bir girdinin gövdesine
`internal/storage` diye tanımsız bir atıf düşürdüm — **test görmedi.**
Beş paket anan bir girdi, beşini birden "tanımlıyor" oluyordu; asıl
boşluk da yalnız kalın olmayan bir paragrafta durduğu için yakalanmıştı,
yani şans eseri.

Kural girdinin **başına** daraltıldı — kalın terimden em çizgisine kadar,
ki dosyanın kendi yazım düzeni zaten bu:

    **uygulayıcı (applier)** (`internal/applier`) — Bu depoda DDL...

Daraltılmış kural iki gerçek boşluk daha buldu: `internal/dblock` ve
`internal/testdb`, ikisi de başka girdilerin gövdesinden anılıyor,
ikisinin de kendi girdisi yok. İkisi de yazıldı.

Dört mutasyonun dördü de kırmızı: girdiyi sil, gövdeye tanımsız atıf
düşür, kalın terimi düz metne çevir, paketi başlıktan gövdeye it.

---

## Aynı kilidin iki anlamı olamaz

Kapıyı koştururken `TestAppliersRunningAtOnceGiveWayInsteadOfColliding`
iki koşunun **birincisinde** düştü: *8 applied, 16 gave way*.

Bu, kilidin hiç alınmamış hâlinin **birebir imzası**. Ama kilit
yerindeydi. Olan şuydu: `internal/asnlookup`'ın yükseltme yolu testi
`SchemaApplyLock`'u kendi süresi boyunca tutuyor, `go test ./...`
paketleri paralel koşturuyor, ve yarışan uygulayıcılar birbirlerine
değil **ona** yol veriyordu.

Yani yanlış kırmızı ile doğru kırmızı aynı kırmızıydı — ve testi ben
öyle yazmıştım.

Tek bir anahtar iki şeyi birden söyleyemiyor:

- *"ben şema uygularken kimse girmesin"* → `dblock.SchemaApply`, üretimin
  mekanizması, elle şema uygulayan testlerin de aldığı.
- *"ben kimin uyguladığını ölçerken kimse uygulamasın"* → yarış testinin
  ihtiyacı, ve bunu `SchemaApplyLock` ile ifade etmek imkânsız, çünkü o
  anahtar zaten ölçülen şeyin kendisi.

İki anlam, iki anahtar: `testdb.SchemaRaceLock` eklendi. Dört koşu üst
üste temiz, ve kilidi kasten bozan mutasyon hâlâ kırmızıya döndürüyor —
yani yalıtım, duyarlılığı öldürmeden geldi.

---

## Aynı hatayı, onu düzeltmek için yazdığım testte tekrarladım

`5cebc75` CI'da düştü: *7 of 24 gave way, 17 applied*. Eşiğim `busy >
appliers`, yani üçten fazla yol verme kırmızı. Kendi makinemde 24/0
çıkıyordu; CI daha yavaş, bir uygulama 250 ms'yi aşabiliyor, ve kilit
**doğru çalışırken** yedi bekleme oluyor.

Bu, v0.13.1'de düzelttiğim hatanın birebir aynısı — *eşik ölçüldüğü
makineye aitti* — ve onu düzeltmek için yazdığım testin içinde. Oran bir
eşik ister, eşik de bir makineye ait olur.

### Sayı değil, cins

Ayrım baştan oradaydı, bakmıyordum: her iki bekleme de 55P03 ama
**neyi** beklediği farklı.

- **Şema kilidini** bekleyen, başka bir uygulayıcıyı bekliyor — tasarımın
  çalışması. Kaçının beklediği tamamen makinenin hızına bağlı.
- **Tabloyu** bekleyen, trafiği bekliyor — ve bir uygulayıcı trafiğe
  ancak şemanın içindeyken rastlar, yani orada başkası varken orada
  olmaması gerekirken.

İkincisi makineden bağımsız: hızlı makinede de yavaş makinede de sıfır
olmalı. `ErrSchemaLockBusy` ikisini ayırt edilebilir kıldı; davranış
değişmedi, `RunOnce` ikisini de `ErrBusy`'ye çeviriyor.

### Ve mutasyonum da yanlışmış

"Kilidi bozan mutasyon hâlâ kırmızıya döndürüyor" demiştim. Döndürüyordu
ama sebebi sandığım değildi: `+mutantOffset`'i yalnız **alma**
çağrısına uygulamıştım, **bırakma** çağrısına değil. Kilit K+ofset'te
alınıp K'da bırakılıyordu, yani hiç bırakılmıyordu. Ölçtüğüm şey "kilit
yok" değil, "kilit sızdırıyor"du — gerçek bir gerileme, ama başka bir
gerileme.

Doğrusu yapıldı: `pg_advisory_lock` ve `pg_advisory_unlock` birlikte
kaldırıldı. Sonuç, kilidin engellediği asıl bozulma:

    applying internal/retention/schema.sql:
        ERROR: tuple concurrently updated (SQLSTATE XX000)  (3x)

**Bir mutasyonun kırmızı vermesi, doğru şeyi bozduğunu kanıtlamaz.** Bu
oturumda ölçtüğünü sanan dördüncü şey bu oldu — ve bu sefer sanan bendim,
test değil.

---

## C8 — Kuralın tersine işlediği tek yer, ve iki gerilim

Geliştirici erişim politikası artık müşterinin: *onay bekle*, *doğrudan
reddet*, ya da bir bitiş zamanına kadar *geçici olarak açık*.

### Kural neden burada ters çalışıyor

Projenin kuralı: **geliştiriciye iş çıkarabilen her şey geliştirici
parolasının arkasında.** Sebebi, müşterinin kendine yetki verip
başkasına iş çıkarmasını engellemek.

Bu ayar tam tersi yönde: müşteri kendini koruyor. Parolanın arkasına
koymak, korumayı korunacak kişinin elinden almak olurdu — ve
ulaşamadığın bir koruma koruma değildir. Kuralı çiğnemiyor, kuralın
gerekçesini uyguluyor.

Bir şeyi de açıkça yazdım: **bu gerçek bir kilit değil.** Sunucuya kabuk
erişimi olan biri zaten içeride. Kilit sandığı şey kilit olmayan bir
müşteri, kilit olduğunu sandığı için başka bir önlem almaz — o yüzden
ayarın kendi açıklaması bunu söylüyor.

### Birinci gerilim: "sebebi ekranda yazsın" ile numaralandırma

C8 diyor ki *"neden giremiyorum" sorusunun cevabı ekranda durmalı*. Ama
o sayfanın yazılı bir kuralı vardı, ve haklı: **her başarısızlık aynı
sayfayı gösterir** — bilinmeyen, süresi dolmuş, kullanılmış, reddedilmiş
— çünkü ayırmak, tahmin eden birine jetonunun bir zamanlar gerçek
olduğunu doğrulardı.

İki şart çelişiyor gibi duruyor. Çözüm ayrımın nerede olduğunu görmekten
geçiyordu: **politika jetonun değil kurulumun özelliği.** Cümle her
ziyaretçiye, geçerli geçersiz ayrımı yapmadan yazılıyor. Bilinmeyen bir
jetonla reddedilmiş bir jeton hâlâ birebir aynı sayfayı görüyor; değişen
tek şey, sayfanın artık *kurulumun* ne yaptığını söylemesi.

### İkinci gerilim: `auto_approved` bayrağını yeniden kullanma tuzağı

`open` politikası isteği onaylıyor. En kolay yol, var olan
`auto_approved` bayrağını kullanmaktı. **Sessizce bozulurdu:** o bayrak
"sahip yokken verildi" demek ve `RedeemDevAccess` bir hesap var olur
olmaz o satırları öldürüyor. Yani politika onayı, sahibinin bilerek evet
dediği tek durumda, doğduğu anda ölü bir bağlantı üretecekti.

Testi bunun üstüne yazdım (`req.AutoApproved` yanlış olmalı **ve**
bağlantı gerçekten kullanılabilmeli), ve mutasyon doğruladı: bayrağı
yeniden kullanan sürüm kırmızıya dönüyor.

### Ölçtüğünü sanan beşinci şey — bu sefer yine bendim

"Ask'e dönerken pencere temizleniyor" testi, temizlemeyi hiç
çalıştırmıyordu: çağrıda pencereyi zaten boş geçiyordum, yani temizleyen
kod hiç koşmadan test yeşil geliyordu. Mutasyon (`if false`) yakaladı.
Düzeltilmiş hâli formun gerçekte yaptığını yapıyor — açılır menü "ask"e
geçiyor, kutu geçen haftaki zaman damgasını hâlâ taşıyor.

### İki aynanın yakaladığı iki hata

Kendi yazdığım aynalar bu fazda bana iki kez "hayır" dedi:

1. **`Live: true`** koymuştum. M1'de kurduğum ayna reddetti: `Live`, bir
   *servisin* `internal/settings` üzerinden yeniden başlatmadan okuduğu
   ayar demek. Bunu okuyan panel, servis değil. Bayrak müşteriye
   collector ve beacon hakkında bu ayarla ilgisi olmayan bir söz
   veriyordu.
2. **Mesaj anahtarını çalışma anında birleştirmiştim**
   (`"politika_" + mode`). Ölü-katalog testi üçünü de kullanılmamış
   saydı — ve asıl mesele şu: eksik bir çeviri de aynı sebeple
   yakalanamazdı, sayfa boş bir paragraf çizerdi. Kapalı bir eşlemeye
   çevrildi.

**Bir ayna, onu yazan kişiye de "hayır" diyebiliyorsa aynadır.**

---

## Aynı test, üçüncü kez: iddia ettiğim çıkarımın kendisi yanlıştı

`ddb7eae` dalda geçti, `main`'de düştü — **aynı commit.** Sebep:
*1 of 24 applications timed out on a table.*

İddiam şuydu: "Bir uygulayıcı trafiğe ancak şemanın içindeyken rastlar,
o yüzden tablo beklemesi iki uygulayıcının orada birlikte olduğu
anlamına gelir — ve bu her makinede geçerli."

**Çıkarımın kendisi yanlış.** Tabloyu bekleten trafik başka bir
uygulayıcı olmak zorunda değil: `go test ./...` paketleri paralel
koşturuyor ve `internal/panel`, `internal/logsink`, `internal/storage`
şema dosyalarının kilitlediği tablolara yazıyor. CI yükü altında bir
bekleme 250 ms'yi aştı. Yirmi dörtte bir.

Bu testte üçüncü kez aynı hataya düştüm, ve her seferinde şekil aynıydı:
**kendi makinemin sağladığı bir şeyi genel bir değişmez sandım.**

| sürüm | iddia | nasıl çöktü |
|---|---|---|
| 1 | "üçten fazlası yol veremez" | oran → eşik → makine. CI'da 17/7. |
| 2 | "hiç kimse tabloyu bekleyemez" | çıkarım yanlış: trafik başka paketlerden de gelir |
| 3 | yalnız XX000 / 40P01 | — |

Kalan tek iddia, engellemeye çalıştığı bozulmanın **kendisi**: iki
uygulayıcı bir katalog satırını birlikte yeniden yazdığında çıkan
`tuple concurrently updated` ve `deadlock detected`. Bunlar hızdan
bağımsız, ve kilidi tamamen kaldıran mutasyon üç koşunun üçünde de
üretti.

İki sayaç hâlâ raporlanıyor ama **yargılanmıyor**. Kayıt satırındaki bir
sayı bedava ve arızaya bakan kişiye koşunun neye benzediğini anlatıyor;
aynı sayı bir `if` içindeyken tek bir makineden ayarlanamayan bir eşik.

**Bir sayıyı raporlamak ile ona göre karar vermek arasındaki fark, bu
oturumda üç kırmızı yapıya mal oldu.**

---

## N1+N2 — Panel hiçbir kurulumda günlüğünü açamıyormuş

Gecelik hat, ilk gerçek koşusunda aylardır saklanan bir kusur buldu:

    panel: logging setup failed: mkdir /var/log/crucible: permission denied

Dört servisten üçü açılıyordu, panel açılmıyordu. Yani kurulum "bitti"
diyor ve müşteri panelsiz kalıyordu.

### Kusur `--no-systemd`'ye özgü değildi

İlk okumada "systemd'siz kurulum dizini yaratmıyor" sandım. Daha derine
bakınca ondan büyük çıktı: depoda **iki yol ailesi** yan yana yaşıyordu.

| yol | nerede |
|---|---|
| `/var/log/crucible-analytic` | `install.sh`'ın `LOG_DIR` varsayılanı, **beş systemd biriminin hepsi** |
| `/var/log/crucible` | `panel.example.toml` (**açık satır**), beacon (yorumlu), `KURULUM.md`, preflight'ın kullanıcıya kopyalattığı komut |

Panel, örneği **yorumsuz** `dir` taşıyan tek servis — yani açılışta ağaç
açmayı deneyen tek servis, ve açamayan tek servis. `logging.Setup` hata
döndürüyor, stderr'e düşmüyor, o yüzden etki tam.

**systemd yolu da aynı şekilde düşerdi**, başka bir sebeple:
`ProtectSystem=strict` bütün dosya sistemini salt-okunur yapıyor ve
`ReadWritePaths` yalnız öteki yazımı içeriyor. Yani bu hiç
`--no-systemd` meselesi değildi; bir isim meselesiydi.

Docker yolunun çalışmasının sebebi de kaza: `entrypoint.sh` panelin o
satırını `dir = ""` yapıyordu — kusurun kendisine yazılmış bir baypas,
depoda öylece duruyordu.

### Talimatlar da yanlıştı, ve o yarısı kod düzeltmesinden sağ çıkardı

`KURULUM.md` operatöre bir dizin yaratmasını söylüyordu, panelin kendi
ön kontrolü de aynı yanlış dizini — **kopyalanmak üzere hazırlanmış bir
komutun içinde.** Kodu düzeltip belgeyi bırakmak, kusuru kullanıcının
eline vermek olurdu.

### Planın kendi tahmini yanlış çıktı

N1 ile N2'yi "ayrı kapanabilir" diye ayırmıştım. Ölçünce ayrılmadılar:
panelin satırını yorumlamak `logging.Setup`'ın `Dir == ""` dalına düşmek,
yani B grubunun kurduğu bütün günlük ağacını sessizce kapatmak demek.
**Düzeltme gibi görünen şey özellik kaybıydı.**

Geriye tek düzeltme kaldı: betik günlük dizinini yapılandırmalara yazsın
ve iki dalda da yaratsın. `LOG_DIR` zaten kabul ediliyordu ve hiçbir yere
yazılmıyordu — yani onu verenler, hiçbir servisin açmadığı bir dizin
yaratıyorlardı.

### Bunu hiçbir şey söyleyemezdi

Dosyaların **her biri kendi içinde tutarlıydı.** Tutarsız olan kümeydi, ve
bir kabuk betiği, beş birim dosyası, beş TOML örneği ve bir Markdown
kılavuzu arasında okuyan ne derleyici ne linter ne şema denetimi var.

`TestOneLogDirectoryFamily` tam bunu soruyor, ve elle liste tutmadan:
**bu depo günlük dizinine ne diyorsa, her yerde onu diyor.** Depoyu
yürüyor, `/var/log/crucible*` biçimindeki her yazımı topluyor, birden
fazlaysa kırmızı. Altıncı bir servisin birim dosyası da yazıldığı gün
kapsama giriyor.

Ölçüldü: kök olmayan bir kullanıcı `--no-systemd` ile `LOG_DIR` vererek
kurdu, kurulum tamamlandı, ve `panel.toml` verilen dizini gösteriyor.
Mutasyon: paneli eski yola geri al → iki yazım, kırmızı.

**Bir kurulumun iki ayrı yoldan aynı duruma varabildiği her yerde, iki yol
da ölçülmeli** — bu oturumda dördüncü kez, ve bu sefer iki yol da
kusurluydu.

---

## N4 — 122 karar eşiği, 13'ü zamana dayalı, 12'si sağlam

Bir günde üç kırmızı yapı, hepsi aynı şekilde: bir test, kendi
makinesinde doğru olan bir şeyi her makinede doğru sandı. N4 bunu tek tek
okumak yerine **makineyle çıkardı**.

Tarama basit: AST'den, bir `t.Error`/`t.Fatal` dalını koruyan her
karşılaştırma. **122 karar eşiği** çıktı. Bunların çoğu sayım —
`len(rows) < 5` her makinede aynı cevabı verir. Zamana dayalı olan
**13** tane.

### On ikisi neden sağlam

Üç ayrı sebeple, ve üçü de "eşiği doğru seçmek"ten farklı:

**Göreli.** `elapsed >= 2*delay` — aynı koşudan başka bir ölçümle
karşılaştırılıyor, iki taraf birlikte ölçekleniyor. Makine iki kat
yavaşlarsa ikisi de iki kat büyüyor.

**Kendine referanslı.** `sniff`'in tabanı, testin fonksiyona *verdiği*
deadline. Yani soru "100 ms'den uzun sürdü mü" değil, "verdiğim sınıra
uydu mu".

**Ölçülmüş büyüklük mertebesi.** `devgate`: doğru davranış erken dönüş,
mikrosaniyeler; yanlış davranış on beş gerçek argon2, saniyenin büyük
kısmı; eşik 200 ms — arada üç mertebe var. `logsink`: doğru 3 ms, yanlış
1,88 s, eşik 500 ms. İkincisinin yorumu ayrıca şunu yazıyor: eşik bir
zamanlar 2 s'ymiş, yani **yakalaması gereken arızanın üstünde** —
daha hızlı bir veritabanında bloklayan sink altından geçerdi.

### Düşen tek sınıf benimkilerdi

Ortak özellikleri: **marjları 1'in altındaydı.**

| eşik | yasakladığı | doğru davranışın ürettiği |
|---|---|---|
| duyarlı yarının tabanı 250 ms | 393 ms bekleme | 393 ms |
| "üçten fazlası yol veremez" | 7 | 7 |
| "hiç kimse tabloyu bekleyemez" | 1 | 1 |

Yani üçü de, doğru davranışın gerçekten ürettiği bir sayıyı yasaklıyordu.
Bu bir eşik değil, donanım tahmini.

**Kural:** *bir zaman kararı, izin verdiği davranışla yakaladığı arıza
arasında ölçülmüş bir büyüklük mertebesi varsa güvenlidir.* O boşluk
varsa eşik boşluğun herhangi bir yerinde durabilir ve makinenin on kat
yavaşlaması gerekir. Boşluk yoksa eşik, onu ölçen makineye aittir.

### Ayna, satır değil dosya sayıyor

`TestEveryTimingVerdictIsAccountedFor` türetilen tarafı kaynaktan
okuyor, elle taraf her dosya için **neden güvenli olduğunu** yazıyor.
Satır numarası değil dosya, bilerek: satır numarası bir testin hiçbir
sebep olmadan değişen tek özelliği, ve her paragraf eklendiğinde
güncellenmesi gereken bir ayna, insanların okumadan güncellemeyi
öğrendiği bir aynadır.

Üç mutasyon: girdiyi listeden sil (dosya var, gerekçe yok → kırmızı),
olmayan bir dosyayı listeye ekle (gerekçe var, dosya yok → kırmızı),
açıklanmamış bir dosyaya yeni zaman eşiği ekle (→ kırmızı).

---

## N5 — Konteynerin şema listesi altıda kalmıştı, ve iki bozuk koruma üst üste bindi

Gecelik, tarball yarısı düzelince Docker yarısını gösterdi:

    init-1 | == recording the schema version
    init-1 | ERROR:  relation "schema_version" does not exist
    Container ca-e2e-init-1  service "init" didn't complete successfully: exit 3

`Dockerfile` şema dosyalarını tek tek `COPY` ediyor — `schemafiles.InOrder`'ın
elle yazılmış bir kopyası. **Dokuzuncu kez aynı kalıp**, ve bu sefer en
pahalısı: kopya altıda kalmış, şema ona çıkmıştı. Eksik dördü kayıt
sinki, yükseltme kuyruğu, yenileme kuyruğu ve şema sürümü satırı — yani
her konteyner kurulumu, her biri eklendiği günden beri onlarsız, ve
yığın hiç ayağa kalkmıyor.

### Önce kendi yanlış teşhisimi düzeltmem gerekti

Bir önceki turda "Docker'ı ben kırdım, `STATE_DIR`'i yeniden
adlandırdım" demiştim. **Yanlıştı.** Gecelik #3'ün log'unda da aynı
`schema_version` hatası vardı ve `mkdir`/izin hatası hiç yoktu — yani
kırık zaten oradaydı, ben kendi değişikliğimi gördüğüm kırmızıya
ölçmeden yapıştırdım.

Tam olarak bu oturumda dört kez uyardığım şey. Kırmızı bir yapıya bakan
kişi, en son ne yaptığını hatırlar ve sebebi orada arar; **son
değişiklik, en görünür şüphelidir, en muhtemel değil.**

(`STATE_DIR` düzeltmesi yine de doğruydu — imajın yaratmadığı bir yolu
göstermek her hâlükârda yanlış — ama testi kıran o değildi ve ben o
dedim.)

### İki koruma aynı anda bozuktu, ve dıştaki içtekini sakladı

Bu dosyalar yalnız `docker` etiketiyle koşuyor, o da yalnız gecelikte.
Gecelik ise kendi ilk işini geçemiyordu, çünkü `e2e`, `install.sh`'ı
`--no-systemd` olmadan çağırıyordu.

Yani boşluğu bildirecek hat, boşluk açılmadan **önce** başka bir sebeple
düşüyordu. Sonuç: L1–L3, M3 ve kayıt sinki eklendi, hiçbiri imaja
girmedi, ve hiçbir şey ses çıkarmadı.

**Hiç koşmayan bir koruma, zayıf bir koruma değildir; korumanın
yokluğudur — ve dışarıdan ikisi aynı görünür.**

### Doğrulama: bu konteynerde yapılabiliyormuş

İlk denememde imaj yapımı `apk add`'de düştü ve "burada doğrulayamam"
dedim. Doğruydu ama eksikti: Dockerfile TLS kesen bir vekil için zaten
hazırlıklı (`extra_ca` derleme sırrı), ve testin kendi yardımcısı
`CA_BUILD_EXTRA_CA` görürse hem sırrı hem `--network host`'u geçiriyor.

    CA_BUILD_EXTRA_CA=/root/.ccr/ca-bundle.crt \
      go test -tags docker -count=1 -timeout 30m ./e2e/

İkisi de yerelde yeşil — imajdaki on şema dosyası sayıldı, compose
yığını ayağa kalktı, istek panele ulaştı. Bu yol artık `CONTRIBUTING.md`'de
yazılı; aranması gereken bir şey olmaktan çıktı.

**"Doğrulayamıyorum" ile "doğrulamayı denemedim" arasındaki fark, bir
ortam değişkeni kadarmış.**

---

## N7 — Yığın çalışıyordu, test panoyu bir kez okuyordu

Gecelik #5'te konteyner yarısı kırmızı geldi, ve bu sefer şema değildi —
N5 tutmuştu. Altı panodan dördü sayı taşıyordu, ikisi boştu:

    İnsan trafiği = Bu site için henüz hiç bağlantı kaydı yok…  (empty)
    Bot trafiği   = …                                          (empty)

Collector isteği vekillemiş, kökenin gövdesini döndürmüştü; testin kendi
iddiası bunu üç satır önce geçmişti. Yani **ürün çalışıyordu.**

### Panoyu iki farklı saat besliyor

| kart | yazan | aralık |
|---|---|---|
| Ziyaretçi, Sayfa görüntüleme, Oturum, Hemen çıkma | beacon | 2 sn |
| İnsan trafiği, Bot trafiği | collector | **10 sn** |

Beacon 2 saniyede bir (ya da 500 satırda) yazıyor; collector hız
deposunu süreç açılışında başlayan bir ticker'la özetliyor. Yani istek
ile satır arasında **sıfır ile bir aralık arası** gecikme var ve hiçbir
yerde arıza yok. Tek okuma, bu aralığın neresine düştüğünü soran bir
yazı-tura — ve yazıyı da turayı da gördüm.

### Ölçtüm, çünkü aritmetik tahmindi

CI kütüğündeki zaman damgalarından "yarım saniyeyle kaçırmış" diye bir
hesap çıkarabiliyordum. Çıkarabilmek, doğru olması değil. Panel oturumunu
isteğin *önüne* alan bir prob yazdım, 250 ms'de bir yokladım:

    +30 ms    altı kart boş
    +1.38 sn  iki kart boş      ← beacon'ın dördü
    +9.39 sn  hiçbiri boş değil ← collector'ün ikisi

Yarım saat önce aynı probun tek okuması **+5.38 sn**'de her kartı dolu
bulmuştu. **Tek makine, tek kod, iki sonuç.** Aradaki fark yalnızca
isteğin ticker döngüsünün neresine düştüğü.

Bu, N4'te kataloglanan sınıfın bir üyesi, ama bir eşik değil — eşik bile
yoktu. Bekleme yoktu.

### Neden tarball yarısı hiç düşmedi

Çünkü o, paneli açmadan önce satırın kendisini bekliyor: süperkullanıcı
bağlantısı var, `traffic_snapshots`'a `flushWait` (30 sn) boyunca soruyor.
Konteyner yolunda öyle bir bağlantı **yok ve olmamalı** — compose dosyası
bilerek hiçbir veritabanı portu yayımlamıyor. İzleyebildiği tek şey
sayfanın kendisi, ve onu tam bir kez okuyordu.

**İki kardeşten sonra yazılanı, birincinin pahalıya öğrendiğini
devralmamış.** Bu, aynı dosyada bir fonksiyon ötede zaten yazılı:
`install()`'ın `--no-systemd` yorumu, kapının ilk günden taşıdığı bayrağın
sonradan yazılan yolda eksik olduğunu anlatıyor. Aynı yara, üçüncü kez.

### Tespit değil, imkânsızlaştırma

Önce `TestOneWayToReadADashboard`'ı "boş kart kontrolü tek dosyada
olsun" diye yazacaktım. Sonra fark ettim ki bu, **bir tür sorununu bir
testle kovalamak**: iki suit de kendi `strings.HasSuffix(line,
"(empty)")` kopyasını taşıyordu, çünkü `cardLines` string döndürüyordu.
Alan yaptım:

    type card struct { title, value string; filled bool }

Kimsenin farklı yazamayacağı bir bool, farklı yazılabilen bir soneki
kovalayan bir testten iyidir. Test yine de kaldı, ama artık başka bir şeyi
koruyor: **`e2e/` içinde `"/site/"` yazan tek dosya `shared_test.go`
olabilir.** Yani üçüncü bir dağıtım yolu, panoyu bekleyen tek yoldan
geçmek zorunda. Liste değil, kaynaktan türetme — çünkü listeye adı
ekleyecek kişi, bu kusuru tarih olarak bilmeyen kişidir.

### İki mutasyon, ikisi de gerekliydi

**M1** — `docker_test.go`'ya doğrudan bir `getPage(…"/site/"…)` koydum.
Ayna kırmızı verdi ve dosyayı adıyla söyledi. Mutasyonun uygulandığını
`grep` ile ayrıca doğruladım; bu oturumda bir kez, uygulanmamış bir
mutasyonun "geçti" demesine kanmıştım.

**M2** — asıl soru buydu: *bekleme, iddiayı köreltti mi?* Vekillenen
isteği tamamen kaldırdım, yani hiçbir trafik satırı oluşamaz. Otuz saniye
bekleyip yine kırmızı vermeliydi.

Bir beklemeyi test etmenin doğru yolu, beklemeyi kısaltmak değil —
`flushWait = 0` bu kusurda kırmızı **veya** yeşil verirdi, çünkü sonucu
yine yazı-tura belirlerdi. **Sonucu rastgele olan bir mutasyon, mutasyon
değildir.** Beklenen şeyi ortadan kaldırmak gerekiyordu, beklemeyi değil.

### Kendi düzeltmemin götürdüğü şey

M2'nin tam kütüğüne bakınca yan etkiyi gördüm: pano okuması artık bir
yoklama, altmışa kadar istek, ve panel her birini kaydediyor. Başarısız
koşuda basılan `docker compose logs --tail 60` dökümünde **panelin sağ
kalan altmış satırının altmışı da bu testin kendi trafiğiydi.** Panelin
açılışta söylediği her şey dışarı itilmişti.

Collector'ün satırları kurtulmuştu, çünkü `--tail` servis başına
çalışıyor — yani sandığım kadar kötü değildi, ama panelinki gerçekten
gitmişti. `--tail 200`. Döküm yalnız kırmızıda basılıyor, yani büyük
sayının bedeli tam da ayrıntının istendiği anda ödeniyor.

**Bir yoklama, gürültüsünü teşhisin üstüne yazar.** Düzeltmenin kendisi
doğruydu; ölçmeseydim, ilk gerçek arızada elimde altmış satır kendi
GET'im olacaktı.

### Doğrulama

| ne | sonuç |
|---|---|
| `-tags docker` (düzeltmeyle) | ✅ altı kartın altısı da sayı taşıyor, İnsan trafiği = 1 |
| `-tags docker` + M2 (istek yok) | ✅ **kırmızı**, "2 dashboard cards have nothing in them after waiting 30s" |
| `-tags e2e` (gerçek TimescaleDB) | ✅ satır → API → pano, JA4 dâhil |
| `go test -race ./...` | ✅ temiz |
| `TestOneWayToReadADashboard` + M1 | ✅ kırmızı, dosyayı adıyla söyledi |

M2 kırmızısı CI'daki belirtinin birebir aynısı — aynı iki kart, aynı
Türkçe cümle — ama artık otuz saniye bekledikten sonra, ve mesaj bunu
söylüyor. Gecelikteki kırmızının eksik olan yarısı tam olarak buydu:
"boş" diyordu, "ne kadar bekledikten sonra boş" demiyordu.

---

## N6a — Doğru olan ama yanlış teşhis koyan bir mesaj

Collector, bot verisi yokken şunu yazıyordu:

    "bot data has never been fetched; the known-bot signal is off"
    path=/var/lib/crucible/known_bots.json
    how=run: collector -config <file> -update-bot-data

Cümlenin dosya hakkındaki kısmı doğru. **Sebep hakkındaki kısmı yanlış**,
ve asıl zararı veren o: systemd biriminde `ProtectSystem=strict` var ve
`ReadWritePaths` yolu öteki yazımla listeliyor, yani o dosya oraya
yazılamıyor. Müşteri önerilen komutu koşturuyor, komut düşüyor, sonraki
açılışta yine "hiç çekilmedi" yazıyor — ve bilinen-bot sinyali sessizce
kapalı kalıyor.

**Yoklama, çünkü izin kontrolü bu kusuru göremez.** Dizinin kipi 0755,
sahibi bu kullanıcı, her şey yolunda görünüyor; salt-okunur olan
altındaki mount. `stat`, `unix.Access`, sahiplik — hepsi "yazılır" diyor.
Yazmayı denemekten kısa hiçbir kontrol doğru cevabı veremiyor.

    func Writable(path string) error   // MkdirAll, CreateTemp, Remove

Dosyayı değil dizini yokluyor, ve bu kestirme değil `Save`'in gerçekten
ihtiyaç duyduğu şey: bir dosyayı rename ile değiştirmek dizinde yazma
izni ister, dosyanın kendisinde değil.

### Nereye koyduğum, neden orası

`cmd/collector` içinde `Save`'in adımlarının bir kopyası olabilirdi.
Olmadı: **sahip olmadığı bir mekanizma hakkında soru cevaplayan kod, o
mekanizma değiştikten sonra da kendinden emin cevap vermeye devam eder.**
`Save`'in yanına, ilk iki adımını paylaşarak koydum, ve
`TestWritableAgreesWithSave` ikisini birbirine bağladı: altı düzenleme,
her birinde ikisi de başarılı ya da ikisi de başarısız.

Testin şekli "Writable şu durumlarda hayır demeli" değil, **"ikisi
anlaşmalı"** — çünkü yanlış cevap cevapsızlıktan kötü. İyimser bir
Writable eski yanlış teşhisi geri getirir; kötümser olanı çalışan bir
kuruluma "yazamıyorsun, komut da yardım etmez" der. İki yönde de kırmızı.

### Testimin ilk hâli yanlış şeyi ölçüyordu

İlk fikstürüm "ebeveyn bir dosya olsun"du — root'ta da çalışsın diye.
Test kırmızı verdi ve haklıydı: o düzenlemede `os.ReadFile` ENOTDIR
veriyor, ENOENT değil, yani `Load` hata döndürüyor ve akış benim yeni
dalıma **hiç gelmiyor**. Bir dal önce duruyor.

Gerçek durum tam olarak şu: dosya yok (ENOENT), dizin yazılamaz. Bunu
root'ta üretmenin yolu yok — root her kipi yürüyor. Yani o vaka root'ta
atlanmak zorunda, ve **kendi varlık sebebini atlayan bir test, koşmayan
bir korumadır.** `nobody` olarak koşturdum; ikisi de geçti. CI zaten
ayrıcalıksız koşuyor, orada her koşuda görülecek.

### Mutasyonlar

| ne | sonuç |
|---|---|
| Writable her zaman `nil` | ✅ üç yazılamaz vaka "said yes and Save then failed" |
| Writable `MkdirAll`'ı atlar | ✅ "henüz olmayan dizin" vakası "said no and Save succeeded" |
| Writable geçici dosyayı bırakır | ✅ "left [.botdata-probe-…] behind" |
| Yeni dal tamamen kaldırılır | ✅ eski satır kelimesi kelimesine geri geldi |

Sonuncusu en anlamlısı: mutasyon, gecelik kütüğündeki cümlenin aynısını
üretti — yazılamayan bir yolda, çalışmayacak komutu öneren INFO satırı.

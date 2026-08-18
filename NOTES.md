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

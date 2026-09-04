package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// Settings are the operational values a deployment can change while it
// is running, as opposed to the handful of bootstrap values that must
// exist in a file before the database is reachable.
//
// The split is not about convenience. Every setting that lives here is
// one a support call can fix from the panel; every setting that stays in
// the config file is one that needs SSH. That is the whole argument for
// this table, and it is why the division is drawn by "can this honestly
// take effect while running" rather than by what is easier to implement.
//
// Two rules hold throughout, and both are about not trusting input:
//
//   - A key is a value from a closed set, never a free string. Unknown
//     keys are rejected rather than stored, so a typo is an error at the
//     moment it is made rather than a setting that silently never
//     applies.
//   - A value is validated against explicit bounds before it is written,
//     not when it is read. A stored value that no reader will accept is
//     a trap laid for whoever restarts the service next.

// Scope says whether a setting is global to the deployment or belongs to
// one site.
type Scope string

const (
	// ScopeGlobal applies to the whole deployment.
	ScopeGlobal Scope = "global"
	// ScopeSite applies to one site, and falls back to the global value
	// when unset.
	ScopeSite Scope = "site"
)

// Kind is a setting's value type, which decides how it is validated and
// how the panel renders it.
type Kind string

const (
	KindInt        Kind = "int"
	KindBool       Kind = "bool"
	KindString     Kind = "string"
	KindEnum       Kind = "enum"
	KindStringList Kind = "string_list"
)

// Key names one setting. A closed set: see the registry below.
type Key string

// The log lifecycle settings.
//
// Logs are the fastest-growing thing this project writes, and a full
// disk stops the collector - which means an analytics feature taking
// down the traffic path, the failure this project refuses everywhere
// else. So their lifecycle is not one number but three stages, and the
// stages exist because not all logs are equally worth keeping.
//
// access and ingest are enormous and interesting for about a week.
// security, auth and audit are small and are exactly what somebody asks
// for a year later, when the question is "who got in, and when". A
// single retention figure would either throw away the second group or
// keep the first group forever.
const (
	// KeyLogRetentionDays is how long an ordinary category is kept
	// before its files are deleted.
	KeyLogRetentionDays Key = "logs.retention_days"
	// KeyLogImportantRetentionDays is how long the categories that
	// matter later are kept: security, auth and audit.
	KeyLogImportantRetentionDays Key = "logs.important_retention_days"
	// KeyLogArchiveAfterDays is when a day's files are compressed in
	// place instead of being left as plain text. Compression is the
	// middle stage between "readable immediately" and "gone": a gzipped
	// day is roughly a tenth of the size and still readable, so keeping
	// a year of security logs costs about as much as keeping five weeks
	// of them uncompressed.
	KeyLogArchiveAfterDays Key = "logs.archive_after_days"
	// KeyLogLevel is the minimum level written.
	KeyLogLevel Key = "logs.level"
	// KeyUpgradeLocked holds the upgrade button behind the developer
	// password.
	//
	// Off by default, and that is the decision rather than an oversight:
	// the customer is meant to be able to press it without knowing
	// anything, because "işi bilmeyen normal müşteri de yapabilmeli" is
	// the requirement. A developer who does not want that - a support
	// contract, a deployment they are responsible for - turns this on.
	//
	// Changing it needs the password, for the obvious reason: a lock a
	// customer can unlock is not one.
	KeyUpgradeLocked Key = "upgrade.locked"

	// KeyReleaseUpdateLocked holds the *binary* update behind the
	// developer password.
	//
	// On by default, which is the opposite of KeyUpgradeLocked, and the
	// difference is the honest statement of which operation is riskier.
	// A schema upgrade runs against a database that keeps serving. A
	// release update replaces the collector, which stands in front of
	// the customer's website - and the panel that would let somebody
	// undo it may be down beside it.
	//
	// So this one starts shut. A developer who wants their customer to
	// be able to update without asking - "bize muhtaç olmasınlar" -
	// turns it off, deliberately, with the password in their hand.
	//
	// Like the schema lock, it needs the password to move in either
	// direction: a lock a customer can unlock is not one, and a lock a
	// customer can *close* would shut the developer's own button.
	KeyReleaseUpdateLocked Key = "release.locked"

	// The IP range dataset choices. Options come from internal/ipsources
	// at init() - see sourceSettings below.
	KeySourceCountry  Key = "sources.country"
	KeySourceASN      Key = "sources.asn"
	KeySourceFallback Key = "sources.fallback_order"

	// KeyLogVerboseUntil is when a temporary raise to debug expires.
	// Stored as an RFC3339 timestamp, empty when not raised.
	//
	// Self-expiring rather than a plain toggle: verbose logging left on
	// because somebody forgot is the way the disk fills.
	KeyLogVerboseUntil Key = "logs.verbose_until"

	// KeyDevAccessPolicy is what happens when the developer asks to come
	// in: wait for the owner, refuse outright, or let them in.
	//
	// The customer's to set, which is the opposite of this project's
	// usual rule. "Anything that can make work for the developer sits
	// behind the developer password" exists so a customer cannot grant
	// themselves authority and then bill somebody for the consequences.
	// This is the other direction entirely: the customer is protecting
	// themselves, and a protection they cannot reach is not one.
	//
	// It is also not a real lock, and saying so plainly matters more
	// than the setting does. A developer with a shell on the machine is
	// already inside; what closes here is the panel's door, not the
	// server's.
	KeyDevAccessPolicy Key = "access.developer"

	// KeyDevAccessOpenUntil is when an "open" policy stops being open.
	// An RFC3339 timestamp, empty when not open.
	//
	// The same shape as KeyLogVerboseUntil and for the same reason, one
	// notch more serious: a door propped open because somebody meant to
	// close it after the call is the failure this whole phase is about.
	// An expired or missing timestamp falls back to asking the owner
	// rather than to refusing - the safe direction here is the one that
	// still lets a decision be made, not the one that locks everybody
	// out of their own deployment.
	KeyDevAccessOpenUntil Key = "access.developer_open_until"
)

// The developer access policies, as stored.
const (
	// DevAccessAsk holds the request until the owner decides. The
	// default, and what the deployment did before this was settable.
	DevAccessAsk = "ask"
	// DevAccessDeny refuses on arrival, with the reason on the page.
	DevAccessDeny = "deny"
	// DevAccessOpen approves on arrival, until KeyDevAccessOpenUntil.
	DevAccessOpen = "open"
)

// The settings a running service applies without a restart.
//
// Live-applicable is a property of the setting, not a preference: a
// campaign policy is a pure function and can be swapped between
// requests, while a buffer size is a channel capacity fixed when the
// channel was made. The panel has to say which is which, or a customer
// changes a value and cannot tell why nothing happened.
const (
	// KeyBeaconSites is the beacon's allowlist. Live.
	//
	// The most common support call this makes answerable without SSH: a
	// customer adds a second domain.
	KeyBeaconSites Key = "beacon.sites"
	// KeyCampaignDropParams removes standard query parameters. Live.
	//
	// The one that motivated all of this: when a customer's counsel says
	// utm_term must not be stored, the answer should be a panel setting
	// rather than a release.
	KeyCampaignDropParams Key = "campaign.drop_params"
	// KeyCampaignExtraParams keeps additional parameters. Live.
	KeyCampaignExtraParams Key = "campaign.extra_params"
	// KeyCampaignStoreClickID keeps raw ad click identifiers. Live.
	KeyCampaignStoreClickID Key = "campaign.store_click_ids"
)

// The two most expensive misconfigurations in this system, moved out of
// the config file in A5.1.
//
// Both were chosen from the repair catalogue's own evidence rather than
// from a guess about what would be convenient.
const (
	// KeyTrustedProxies lists the networks whose forwarded headers the
	// beacon believes. Live.
	//
	// Named for the beacon rather than left generic, matching
	// beacon.sites above. The prefix is not decoration: it says which
	// process reads the value, and a key that does not say cannot be
	// answered honestly when a second service grows the same concept.
	//
	// The catalogue says this one deserves the top of the list, and it
	// is right: an empty list behind Cloudflare makes every visitor look
	// like the same address, and that does not merely lose the address -
	// it makes **every other number in the system wrong at the same
	// time**, because visitor counts, geography and the crossover join
	// are all derived from it. It is the most common real
	// misconfiguration there is, and until now it cost an SSH session.
	//
	// Stored as text because a database column is text; never *treated*
	// as text. Check parses every entry as a netip.Prefix before the
	// value is written, and the service parses again before it is used -
	// so a row edited by hand into something that is not a network
	// cannot widen who gets believed.
	KeyTrustedProxies Key = "beacon.trusted_proxies"
)

// The settings that stop an attack, moved out of the config file in
// A5.2.
//
// Chosen by what a support call actually asks for rather than by what
// was easiest to wire. "We are being hit from there, block it" is the
// request, and until now the answer was SSH, an edit and a restart - the
// longest possible path while an attack is in progress.
//
// All four are pure data consulted per connection or per flush, which is
// what makes them honestly live. The rest of [asn_lookup] is not:
// enabling the lookup loads a hundred-odd megabytes of range tables and
// the cache size is fixed when the LRU is built, so those stay in the
// file and the panel says so rather than offering a control it could not
// honour.
const (
	// KeyBlockedCountries lists ISO 3166-1 alpha-2 codes to reject
	// outright. Live.
	//
	// Rejection here is unconditional and independent of the overload
	// policy: blocking by geography is a deliberate decision, not load
	// shedding. An address the lookup could not resolve is never blocked
	// by it - a rule needs a value to match against.
	KeyBlockedCountries Key = "collector.blocked_countries"
	// KeyBlockedASNs lists autonomous system numbers to reject. Live.
	KeyBlockedASNs Key = "collector.blocked_asns"
	// KeyKnownBotASNs lists networks whose traffic gets a bot-score
	// bonus. Live.
	//
	// Separate from KeyBlockedASNs and deliberately so: a blocked network
	// never reaches scoring at all, so one list cannot serve both. This
	// one marks traffic; that one refuses it.
	KeyKnownBotASNs Key = "collector.known_bot_asns"
	// KeyApplyASNToScoring turns the signal above on. Live.
	//
	// Off by default, because an ASN bonus applied to a deployment that
	// never chose one would change every score in the table for a reason
	// nobody could point at.
	KeyApplyASNToScoring Key = "collector.apply_asn_to_scoring"
)

// The admission limits, per service. Catalogue #8, #9 and #10.
//
// **Per service, and that is not verbosity.** The first draft of this
// made them one family read by both, which would have been a setting
// that cannot mean what it says: the collector sees every connection to
// the site, the beacon sees only the requests from visitors whose
// browser ran the snippet. A ceiling that is right for one is wrong for
// the other by an order of magnitude, and one number covering both would
// be a number that is wrong somewhere no matter what it is set to.
//
// The prefix convention is already the one beacon.sites uses: the name
// says which process reads it.
const (
	KeyCollectorMaxConcurrent  Key = "collector.limits.max_concurrent"
	KeyCollectorMaxPerSecond   Key = "collector.limits.max_requests_per_second"
	KeyCollectorOverloadPolicy Key = "collector.limits.overload_policy"
	KeyCollectorThrottleQueue  Key = "collector.limits.throttle_queue_size"

	KeyBeaconMaxConcurrent  Key = "beacon.limits.max_concurrent"
	KeyBeaconMaxPerSecond   Key = "beacon.limits.max_requests_per_second"
	KeyBeaconOverloadPolicy Key = "beacon.limits.overload_policy"
	KeyBeaconThrottleQueue  Key = "beacon.limits.throttle_queue_size"
)

// The overload policies, mirroring limiter.Policy.
//
// Named here rather than imported so the panel's registry does not drag
// in a package that belongs to the traffic path. The two lists agreeing
// is asserted by a test.
const (
	OverloadFailOpen   = "fail_open"
	OverloadFailClosed = "fail_closed"
	OverloadThrottle   = "throttle"
)

// # The analytics lifecycle settings are not here, deliberately
//
// How long visit records are kept used to be two keys in this registry -
// analytics.retention_days and analytics.compress_after_days - shown
// behind the developer password. Both are gone, for two different
// reasons worth keeping written down.
//
// Retention moved to the services' config files, where changing it means
// reaching the server. Everything else in this registry is operational:
// a wrong value costs performance, accuracy or disk. Retention is the one
// with legal weight - it is the direct subject of KVKK's "no longer than
// the purpose needs" - and the developer password was a lock on the door
// of a room the customer was still standing in. The value was visible,
// editable over HTTP, and one leaked password away from being somebody
// else's decision. See internal/beacon.Config.RetentionPolicy.
//
// Compression was removed because nothing read it. It had a label, help
// text and a password gate, and no service anywhere in this repository
// ever looked the key up: a customer could change it, the panel would
// record the change in the audit log, and TimescaleDB would go on
// compressing exactly as before. A setting that does nothing is worse
// than a missing one, because it is believed. If chunk compression is
// worth having it needs a reader first, and then it can come back.

// The panel's own presentation settings.
//
// These carry no legal weight and no service reads them, so they are the
// one family the customer changes freely: what their site is called, and
// which clock the numbers are read against. Both exist because the
// person who installed the deployment is not the person who lives in it.
const (
	// KeySiteName is what a site is called in the panel.
	//
	// Site-scoped, and deliberately separate from the site *id*, which
	// is the beacon's allowlist entry and appears in a public snippet.
	// Renaming a site must never mean re-editing a snippet on somebody
	// else's website.
	KeySiteName Key = "panel.site_name"

	// KeyPanelTimezone is the zone every date and time renders in.
	//
	// A setting rather than only a config-file value because the
	// customer knows their own timezone better than whoever installed
	// the deployment does, and getting it wrong is not cosmetic: a panel
	// showing the evening traffic peak in UTC tells a shop in Istanbul
	// that it happened in the afternoon.
	//
	// Empty means "use what the config file says", which is the state
	// every deployment starts in.
	KeyPanelTimezone Key = "panel.timezone"

	// KeyVisibleCards and KeyVisibleBreakdowns are which blocks a site's
	// page shows.
	//
	// # Why this is a setting at all
	//
	// The person who buys a website does not know what a TLS fingerprint
	// is, and does not want to. Deciding for every customer that they
	// see the same twelve blocks is deciding that most of them see
	// several they cannot read - and a page carrying a number somebody
	// cannot interpret is worse than a page without it, because it
	// invites a wrong conclusion rather than no conclusion.
	//
	// So the installer asks what this customer wants and turns those on.
	// The rest stay off until somebody asks for them.
	//
	// # Empty means the default, never a blank page
	//
	// An unset row is every deployment that existed before this setting
	// did, and a page that went blank on upgrade would be the worst
	// possible reading of "not configured". It is also the rule the
	// dashboard already follows one level down: a view is never hidden,
	// because a customer who cannot see what they are paying for never
	// finds out they have it.
	//
	// The cost of that rule, stated because it is a real limit rather
	// than an oversight: a deployment cannot select *nothing*. The
	// smallest expressible answer is one block. A section with nothing
	// behind it already says so in its own words, which is the honest
	// version of hiding it.
	//
	// # The values are ids, and the closed set is not here
	//
	// Both hold ids from registries this package cannot see: the cards
	// live in internal/panel/web and the breakdown kinds in
	// internal/panel/analytics, and neither may import back into this
	// one. Copying the lists here to validate against would create a
	// second source of truth for a closed set - the exact failure the
	// registries exist to prevent - so the check lives at the write
	// path, next to the registry it checks, and the page looks every id
	// up before using it.
	KeyVisibleCards      Key = "panel.cards"
	KeyVisibleBreakdowns Key = "panel.breakdowns"
)

// The privacy settings.
const (
	// KeyPrivacyIPStorage is whether stored addresses are whole or
	// masked. "masked" by default, on legal advice.
	//
	// Masking is applied when the row is written, and as the last step
	// before writing: the whole address derives the visitor id and
	// resolves country and ASN first, then is masked. In the other order
	// masking would quietly degrade geography and visitor counts too,
	// and nothing would say so.
	KeyPrivacyIPStorage Key = "privacy.ip_storage"
)

// The IP storage modes.
const (
	// IPStorageFull keeps full precision without keeping the address:
	// the masked network plus a keyed token of the whole one. Needs the
	// key to be in the config file already.
	IPStorageFull = "full"
	// IPStorageMasked keeps IPv4 to /24 and IPv6 to /64.
	IPStorageMasked = "masked"
)

// Definition describes one setting: what it is, what values it admits,
// and what it is called in the panel.
type Definition struct {
	Key   Key
	Scope Scope
	Kind  Kind
	// Default is what a deployment gets before anyone changes anything.
	Default any
	// Min and Max bound an int. Both zero means unbounded, which no
	// current setting is.
	Min, Max int
	// Enum lists the admissible values of a KindEnum setting.
	Enum []string
	// Check is an extra validator for values the Kind cannot describe.
	//
	// KindString means "some text", which is right for a site's name and
	// useless for a timezone: "Europe/Istanbul" and "Avrupa/İstanbul"
	// are both text and only one of them is a zone. Rather than adding a
	// Kind per such value - each with a parser the settings package
	// would then own - the definition carries the one function that
	// knows. It runs after the Kind's own checks, on the canonical form,
	// and returns a sentence the panel can show.
	//
	// Nil means the Kind's checks are the whole rule.
	Check func(any) error
	// Label and Help are Turkish, because they are read in the panel.
	Label string
	Help  string
	// Developer marks a setting shown behind the developer-mode toggle.
	//
	// Grouping and visibility, not permission. A customer may turn
	// developer mode on and reach every one of these; what stops them
	// changing one is never this flag, but one of the two below. Reading
	// it as a permission was a mistake worth naming: it made a technical
	// setting and a legally sensitive one look identical to the code,
	// when the only thing they share is that a shop owner does not want
	// them on the front page.
	Developer bool
	// ConfigFileOnly marks a setting that lives in a service's config
	// file rather than the settings table, and therefore cannot be
	// changed from the panel by anybody - the operator included.
	//
	// Shown anyway, and that is the point: a value nobody can see is a
	// value nobody can ask about, and the customer is left unable to
	// account for their own deployment. What the panel offers here is
	// the value and where it lives, not a control it could not honour.
	ConfigFileOnly bool
	// Live marks a setting a running service applies without a restart.
	//
	// The panel shows this, because a customer who changes a value and
	// sees nothing happen will assume the panel is broken rather than
	// that the setting needs a restart.
	Live bool
	// RequiresDeveloperPassword marks a setting that carries legal
	// weight: one that changes what personal data is stored, or for how
	// long. Changing it needs the developer password from the config
	// file, every time - see internal/devgate.
	//
	// Being in developer mode is not enough. That says who you are;
	// this asks whether you are entitled to make this particular change,
	// and the answer has to come from somebody with access to the
	// server rather than to the panel.
	RequiresDeveloperPassword bool
	// Category is the section this setting appears under.
	//
	// Required. The zero value is not a category, and
	// TestEverySettingIsInACategory refuses it: before D4c the page was
	// one flat list, so a new definition needed no group and got shown
	// regardless. Now a definition with no category would be drawn
	// under no heading - which is to say, not drawn.
	Category Category
	// GateReason is why this particular setting is guarded, in Turkish.
	//
	// Per setting rather than one blanket sentence, because "this needs
	// a password" invites the person to look for the password, while
	// "this decides whether whole IP addresses are stored" tells them
	// what they are about to do. The second is what makes the prompt
	// worth interrupting somebody with.
	GateReason string
}

// Category groups settings on the page.
//
// A closed type rather than a string, and the reason is the one D4c was
// written for: the page is about to stop being one list, and a setting
// whose group is a free-form string can be given a group that does not
// exist - which renders as nothing at all. A typo would hide a setting
// rather than misplace it, and a hidden setting is the failure this
// whole phase is trying not to introduce.
type Category string

const (
	// CatGorunum is what the customer sees, and it is first because it
	// is what somebody opening this page most often came for.
	CatGorunum Category = "gorunum"
	// CatToplama is what gets collected at all.
	CatToplama Category = "toplama"
	// CatBot is the bot and traffic policy.
	CatBot Category = "bot"
	// CatGizlilik holds the settings with legal weight: what is stored
	// about a visitor, and for how long. Grouped together deliberately -
	// they are the ones somebody has to find and account for when a
	// customer or a regulator asks, and answering "where is that
	// configured" by naming four sections is not an answer.
	//
	// The three campaign settings are in here rather than in a section
	// of their own, and that placement was a correction rather than a
	// choice. They read as marketing configuration; their own
	// GateReason says otherwise - "ham tıklama kimliği ... reklam ağının
	// kayıtlarıyla eşleştirilebilen kalıcı bir tanımlayıcıya dönüşür".
	// TestTheLegallyWeightySettingsAreTogether is what said so, and it
	// said it before the page was ever drawn.
	//
	// A setting belongs here because of what it stores, not because of
	// what it is called.
	CatGizlilik Category = "gizlilik"
	// CatSinirlar is admission control: how much each service accepts
	// before it sheds load.
	CatSinirlar Category = "sinirlar"
	// CatTanilama is diagnostics: what gets logged, how loudly.
	CatTanilama Category = "tanilama"
	// CatBakim is maintenance.
	CatBakim Category = "bakim"
	// CatErisim is who may reach this deployment's panel, and on whose
	// say-so. One setting today - the developer access policy - and it
	// is here rather than under maintenance because a customer looking
	// for "can somebody else get in" is not looking for housekeeping.
	CatErisim Category = "erisim"
)

// CategoryOrder is the order the page draws them in.
//
// A slice, because Go map iteration is deliberately random and a
// settings page whose sections move between reloads is one nobody can
// build a habit on. The order is by how often somebody wants each, not
// alphabetical.
//
// It is also the closed set: TestEverySettingIsInACategory checks both
// directions against it, so a category constant that is never used and a
// setting pointing at a category that is not here both fail.
var CategoryOrder = []Category{
	CatGorunum, CatToplama, CatBot, CatGizlilik, CatSinirlar, CatTanilama, CatBakim,
	CatErisim,
}

// registry is every setting this system has. Adding one is a code change
// that goes through review, which is the same rule the panel's repair
// operations follow and for the same reason: a running system should not
// be talkable into a setting nobody designed.
var registry = map[Key]Definition{
	KeySiteName: {
		Key: KeySiteName, Scope: ScopeSite, Kind: KindString,
		Category: CatGorunum,
		Default:  "",
		Label:    "Sitenin adı",
		Help:     "Panelde görünen ad. Site kimliğini değiştirmez — snippet olduğu gibi kalır.",
	},
	KeyVisibleCards: {
		Key: KeyVisibleCards, Scope: ScopeSite, Kind: KindStringList,
		Category: CatGorunum,
		Default:  []string{},
		Label:    "Gösterilecek kartlar",
		Help: "Bu sitenin panosunda hangi özet kartlarının görüneceği. Boş " +
			"bırakılırsa varsayılan altısı gösterilir — boş, \"hiçbiri\" demek " +
			"değil. Kapalı bir kartın verisi analitik servisinden hiç istenmez.",
	},
	KeyVisibleBreakdowns: {
		Key: KeyVisibleBreakdowns, Scope: ScopeSite, Kind: KindStringList,
		Category: CatGorunum,
		Default:  []string{},
		Label:    "Gösterilecek kırılımlar",
		Help: "Kartların altındaki hangi kırılım tablolarının görüneceği: sayfalar, " +
			"kaynaklar, kampanyalar, cihazlar, ülkeler, olaylar. Boş bırakılırsa " +
			"hepsi gösterilir. Kapalı bir kırılımın sorgusu hiç atılmaz, yani " +
			"sayfa da o kadar hızlanır.",
	},
	KeyPanelTimezone: {
		Key: KeyPanelTimezone, Scope: ScopeGlobal, Kind: KindString,
		Category: CatGorunum,
		Default:  "",
		Label:    "Saat dilimi",
		Help:     "Paneldeki her tarih ve saat bu dilimde gösterilir. Boş bırakılırsa yapılandırma dosyasındaki değer geçerli olur.",
		Check:    checkTimezone,
	},
	KeyBeaconSites: {
		Key: KeyBeaconSites, Scope: ScopeGlobal, Kind: KindStringList,
		Category:  CatToplama,
		Default:   []string{},
		Label:     "Kabul edilen siteler",
		Help:      "Beacon'ın olay kabul ettiği site kimlikleri. Boş bırakılırsa yapılandırma dosyasındaki liste geçerli olur.",
		Developer: true,
		Live:      true,
	},
	KeyBlockedCountries: {
		Key: KeyBlockedCountries, Scope: ScopeGlobal, Kind: KindStringList,
		Category: CatBot,
		Default:  []string{},
		Label:    "Engellenen ülkeler",
		Help: "Buradaki ülkelerden gelen bağlantılar reddedilir. İki harfli ülke kodu, " +
			"satır başına bir tane (TR, DE, CN). Değişiklik anında geçerli olur — " +
			"yeniden başlatma gerekmez. Ülkesi belirlenemeyen bir adres bu listeyle " +
			"asla engellenmez.",
		Developer: true,
		Live:      true,
		Check:     checkCountryCodes,
	},
	KeyBlockedASNs: {
		Key: KeyBlockedASNs, Scope: ScopeGlobal, Kind: KindStringList,
		Category: CatBot,
		Default:  []string{},
		Label:    "Engellenen ağlar (ASN)",
		Help: "Buradaki otonom sistem numaralarından gelen bağlantılar reddedilir. " +
			"Yalnız rakam, satır başına bir tane. Değişiklik anında geçerli olur.",
		Developer: true,
		Live:      true,
		Check:     checkASNList,
	},
	KeyKnownBotASNs: {
		Key: KeyKnownBotASNs, Scope: ScopeGlobal, Kind: KindStringList,
		Category: CatBot,
		Default:  []string{},
		Label:    "Bot olarak işaretlenen ağlar (ASN)",
		Help: "Bu ağlardan gelen trafiğin bot puanına ekleme yapılır — engellenmez, " +
			"işaretlenir. Engellenen ağlar listesinden ayrıdır: engellenen bir ağ " +
			"zaten puanlamaya hiç ulaşmaz. Yalnız \"ASN puanlamaya uygulansın\" " +
			"açıkken kullanılır.",
		Developer: true,
		Live:      true,
		Check:     checkASNList,
	},
	KeyApplyASNToScoring: {
		Key: KeyApplyASNToScoring, Scope: ScopeGlobal, Kind: KindBool,
		Category: CatBot,
		Default:  false,
		Label:    "ASN puanlamaya uygulansın",
		Help: "Kapalıyken yukarıdaki bot ağı listesi hiç kullanılmaz. Varsayılan kapalı: " +
			"kimsenin seçmediği bir eklenti, tablodaki her puanı kimsenin " +
			"gösteremeyeceği bir sebeple değiştirirdi.",
		Developer: true,
		Live:      true,
	},
	KeyTrustedProxies: {
		Key: KeyTrustedProxies, Scope: ScopeGlobal, Kind: KindStringList,
		Category: CatToplama,
		Default:  []string{},
		Label:    "Güvenilen vekil ağları",
		Help: "Cloudflare gibi bir vekilin arkasındaysanız onun ağlarını buraya yazın; " +
			"yalnız bu ağlardan gelen ilettiği adrese inanılır. Liste boşken vekil " +
			"arkasındaki her ziyaretçi aynı adres görünür ve panelinizdeki neredeyse " +
			"her sayı yanlış olur. CIDR biçiminde, satır başına bir tane " +
			"(örnek: 173.245.48.0/20). Boş bırakılırsa yapılandırma dosyasındaki liste geçerli.",
		Developer: true,
		Live:      true,
		Check:     checkPrefixes,
	},
	KeyCampaignDropParams: {
		Key: KeyCampaignDropParams, Scope: ScopeGlobal, Kind: KindStringList,
		Category:  CatGizlilik,
		Default:   []string{},
		Label:     "Saklanmayacak kampanya parametreleri",
		Help:      "Örneğin utm_term. Hukuki bir karar gerektirdiği için sürüm değil, ayar.",
		Developer: true,
		Live:      true,

		RequiresDeveloperPassword: true,
		GateReason: "Bu liste, hangi kampanya parametresinin diske hiç yazılmayacağını " +
			"belirler. utm_term bazı reklam kurulumlarında ziyaretçinin gerçek arama " +
			"metnini taşır; listeden çıkarmak o metni saklamaya başlamak demektir.",
	},
	KeyCampaignExtraParams: {
		Key: KeyCampaignExtraParams, Scope: ScopeGlobal, Kind: KindStringList,
		Category:  CatGizlilik,
		Default:   []string{},
		Label:     "Ek olarak saklanacak parametreler",
		Help:      "Sitenin kendi parametreleri. Büyük/küçük harfe duyarlı eşleşir.",
		Developer: true,
		Live:      true,

		RequiresDeveloperPassword: true,
		GateReason: "Buraya eklenen her ad, içeriğini bizim denetlemediğimiz bir alanı " +
			"saklamaya karar vermektir. Sitenin o parametreye ne koyduğu " +
			"bilinmeden eklenmemeli.",
	},
	KeyCampaignStoreClickID: {
		Key: KeyCampaignStoreClickID, Scope: ScopeGlobal, Kind: KindBool,
		Category:  CatGizlilik,
		Default:   false,
		Label:     "Ham reklam tıklama kimliğini sakla",
		Help:      "Kapalıyken yalnızca hangi reklam ağı olduğu saklanır. Her tıklamada benzersiz olduğu için varsayılan kapalı.",
		Developer: true,
		Live:      true,

		RequiresDeveloperPassword: true,
		GateReason: "Ham tıklama kimliği her tıklamada benzersizdir; saklandığında " +
			"reklam ağının kayıtlarıyla eşleştirilebilen kalıcı bir tanımlayıcıya dönüşür.",
	},
	KeyLogRetentionDays: {
		Key: KeyLogRetentionDays, Scope: ScopeGlobal, Kind: KindInt,
		Category: CatGizlilik,
		Default:  14, Min: 1, Max: 3650,
		Label:     "Günlük kaydı saklama süresi (gün)",
		Help:      "Sıradan kayıtlar (erişim, alım, uygulama) bu süre sonunda silinir.",
		Developer: true,

		RequiresDeveloperPassword: true,
		GateReason: "Erişim kayıtları IP adresi içerir. Süreyi uzatmak kişisel veriyi " +
			"daha uzun tutmak, kısaltmak ise saklama yükümlülüğü varsa onu ihlal " +
			"etmek olabilir. İki yön de hukuki karardır.",
	},
	KeyLogImportantRetentionDays: {
		Key: KeyLogImportantRetentionDays, Scope: ScopeGlobal, Kind: KindInt,
		Category: CatGizlilik,
		Default:  365, Min: 1, Max: 3650,
		Label:     "Önemli kayıtları saklama süresi (gün)",
		Help:      "Güvenlik, kimlik doğrulama ve denetim kayıtları. Bunlar bir yıl sonra sorulanlardır.",
		Developer: true,

		RequiresDeveloperPassword: true,
		GateReason: "\"Kim girdi, ne zaman\" kaydı budur. Kısaltmak, bir olay " +
			"soruşturulurken cevabın artık var olmaması anlamına gelir.",
	},
	KeyUpgradeLocked: {
		Key: KeyUpgradeLocked, Scope: ScopeGlobal, Kind: KindBool,
		Category: CatBakim,
		Default:  false,
		Label:    "Yükseltmeyi geliştirici parolasına kilitle",
		Help: "Kapalıyken müşteri şema yükseltmesini tek tıkla başlatabilir. " +
			"Açıkken yalnız geliştirici parolası başlatabilir; yetki verilmesi yetmez.",
		Developer: true,

		// The lock needs the password to move, in both directions. A
		// customer who could unlock it would be a customer for whom the
		// lock does not exist, and one who could lock it could shut the
		// developer's own button - so the two directions are the same
		// decision and get the same guard.
		RequiresDeveloperPassword: true,
		GateReason: "Bu kilit, yükseltmeyi kimin başlatabileceğini belirler. " +
			"Yetkiyle korunamaz: müşteri kendine yetki verebilir, parolayı veremez.",
	},
	KeyReleaseUpdateLocked: {
		Key: KeyReleaseUpdateLocked, Scope: ScopeGlobal, Kind: KindBool,
		Category: CatBakim,
		Default:  true,
		Label:    "Sürüm güncellemesini geliştirici parolasına kilitle",
		Help: "Açıkken yalnız geliştirici parolası yeni sürüm kurabilir; yetki " +
			"verilmesi yetmez. Kapalıyken müşteri de kurabilir. Şema " +
			"yükseltmesinin aksine burada varsayılan açıktır: bu işlem " +
			"servislerin kendisini değiştirir.",
		Developer: true,

		RequiresDeveloperPassword: true,
		GateReason: "Bu kilit, sunucudaki programları kimin değiştirebileceğini " +
			"belirler. Yetkiyle korunamaz: müşteri kendine yetki verebilir, " +
			"parolayı veremez.",
	},
	KeyLogArchiveAfterDays: {
		Key: KeyLogArchiveAfterDays, Scope: ScopeGlobal, Kind: KindInt,
		Category: CatGizlilik,
		Default:  7, Min: 1, Max: 3650,
		Label:     "Kaç gün sonra arşivlensin",
		Help:      "Bu süreden eski günler sıkıştırılır. Okunabilir kalır, yaklaşık onda bir yer kaplar.",
		Developer: true,
	},
	KeyLogLevel: {
		Key: KeyLogLevel, Scope: ScopeGlobal, Kind: KindEnum,
		Category: CatTanilama,
		Default:  "info", Enum: []string{"debug", "info", "warn", "error"},
		Label: "Kayıt ayrıntı düzeyi",
		Help: "debug, en sık karşılaşılan yanlış yapılandırmayı görünür kılar; çok ayrıntılıdır. " +
			"Değişiklik bir sonraki kayıt satırında geçerli olur.",
		Developer: true,
		// Live, and it was not marked so until A5.2. The services apply
		// it through a slog.LevelVar, which is read on every record, so a
		// change takes effect on the next line - and the panel was
		// telling the customer to restart for it.
		Live: true,
	},
	KeyLogVerboseUntil: {
		Key: KeyLogVerboseUntil, Scope: ScopeGlobal, Kind: KindString,
		Category: CatTanilama,
		Default:  "",
		Label:    "Ayrıntılı kayıt bitiş zamanı",
		Help: "Geçici olarak debug'a çıkarır ve kendiliğinden söner. " +
			"Değişiklik bir sonraki kayıt satırında geçerli olur.",
		Developer: true,
		// Same as above, and it was in a worse state: read live and
		// absent from the test that binds the two lists together, so
		// nothing looked at it at all.
		Live: true,
	},
	KeyDevAccessPolicy: {
		Key: KeyDevAccessPolicy, Scope: ScopeGlobal, Kind: KindEnum,
		Category: CatErisim,
		Default:  DevAccessAsk,
		Enum:     []string{DevAccessAsk, DevAccessDeny, DevAccessOpen},
		Label:    "Geliştirici erişimi",
		Help: "Geliştirici panele girmek istediğinde ne olacağı. " +
			"ask (varsayılan): istek sizin onayınızı bekler. " +
			"deny: istek geldiği anda reddedilir ve sebebi geliştiricinin " +
			"ekranında yazar. open: onay sorulmadan kabul edilir, ama yalnız " +
			"aşağıdaki bitiş zamanına kadar — zaman geçince kendiliğinden " +
			"ask'e döner. " +
			"Bunun gerçek bir kilit olmadığını bilerek söylüyoruz: sunucuya " +
			"kabuk erişimi olan biri zaten içeride, burada kapanan yalnız " +
			"panelin kapısı.",
		// Not RequiresDeveloperPassword, and that is the phase's whole
		// point rather than an omission. The password rule protects the
		// developer from work the customer gives themselves authority to
		// create; this setting runs the other way, and a protection its
		// owner cannot reach is not a protection.
		//
		// Not Live either, which reads wrong at first and is not: the
		// change does take effect on the very next request. Live means
		// something narrower - that a *service* re-reads the value
		// through internal/settings without a restart - and this is read
		// by the panel, on the request that needs it, which is neither.
		// Marking it Live was the first version and the mirror test in
		// settings_integration_test.go refused it, correctly: the flag
		// promises the customer something about collector and beacon
		// that has nothing to do with this setting.
	},
	KeyDevAccessOpenUntil: {
		Key: KeyDevAccessOpenUntil, Scope: ScopeGlobal, Kind: KindString,
		Category: CatErisim,
		Default:  "",
		Label:    "Geliştirici erişimi açık kalma bitişi",
		Help: "Yalnız yukarısı open iken anlamlı. RFC3339 zaman damgası; boş " +
			"bırakılırsa ya da zamanı geçmişse politika ask gibi davranır. " +
			"Kalıcı olarak açık bırakılamamasının sebebi bu: destek çağrısı " +
			"bitince kapatmayı unutmak, bu ayarın var olma sebebinin kendisi.",
		Check: checkOpenUntil,
		// Not Live, for the reason written out above.
	},
	KeyPrivacyIPStorage: {
		Key: KeyPrivacyIPStorage, Scope: ScopeGlobal, Kind: KindEnum,
		Category: CatGizlilik,
		Default:  IPStorageMasked, Enum: []string{IPStorageFull, IPStorageMasked},
		Label: "IP adresi saklama biçimi",
		Help: "Ham IP adresi hiçbir modda saklanmaz. masked (varsayılan): yalnız ağ " +
			"saklanır (IPv4 /24, IPv6 /64), anahtar gerekmez. full: aynı maskeli ağ " +
			"artı tam adresten türetilen anahtarlı bir jeton saklanır — aynı /24 " +
			"içindeki iki ziyaretçi ayrılabilir, ama adresin kendisi yine diske " +
			"yazılmaz. full'e geçmek için anahtarın önceden yapılandırma dosyasında " +
			"bulunması şarttır. Değişiklik geçmişe dönük değildir.",
		Developer: true,
		Live:      true,

		RequiresDeveloperPassword: true,
		GateReason: "IP adresi KVKK/GDPR anlamında kişisel veridir ve bu ayar onun " +
			"tam mı yoksa maskeli mi saklanacağına karar verir. Hukukçu görüşü " +
			"maskeli yönünde; varsayılan da odur.",
	},
}

// limitDefinitions builds one service's admission-limit family.
//
// Generated rather than written out twice. Eight near-identical literals
// is eight chances for the collector's ceiling and the beacon's to drift
// apart in their bounds or their wording, and the difference between the
// two families is meant to be *which process reads them*, nothing else.
//
// service is the Turkish label the panel puts in front of each row, so a
// customer looking at eight limit settings can tell at a glance which
// four are which.
func limitDefinitions(service string, concurrent, perSecond, policy, queue Key) []Definition {
	return []Definition{
		{
			Key: concurrent, Scope: ScopeGlobal, Kind: KindInt,
			Category: CatSinirlar,
			Default:  0, Min: 0, Max: 100000,
			Label: service + " — eşzamanlı istek sınırı",
			Help: "Aynı anda işlenen bağlantı/istek sayısının üst sınırı. 0 sınırsız " +
				"demektir ve yapılandırma dosyasındaki değere düşer.",
			Developer: true, Live: true,
		},
		{
			Key: perSecond, Scope: ScopeGlobal, Kind: KindInt,
			Category: CatSinirlar,
			Default:  0, Min: 0, Max: 1000000,
			Label: service + " — saniyedeki istek sınırı",
			Help: "Saniyede işlenen istek sayısının üst sınırı. 0 sınırsız demektir ve " +
				"yapılandırma dosyasındaki değere düşer.",
			Developer: true, Live: true,
		},
		{
			Key: policy, Scope: ScopeGlobal, Kind: KindEnum,
			Category: CatSinirlar,
			Default:  "",
			Enum:     []string{"", OverloadFailOpen, OverloadFailClosed, OverloadThrottle},
			Label:    service + " — sınır aşıldığında",
			Help: "fail_open: trafiği geçirmeye devam eder, yalnız kaydı atlar — " +
				"sitenizi asla durdurmaz, varsayılan budur. fail_closed: fazlasını " +
				"reddeder, yani ziyaretçi siteye ulaşamaz. throttle: kuyruğa alır. " +
				"Boş bırakılırsa yapılandırma dosyasındaki değer geçerli olur.",
			Developer: true, Live: true,
		},
		{
			Key: queue, Scope: ScopeGlobal, Kind: KindInt,
			Category: CatSinirlar,
			Default:  0, Min: 0, Max: 10000,
			Label: service + " — kuyruk boyutu",
			Help: "Yalnız throttle politikasında kullanılır. Kuyruk dolduğunda fazlası " +
				"beklemeden reddedilir.",
			Developer: true, Live: true,
		},
	}
}

func init() {
	families := [][]Definition{
		limitDefinitions("Collector",
			KeyCollectorMaxConcurrent, KeyCollectorMaxPerSecond,
			KeyCollectorOverloadPolicy, KeyCollectorThrottleQueue),
		limitDefinitions("Beacon",
			KeyBeaconMaxConcurrent, KeyBeaconMaxPerSecond,
			KeyBeaconOverloadPolicy, KeyBeaconThrottleQueue),
	}
	for _, family := range families {
		for _, def := range family {
			if _, clash := registry[def.Key]; clash {
				// A generated family colliding with a hand-written entry
				// would silently replace it, and the replacement would be
				// the one with the generic wording. Panicking at init is
				// the right severity: it happens on every run, including
				// the test run, and never in front of a customer.
				panic("panel: duplicate setting key " + string(def.Key))
			}
			registry[def.Key] = def
		}
	}
}
func Lookup(key Key) (Definition, bool) {
	def, ok := registry[key]
	return def, ok
}

// DefinitionFor looks up one setting.
//
// Exported so the panel's settings page can answer "is this a real key,
// and what shape is it" without the web package holding a second copy of
// the registry. The second return is false for a key this build does not
// know - which is not a validation failure to explain to somebody, but a
// request that did not come from a page this build rendered.
func DefinitionFor(key Key) (Definition, bool) {
	def, ok := registry[key]
	return def, ok
}

// AllDefinitions returns every setting, sorted by key, for the panel's
// settings pages.
func AllDefinitions() []Definition {
	out := make([]Definition, 0, len(registry))
	for _, def := range registry {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ErrUnknownSetting is returned for a key nobody defined.
var ErrUnknownSetting = fmt.Errorf("panel: unknown setting")

// ErrInvalidSetting marks an error whose text is written for the person
// who typed the value, and is therefore safe to show them.
//
// The distinction exists because the panel used to render err.Error()
// from a settings write straight into the page. Most of the time that is
// exactly right - "analytics.retention_days must be between 1 and 3650"
// is the whole point of validating - but the same call also returns
// wrapped database errors, and a pgx error carries constraint names, SQL
// state and sometimes the query text. That is CWE-209: the customer's
// browser being shown the schema.
//
// So a validation failure says so, and everything else is logged and
// summarised. A caller deciding what to print asks this rather than
// guessing from the wording.
var ErrInvalidSetting = errors.New("panel: invalid setting value")

// invalidf builds an ErrInvalidSetting with a message meant for a
// reader. The sentinel is wrapped rather than formatted in, so the text
// stays the sentence somebody sees.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSetting, fmt.Sprintf(format, args...))
}

// Validate checks a value against its definition, returning the
// canonical form to store.
//
// Called before writing, never after reading. A value that no reader
// will accept must not be storable in the first place.
func Validate(key Key, value any) (any, error) {
	def, ok := registry[key]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownSetting, key)
	}

	canonical, err := canonicalise(def, key, value)
	if err != nil {
		return nil, err
	}

	// Check runs here, once, for every Kind - not inside one branch of
	// the switch.
	//
	// It used to live in the KindString case alone, which made
	// Definition.Check's own documentation false for four kinds out of
	// five: a validator attached to a list or an int was simply never
	// called, and nothing said so. That is the worst shape a bug can
	// take in a validation path, because the symptom is a value being
	// accepted rather than an error, and it was found by a test that
	// expected a bad network to be refused and watched it be stored.
	if def.Check != nil {
		if err := def.Check(canonical); err != nil {
			return nil, invalidf("%s: %s", key, err)
		}
	}
	return canonical, nil
}

// canonicalise applies the Kind's own rules and returns the stored form.
func canonicalise(def Definition, key Key, value any) (any, error) {
	switch def.Kind {
	case KindInt:
		n, err := toInt(value)
		if err != nil {
			return nil, invalidf("%s: %s", key, err)
		}
		if n < def.Min || n > def.Max {
			return nil, invalidf("%s must be between %d and %d, got %d", key, def.Min, def.Max, n)
		}
		return n, nil

	case KindBool:
		b, ok := value.(bool)
		if !ok {
			return nil, invalidf("%s must be true or false", key)
		}
		return b, nil

	case KindEnum:
		s, ok := value.(string)
		if !ok {
			return nil, invalidf("%s must be a string", key)
		}
		for _, admissible := range def.Enum {
			if s == admissible {
				return s, nil
			}
		}
		return nil, invalidf("%s must be one of %s, got %q", key, strings.Join(def.Enum, ", "), s)

	case KindString:
		s, ok := value.(string)
		if !ok {
			return nil, invalidf("%s must be a string", key)
		}
		if len(s) > 1024 {
			return nil, invalidf("%s is too long (max 1024 characters)", key)
		}
		return s, nil

	case KindStringList:
		list, err := toStringList(value)
		if err != nil {
			return nil, invalidf("%s: %s", key, err)
		}
		if len(list) > 1000 {
			return nil, invalidf("%s has too many entries (max 1000)", key)
		}
		return list, nil
	}
	return nil, fmt.Errorf("panel: %s has an unhandled kind %q", key, def.Kind)
}

// toInt accepts the numeric shapes JSON round-tripping produces, so a
// value read back from JSONB validates the same as one just typed.
func toInt(value any) (int, error) {
	switch n := value.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		// JSON has one number type, so an integer setting arrives as a
		// float. Refuse a fractional one rather than truncating it
		// silently.
		if n != float64(int(n)) {
			return 0, fmt.Errorf("must be a whole number, got %v", n)
		}
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("must be a whole number, got %v", n)
		}
		return int(i), nil
	default:
		return 0, fmt.Errorf("must be a number, got %T", value)
	}
}

func toStringList(value any) ([]string, error) {
	switch list := value.(type) {
	case []string:
		return list, nil
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("every entry must be a string, got %T", item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("must be a list of strings, got %T", value)
	}
}

// Setting is one stored value with its provenance, so the panel can say
// who changed it and when.
type Setting struct {
	Key       Key
	Scope     Scope
	SiteID    string
	Value     any
	UpdatedAt time.Time
	UpdatedBy *int64
}

// GateAction is the action name an authorization must carry to change a
// guarded setting.
//
// A function rather than the bare key, so the two sides cannot drift
// apart: the handler that asks for authorization and the store that
// checks it both call this, and a change to the naming is one edit. The
// prefix leaves room for guarded operations that are not settings.
func GateAction(key Key) string { return "setting:" + string(key) }

// ErrDeveloperPasswordRequired is returned when a guarded setting is
// written without a valid, current authorization.
var ErrDeveloperPasswordRequired = fmt.Errorf("panel: this setting requires the developer password")

// GuardedKeys lists every setting the developer password protects,
// sorted. For the panel, and for the test that checks this list against
// what the handlers actually ask authorization for.
func GuardedKeys() []Key {
	out := []Key{}
	for key, def := range registry {
		if def.RequiresDeveloperPassword {
			out = append(out, key)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SetSetting validates and stores a value.
//
// site must be empty for a global setting and non-empty for a site one;
// the mismatch is refused rather than quietly stored under the wrong
// scope, where it would read back as "unset" forever.
//
// It refuses guarded settings outright - those go through
// SetGuardedSetting. The refusal is here, on the one write path, rather
// than in the handler that happens to exist today: a call site added
// next year cannot forget a check it is unable to compile without.
func (s *Store) SetSetting(ctx context.Context, key Key, site string, value any, actorID *int64) error {
	if def, ok := registry[key]; ok && def.RequiresDeveloperPassword {
		return fmt.Errorf("%w (%s)", ErrDeveloperPasswordRequired, key)
	}
	return s.setSetting(ctx, key, site, value, actorID)
}

// SetGuardedSetting stores a value for a setting that carries legal
// weight.
//
// The authorization can only have come from a successful
// devgate.Gate.Verify, cannot be constructed by any other package, names
// exactly this setting, and expires seconds after it was granted. Those
// four properties together are what "the password is asked every single
// time" means in code rather than in a comment.
//
// It accepts unguarded settings too, so a form that saves a mixed set
// does not need two code paths and cannot pick the wrong one.
func (s *Store) SetGuardedSetting(ctx context.Context, key Key, site string, value any, actorID *int64, auth devgate.Authorization) error {
	def, ok := registry[key]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownSetting, key)
	}
	if def.RequiresDeveloperPassword && !auth.Authorizes(GateAction(key)) {
		// One error for "no authorization", "an authorization for
		// something else" and "an authorization that has expired".
		// Nothing useful is done differently between them, and the
		// caller that could tell them apart is the caller that would
		// start retrying with the wrong one.
		return fmt.Errorf("%w (%s)", ErrDeveloperPasswordRequired, key)
	}
	return s.setSetting(ctx, key, site, value, actorID)
}

func (s *Store) setSetting(ctx context.Context, key Key, site string, value any, actorID *int64) error {
	def, ok := registry[key]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownSetting, key)
	}
	if def.Scope == ScopeGlobal && site != "" {
		return fmt.Errorf("panel: %s is a global setting and takes no site", key)
	}
	if def.Scope == ScopeSite && site == "" {
		return fmt.Errorf("panel: %s is a per-site setting and requires one", key)
	}

	canonical, err := Validate(key, value)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return fmt.Errorf("panel: encode %s: %w", key, err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO panel_settings (scope, site_id, key, value, updated_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (scope, site_id, key)
		DO UPDATE SET value = EXCLUDED.value,
		              updated_at = now(),
		              updated_by = EXCLUDED.updated_by`,
		string(def.Scope), site, string(key), encoded, actorID)
	if err != nil {
		return wrapStoreError(key, err)
	}
	return nil
}

// wrapStoreError wraps a database failure from a settings write.
//
// Deliberately not an ErrInvalidSetting: the text carries constraint
// names, SQL state and sometimes the query, and the panel decides what
// to print by asking that sentinel. Named rather than inlined so the
// distinction has somewhere to be tested from.
func wrapStoreError(key Key, err error) error {
	return fmt.Errorf("panel: store setting %s: %w", key, err)
}

// GetSetting returns a stored value, or the definition's default when
// nothing has been stored.
//
// A site setting falls back to the global row for the same key before it
// falls back to the default, so "set it once for the deployment, override
// it for the one site that needs it" works without writing a row per
// site.
func (s *Store) GetSetting(ctx context.Context, key Key, site string) (any, error) {
	def, ok := registry[key]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownSetting, key)
	}

	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT value FROM panel_settings
		WHERE key = $1 AND (site_id = $2 OR site_id = '')
		-- The site's own row wins over the deployment-wide one.
		ORDER BY (site_id = '') ASC
		LIMIT 1`,
		string(key), site).Scan(&raw)
	if err != nil {
		if err == pgx.ErrNoRows {
			return def.Default, nil
		}
		return nil, fmt.Errorf("panel: read setting %s: %w", key, err)
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("panel: decode setting %s: %w", key, err)
	}

	// Validated on the way out as well as in. A row written by an older
	// build, or edited in the database by hand, must not be able to hand
	// a service a value outside the bounds it was written against - the
	// same reasoning behind bounding argon2 cost parameters when reading
	// a stored hash.
	canonical, err := Validate(key, value)
	if err != nil {
		return def.Default, nil
	}
	return canonical, nil
}

// GetIntSetting is the typed accessor callers actually use.
func (s *Store) GetIntSetting(ctx context.Context, key Key, site string) (int, error) {
	value, err := s.GetSetting(ctx, key, site)
	if err != nil {
		return 0, err
	}
	return toInt(value)
}

// GetBoolSetting is the typed accessor for bool settings.
//
// A wrong type is an error rather than a false: a caller reading a
// setting that is not a bool has a bug, and answering "no" would hide
// it behind a plausible-looking default - which for a lock means
// answering "unlocked" to a question it could not read.
func (s *Store) GetBoolSetting(ctx context.Context, key Key, site string) (bool, error) {
	value, err := s.GetSetting(ctx, key, site)
	if err != nil {
		return false, err
	}
	b, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("panel: %s is not a bool setting", key)
	}
	return b, nil
}

// GetStringSetting is the typed accessor for enum and string settings.
func (s *Store) GetStringSetting(ctx context.Context, key Key, site string) (string, error) {
	value, err := s.GetSetting(ctx, key, site)
	if err != nil {
		return "", err
	}
	s2, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("panel: %s is not a string setting", key)
	}
	return s2, nil
}

// ListSettings returns every stored row for a scope, for the panel's
// settings page and for the diagnostic bundle.
func (s *Store) ListSettings(ctx context.Context, site string) ([]Setting, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT scope, site_id, key, value, updated_at, updated_by
		FROM panel_settings
		WHERE site_id = $1 OR site_id = ''
		ORDER BY key, site_id`, site)
	if err != nil {
		return nil, fmt.Errorf("panel: list settings: %w", err)
	}
	defer rows.Close()

	out := []Setting{}
	for rows.Next() {
		var set Setting
		var raw []byte
		var scope string
		if err := rows.Scan(&scope, &set.SiteID, &set.Key, &raw, &set.UpdatedAt, &set.UpdatedBy); err != nil {
			return nil, fmt.Errorf("panel: scan setting: %w", err)
		}
		set.Scope = Scope(scope)
		if err := json.Unmarshal(raw, &set.Value); err != nil {
			return nil, fmt.Errorf("panel: decode setting %s: %w", set.Key, err)
		}
		out = append(out, set)
	}
	return out, rows.Err()
}

// ResetSetting removes a stored value so the default applies again.
//
// Guarded settings are refused here too, and that is not a formality.
// Resetting is a change of value like any other, and for at least one
// guarded setting it moves in the dangerous direction: the default for
// campaign.drop_params is the empty list, so "reset" means "start
// storing utm_term again". A gate that covered writes but not resets
// would have a way around it that reads as tidying up.
func (s *Store) ResetSetting(ctx context.Context, key Key, site string) error {
	if def, ok := registry[key]; ok && def.RequiresDeveloperPassword {
		return fmt.Errorf("%w (%s)", ErrDeveloperPasswordRequired, key)
	}
	return s.resetSetting(ctx, key, site)
}

// ResetGuardedSetting removes a stored value for a guarded setting,
// against a fresh authorization.
func (s *Store) ResetGuardedSetting(ctx context.Context, key Key, site string, auth devgate.Authorization) error {
	def, ok := registry[key]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownSetting, key)
	}
	if def.RequiresDeveloperPassword && !auth.Authorizes(GateAction(key)) {
		return fmt.Errorf("%w (%s)", ErrDeveloperPasswordRequired, key)
	}
	return s.resetSetting(ctx, key, site)
}

func (s *Store) resetSetting(ctx context.Context, key Key, site string) error {
	if _, ok := registry[key]; !ok {
		return fmt.Errorf("%w %q", ErrUnknownSetting, key)
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM panel_settings WHERE key = $1 AND site_id = $2`, string(key), site)
	if err != nil {
		return fmt.Errorf("panel: reset setting %s: %w", key, err)
	}
	return nil
}

// LogLifecycle reads the log settings and returns the policy a service
// should maintain its tree against.
//
// This is the bridge between the panel and internal/logging, and it is
// deliberately a plain read rather than an import of the logging package
// here: internal/panel writes only to panel_* tables and knows nothing
// about the filesystem, so it returns numbers and lets the caller build
// the policy. Keeping the dependency pointing one way is what allows the
// panel's database role to stay as narrow as it is.
func (s *Store) LogLifecycle(ctx context.Context) (archiveAfter, retention, importantRetention int, err error) {
	if archiveAfter, err = s.GetIntSetting(ctx, KeyLogArchiveAfterDays, ""); err != nil {
		return 0, 0, 0, err
	}
	if retention, err = s.GetIntSetting(ctx, KeyLogRetentionDays, ""); err != nil {
		return 0, 0, 0, err
	}
	if importantRetention, err = s.GetIntSetting(ctx, KeyLogImportantRetentionDays, ""); err != nil {
		return 0, 0, 0, err
	}
	// A configuration that deletes what it has not yet archived is
	// almost certainly a mistake, and the consequence - losing a day's
	// logs before they were compressed - is not recoverable. Clamping is
	// better than refusing: refusing would leave the service with no
	// policy at all.
	if archiveAfter > retention {
		archiveAfter = retention
	}
	return archiveAfter, retention, importantRetention, nil
}

// checkTimezone refuses a zone name this machine cannot load.
//
// The same rule the config file already applies, for the same reason: a
// panel that accepts "Avrupa/İstanbul" and then quietly renders in UTC
// tells a shop in Istanbul that its evening peak happened in the
// afternoon, and nothing anywhere says why. Refusing at the point of
// writing is the only place the person typing it is still around to
// hear about it.
//
// Empty is allowed and means "use the config file's value", which is the
// state every deployment starts in.
// checkPrefixes refuses a trusted-proxy list that is not a list of
// networks.
//
// The repair catalogue says this value is "netip.Prefix, never as text",
// and a JSON column makes that impossible to honour literally - what it
// can mean is that no value reaches the column without having been a
// netip.Prefix first. This is where that happens.
//
// It matters more here than for most settings. Everything downstream -
// the visitor count, the country, the crossover join - is derived from
// whichever address this decides to believe, so a typo does not produce
// an error, it produces a panel full of confident wrong numbers.
//
// A bare address is accepted and read as a single host, because that is
// what somebody with one proxy will type and refusing it would be
// pedantry.
// checkCountryCodes refuses anything that is not a two-letter code.
//
// Validated on the way in rather than shrugged off at read time: a
// stored "Turkiye" would never match asnlookup's "TR", so the setting
// would appear to save and block nothing - the worst outcome for a
// control somebody reached for during an attack.
func checkCountryCodes(value any) error {
	entries, ok := value.([]string)
	if !ok {
		return errors.New("expected a list of country codes")
	}
	for _, entry := range entries {
		code := strings.ToUpper(strings.TrimSpace(entry))
		if code == "" {
			return errors.New("the list has an empty line in it")
		}
		if len(code) != 2 {
			return fmt.Errorf("%q is not a two-letter country code (TR, DE, CN)", entry)
		}
		for _, r := range code {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("%q is not a two-letter country code (TR, DE, CN)", entry)
			}
		}
	}
	return nil
}

// checkASNList refuses anything that is not a positive number.
//
// Stored as text because the column is text; never treated as text. Zero
// is refused explicitly: it is asnlookup's "not resolved", so a rule
// built from it would match every address the lookup could not place.
func checkASNList(value any) error {
	entries, ok := value.([]string)
	if !ok {
		return errors.New("expected a list of ASN numbers")
	}
	for _, entry := range entries {
		raw := strings.TrimSpace(entry)
		if raw == "" {
			return errors.New("the list has an empty line in it")
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%q is not a number (an ASN is written as digits only, e.g. 64512)", entry)
		}
		if n <= 0 {
			return fmt.Errorf("%d is not a usable ASN; 0 is what the lookup reports for an "+
				"address it could not place, so a rule made from it would match those", n)
		}
	}
	return nil
}

func checkPrefixes(value any) error {
	entries, ok := value.([]string)
	if !ok {
		return errors.New("expected a list of networks")
	}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return errors.New("the list has an empty line in it")
		}
		if strings.Contains(entry, "/") {
			if _, err := netip.ParsePrefix(entry); err != nil {
				return fmt.Errorf("%q is not a network (try 173.245.48.0/20)", entry)
			}
			continue
		}
		if _, err := netip.ParseAddr(entry); err != nil {
			return fmt.Errorf("%q is neither an address nor a network "+
				"(try 173.245.48.0/20, or a single address)", entry)
		}
	}
	return nil
}

func checkTimezone(value any) error {
	name, _ := value.(string)
	if name == "" {
		return nil
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Errorf("%q is not a timezone this machine knows (try Europe/Istanbul)", name)
	}
	return nil
}

// checkOpenUntil accepts an empty string or an RFC3339 timestamp.
//
// Validated on the way in rather than shrugged at on the way out, which
// is this file's rule and matters more here than usual: an unparseable
// timestamp has to mean *something* at read time, and both answers are
// bad. Treat it as "open" and a typo props the door open; treat it as
// expired and the customer who typed it believes a door is open that is
// not. Refusing it at the moment it is typed is the only reading that
// leaves nobody misinformed.
//
// KeyLogVerboseUntil has no such check and should - it is the same
// shape with the same failure. Left alone deliberately: this phase does
// not touch logging, and a fix smuggled in beside an unrelated one is
// how a change nobody reviewed reaches a release.
func checkOpenUntil(value any) error {
	text, _ := value.(string)
	if text == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, text); err != nil {
		return fmt.Errorf("%q bir RFC3339 zaman damgası değil "+
			"(örnek: 2026-09-01T18:30:00+03:00)", text)
	}
	return nil
}

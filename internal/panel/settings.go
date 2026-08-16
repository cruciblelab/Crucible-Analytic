package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	// KeyLogVerboseUntil is when a temporary raise to debug expires.
	// Stored as an RFC3339 timestamp, empty when not raised.
	//
	// Self-expiring rather than a plain toggle: verbose logging left on
	// because somebody forgot is the way the disk fills.
	KeyLogVerboseUntil Key = "logs.verbose_until"
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

// The analytics lifecycle settings.
const (
	// KeyAnalyticsRetentionDays is how long traffic_snapshots and
	// beacon_events are kept.
	KeyAnalyticsRetentionDays Key = "analytics.retention_days"
	// KeyAnalyticsCompressAfterDays is when TimescaleDB compresses a
	// chunk. Usually the better answer than deleting: the customer keeps
	// their history and gets most of the disk back.
	KeyAnalyticsCompressAfterDays Key = "analytics.compress_after_days"
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
	// IPStorageFull keeps the whole address.
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
	// Label and Help are Turkish, because they are read in the panel.
	Label string
	Help  string
	// Developer marks a setting the operator owns rather than the
	// customer.
	//
	// It controls who may *change* it, not who may see it. A customer
	// with full rights on their own panel sees every one of these, with
	// its value and its explanation, and cannot edit it - the servers
	// belong to the operator, so these are the operator's to answer for.
	//
	// Hiding them would be the worse design. A setting nobody can see is
	// a setting nobody can ask about, and the customer's own deployment
	// would have behaviour they cannot account for. Showing the value
	// and withholding the control says the true thing: this is decided,
	// it is decided by us, and here is what it is.
	Developer bool
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
	// GateReason is why this particular setting is guarded, in Turkish.
	//
	// Per setting rather than one blanket sentence, because "this needs
	// a password" invites the person to look for the password, while
	// "this decides whether whole IP addresses are stored" tells them
	// what they are about to do. The second is what makes the prompt
	// worth interrupting somebody with.
	GateReason string
}

// registry is every setting this system has. Adding one is a code change
// that goes through review, which is the same rule the panel's repair
// operations follow and for the same reason: a running system should not
// be talkable into a setting nobody designed.
var registry = map[Key]Definition{
	KeyBeaconSites: {
		Key: KeyBeaconSites, Scope: ScopeGlobal, Kind: KindStringList,
		Default:   []string{},
		Label:     "Kabul edilen siteler",
		Help:      "Beacon'ın olay kabul ettiği site kimlikleri. Boş bırakılırsa yapılandırma dosyasındaki liste geçerli olur.",
		Developer: true,
		Live:      true,
	},
	KeyCampaignDropParams: {
		Key: KeyCampaignDropParams, Scope: ScopeGlobal, Kind: KindStringList,
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
		Default: 14, Min: 1, Max: 3650,
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
		Default: 365, Min: 1, Max: 3650,
		Label:     "Önemli kayıtları saklama süresi (gün)",
		Help:      "Güvenlik, kimlik doğrulama ve denetim kayıtları. Bunlar bir yıl sonra sorulanlardır.",
		Developer: true,

		RequiresDeveloperPassword: true,
		GateReason: "\"Kim girdi, ne zaman\" kaydı budur. Kısaltmak, bir olay " +
			"soruşturulurken cevabın artık var olmaması anlamına gelir.",
	},
	KeyLogArchiveAfterDays: {
		Key: KeyLogArchiveAfterDays, Scope: ScopeGlobal, Kind: KindInt,
		Default: 7, Min: 1, Max: 3650,
		Label:     "Kaç gün sonra arşivlensin",
		Help:      "Bu süreden eski günler sıkıştırılır. Okunabilir kalır, yaklaşık onda bir yer kaplar.",
		Developer: true,
	},
	KeyLogLevel: {
		Key: KeyLogLevel, Scope: ScopeGlobal, Kind: KindEnum,
		Default: "info", Enum: []string{"debug", "info", "warn", "error"},
		Label:     "Kayıt ayrıntı düzeyi",
		Help:      "debug, en sık karşılaşılan yanlış yapılandırmayı görünür kılar; çok ayrıntılıdır.",
		Developer: true,
	},
	KeyLogVerboseUntil: {
		Key: KeyLogVerboseUntil, Scope: ScopeGlobal, Kind: KindString,
		Default:   "",
		Label:     "Ayrıntılı kayıt bitiş zamanı",
		Help:      "Geçici olarak debug'a çıkarır ve kendiliğinden söner.",
		Developer: true,
	},
	KeyAnalyticsRetentionDays: {
		Key: KeyAnalyticsRetentionDays, Scope: ScopeSite, Kind: KindInt,
		Default: 90, Min: 1, Max: 3650,
		Label: "Analitik verisi saklama süresi (gün)",
		Help:  "Bu süreden eski ziyaret kayıtları silinir.",

		RequiresDeveloperPassword: true,
		GateReason: "Ziyaret kayıtlarının ne kadar süre saklanacağı, KVKK'nın " +
			"\"gerektiğinden uzun tutma\" ilkesinin doğrudan konusudur.",
	},
	KeyAnalyticsCompressAfterDays: {
		Key: KeyAnalyticsCompressAfterDays, Scope: ScopeSite, Kind: KindInt,
		Default: 30, Min: 1, Max: 3650,
		Label:     "Kaç gün sonra sıkıştırılsın",
		Help:      "Geçmiş korunur, diskin çoğu geri alınır.",
		Developer: true,
	},
	KeyPrivacyIPStorage: {
		Key: KeyPrivacyIPStorage, Scope: ScopeGlobal, Kind: KindEnum,
		Default: IPStorageMasked, Enum: []string{IPStorageFull, IPStorageMasked},
		Label: "IP adresi saklama biçimi",
		Help: "masked: IPv4 son oktet sıfırlanır, IPv6 /64'e kırpılır. Varsayılan budur. " +
			"full: adres olduğu gibi saklanır; kesişim görünümleri keskinleşir, " +
			"saklanan kişisel veri artar. Değişiklik geçmişe dönük değildir.",
		Developer: true,
		Live:      true,

		RequiresDeveloperPassword: true,
		GateReason: "IP adresi KVKK/GDPR anlamında kişisel veridir ve bu ayar onun " +
			"tam mı yoksa maskeli mi saklanacağına karar verir. Hukukçu görüşü " +
			"maskeli yönünde; varsayılan da odur.",
	},
}

// Lookup returns a setting's definition.
func Lookup(key Key) (Definition, bool) {
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

	switch def.Kind {
	case KindInt:
		n, err := toInt(value)
		if err != nil {
			return nil, fmt.Errorf("panel: %s: %w", key, err)
		}
		if n < def.Min || n > def.Max {
			return nil, fmt.Errorf("panel: %s must be between %d and %d, got %d", key, def.Min, def.Max, n)
		}
		return n, nil

	case KindBool:
		b, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("panel: %s must be true or false", key)
		}
		return b, nil

	case KindEnum:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("panel: %s must be a string", key)
		}
		for _, admissible := range def.Enum {
			if s == admissible {
				return s, nil
			}
		}
		return nil, fmt.Errorf("panel: %s must be one of %s, got %q", key, strings.Join(def.Enum, ", "), s)

	case KindString:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("panel: %s must be a string", key)
		}
		if len(s) > 1024 {
			return nil, fmt.Errorf("panel: %s is too long (max 1024 characters)", key)
		}
		return s, nil

	case KindStringList:
		list, err := toStringList(value)
		if err != nil {
			return nil, fmt.Errorf("panel: %s: %w", key, err)
		}
		if len(list) > 1000 {
			return nil, fmt.Errorf("panel: %s has too many entries (max 1000)", key)
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
		return fmt.Errorf("panel: store setting %s: %w", key, err)
	}
	return nil
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

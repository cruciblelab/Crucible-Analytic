package panel

import "sort"

// The settings that live in a service's config file rather than in the
// settings table, listed so the panel can show them.
//
// # Why show something nobody can edit
//
// Because the customer's deployment behaves the way these say, and a
// value nobody can see is a value nobody can ask about. A customer who
// wants to know which address the beacon listens on, or whether this
// installation terminates TLS itself, should be able to look rather than
// open a ticket to find out. What the panel withholds is the control,
// which it could not honour anyway: these are read once at startup from
// a file on disk.
//
// # Why not simply move them into the settings table
//
// Each one is needed before the settings table is reachable, or cannot
// change without restarting the process that reads it. You cannot ask
// the database how to reach the database. A listening socket is bound
// once. Moving these would not make them live; it would make them look
// live, which is worse.
//
// # Secrets are named but never shown
//
// A connection string carries a password and the developer hash is a
// credential. Both appear in the list - hiding their existence would
// leave a gap in the account of how the deployment is configured - but
// neither ever carries a value, and the type makes that structural
// rather than a rule somebody has to remember.

// ConfigFileSetting is one value that lives in a config file.
type ConfigFileSetting struct {
	// Service names which process reads it.
	Service string
	// Key is the TOML path, exactly as written in the file.
	Key string
	// Label and Help are Turkish, for the panel.
	Label string
	Help  string
	// Value is what the file holds, when the panel can see it. Empty
	// means "not known here" rather than "empty in the file", and the
	// panel should say so rather than rendering a blank.
	Value string
	// Known reports whether Value was actually supplied, so the panel can
	// tell an unset value from an unseen one.
	Known bool
	// Secret marks a value that must never be displayed. Set on the
	// entry itself, so it cannot be forgotten at a call site.
	Secret bool
}

// configFileSettings is the registry. Adding a key here is how a new
// config value becomes visible in the panel; nothing is discovered
// automatically, because a value the panel could not explain is not
// worth showing.
var configFileSettings = []ConfigFileSetting{
	{
		Service: "collector", Key: "site_id",
		Label: "Site kimliği",
		Help:  "Bu collector'ın hangi siteyi ölçtüğü. Her satıra damgalanır ve okuma API'sinde yol parçası olur.",
	},
	{
		Service: "collector", Key: "mode",
		Label: "Çalışma modu",
		Help:  "passthrough: TLS'e hiç dokunulmaz, adres satırı teknik olarak görülemez. full: TLS burada sonlandırılır.",
	},
	{
		Service: "collector", Key: "network.listen_addr",
		Label: "Dinlenen adres",
		Help:  "Collector'ın bağlantı kabul ettiği adres. Soket açılışta bağlanır, çalışırken değişemez.",
	},
	{
		Service: "collector", Key: "network.backend_addr",
		Label: "Arka uç adresi",
		Help:  "Trafiğin yönlendirildiği asıl site.",
	},
	{
		Service: "collector", Key: "tls.cert_file",
		Label: "TLS sertifika yolu",
		Help:  "Yalnız full modda kullanılır. Dosya sistemindedir; yenilemesi kök yetkisi ister.",
	},
	{
		Service: "collector", Key: "storage.timescale_dsn",
		Label:  "Veritabanı bağlantısı (collector)",
		Help:   "Bağlantı dizesi parola taşır. Varlığı burada görünür, değeri hiçbir zaman gösterilmez.",
		Secret: true,
	},
	{
		Service: "beacon", Key: "listen_addr",
		Label: "Dinlenen adres (beacon)",
		Help:  "Tarayıcıdan gelen olayların ulaştığı adres. Önünde bir web sunucusu olması beklenir.",
	},
	{
		Service: "beacon", Key: "path_prefix",
		Label: "Yol öneki",
		Help:  "Snippet'in ve olay ucunun sunulduğu yol. Sitenin kendi alan adından yönlendirilir.",
	},
	{
		Service: "beacon", Key: "trusted_proxies",
		Label: "Güvenilen vekiller",
		Help: "X-Forwarded-For başlığına yalnız bu adreslerden geldiğinde inanılır. Fazla geniş " +
			"verilmesi her ziyaretçinin adresini vekilin adresi gibi gösterir.",
	},
	{
		Service: "beacon", Key: "timescale_dsn",
		Label:  "Veritabanı bağlantısı (beacon)",
		Help:   "Parola taşır; değeri gösterilmez.",
		Secret: true,
	},
	{
		Service: "api", Key: "listen_addr",
		Label: "Dinlenen adres (okuma API'si)",
		Help:  "Panelin analitiği okuduğu uç. Bu süreç hiçbir tabloya yazamaz.",
	},
	{
		Service: "panel", Key: "developer.password_hash",
		Label:  "Geliştirici şifresi (hash)",
		Help:   "Hukuki ağırlıklı ayarları koruyan şifrenin hash'i. Değeri gösterilmez; varlığı kurulum kontrolünde raporlanır.",
		Secret: true,
	},
	{
		Service: "hepsi", Key: "logging.dir",
		Label: "Günlük dizini",
		Help:  "Kayıt ağacının kökü. Dizin ve izinleri sunucuda oluşturulur; panel kendi yazacağı dizini yaratamaz.",
	},
}

// ConfigFileSettings returns the registry with values filled in where the
// caller could supply them.
//
// values is keyed by "service.key" - "beacon.listen_addr". A key not in
// the map comes back with Known false, which the panel renders as "not
// visible from here" rather than as an empty value; the difference
// matters, because one is a fact about the deployment and the other is a
// fact about the panel.
//
// A secret entry never takes a value, whatever the map holds. That check
// is here rather than at the call sites because there will be more call
// sites than there are people who remember this rule.
func ConfigFileSettings(values map[string]string) []ConfigFileSetting {
	out := make([]ConfigFileSetting, 0, len(configFileSettings))
	for _, setting := range configFileSettings {
		if !setting.Secret {
			if value, ok := values[setting.Service+"."+setting.Key]; ok {
				setting.Value, setting.Known = value, true
			}
		}
		out = append(out, setting)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ConfigFileNotice is what the panel shows above the list.
const ConfigFileNotice = "Aşağıdakiler sunucudaki yapılandırma dosyalarında durur, veritabanında " +
	"değil. Panelden değiştirilemezler — geliştirici tarafından da — çünkü servis " +
	"bunları açılışta bir kez okur. Kurulumunuzun nasıl yapılandırıldığını " +
	"görebilmeniz için burada listelenir. Parola taşıyan alanların yalnızca varlığı " +
	"yazılıdır, değeri hiçbir yerde gösterilmez."

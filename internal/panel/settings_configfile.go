package panel

import (
	"github.com/cruciblelab/crucible-analytic/internal/configreport"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

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
//
// # Where the values come from (PLAN §W4, item 5)
//
// From the services, through their heartbeat rows: each process says what
// it loaded (internal/configreport), and this file only labels it. Until
// W4d the values came from a map the panel's main filled with its own
// keys - "panel.listen_addr", "panel.timezone" - none of which this list
// had, so on the live rig the step showed 13 rows, 10 of them "panele
// bildirilmemiş", 3 secret and 0 known, on every deployment. The keys are
// now configreport.Entries; this file holds only the words, and
// TestConfigFileLabelsAreTheReportedKeys holds the two to one list.

// ConfigFileSetting is one value that lives in a config file.
type ConfigFileSetting struct {
	// Service names the process the value belongs to: the program -
	// "collector" - when nothing has reported, or the heartbeat row's
	// key - "collector@shop" - when one has, so two collectors on one
	// deployment are two sets of rows.
	Service string
	// Key is the TOML path, exactly as written in the file.
	Key string
	// Label and Help are Turkish, for the panel.
	Label string
	Help  string
	// Value is what the process loaded, when it said. Empty means "not
	// reported" rather than "empty in the file", and the panel should
	// say so rather than rendering a blank.
	Value string
	// Known reports whether Value was actually supplied, so the panel can
	// tell an unset value from an unseen one.
	Known bool
	// Secret marks a value that must never be displayed. Taken from
	// configreport's entry, so it cannot be forgotten at a call site.
	Secret bool
}

// configFileLabel is what the panel says about one reported key.
type configFileLabel struct{ Label, Help string }

// configFileLabels are the words for configreport.Entries' file keys,
// keyed "program.key". The keys themselves - and which are secret - are
// configreport's; a key there with no words here, or words here for no
// key, fails TestConfigFileLabelsAreTheReportedKeys.
var configFileLabels = map[string]configFileLabel{
	"collector.site_id": {"Site kimliği",
		"Bu collector'ın hangi siteyi ölçtüğü. Her satıra damgalanır ve okuma API'sinde yol parçası olur."},
	"collector.mode": {"Çalışma modu",
		"passthrough: TLS'e hiç dokunulmaz, adres satırı teknik olarak görülemez. full: TLS burada sonlandırılır."},
	"collector.network.listen_addr": {"Dinlenen adres",
		"Collector'ın bağlantı kabul ettiği adres. Soket açılışta bağlanır, çalışırken değişemez."},
	"collector.network.backend_addr": {"Arka uç adresi",
		"Trafiğin yönlendirildiği asıl site."},
	"collector.tls.cert_file": {"TLS sertifika yolu",
		"Yalnız full modda kullanılır. Dosya sistemindedir; yenilemesi kök yetkisi ister."},
	"collector.storage.timescale_dsn": {"Veritabanı bağlantısı",
		"Bağlantı dizesi parola taşır. Varlığı burada görünür, değeri hiçbir zaman gösterilmez."},
	"collector.logging.dir": {"Günlük dizini",
		"Bu collector'ın kayıt ağacının kökü. Dizin ve izinleri sunucuda oluşturulur."},

	"beacon.listen_addr": {"Dinlenen adres",
		"Tarayıcıdan gelen olayların ulaştığı adres. Önünde bir web sunucusu olması beklenir."},
	"beacon.path_prefix": {"Yol öneki",
		"Snippet'in ve olay ucunun sunulduğu yol. Sitenin kendi alan adından yönlendirilir."},
	"beacon.trusted_proxies": {"Güvenilen vekiller",
		"X-Forwarded-For başlığına yalnız bu adreslerden geldiğinde inanılır. Fazla geniş " +
			"verilmesi her ziyaretçinin adresini vekilin adresi gibi gösterir."},
	"beacon.sites": {"Dosyadaki siteler",
		"Panelde site listesi kaydedilmemişse beacon bunları kabul eder. Panelde kaydedilen liste bunun yerine geçer."},
	"beacon.timescale_dsn": {"Veritabanı bağlantısı",
		"Parola taşır; değeri gösterilmez."},
	"beacon.logging.dir": {"Günlük dizini",
		"Beacon'ın kayıt ağacının kökü."},

	"api.listen_addr": {"Dinlenen adres",
		"Panelin analitiği okuduğu uç. Bu süreç hiçbir tabloya yazamaz."},
	"api.timescale_dsn": {"Veritabanı bağlantısı",
		"Parola taşır; değeri gösterilmez."},
	"api.logging.dir": {"Günlük dizini",
		"Okuma API'sinin kayıt ağacının kökü."},

	"panel.listen_addr": {"Dinlenen adres",
		"Panelin bağlantı kabul ettiği adres. Önünde bir web sunucusu olması beklenir."},
	"panel.trusted_proxies": {"Güvenilen vekiller",
		"Giriş denemelerinin kime sayılacağı bu adreslerden gelen X-Forwarded-For başlığından okunur."},
	"panel.panel_dsn": {"Veritabanı bağlantısı",
		"Parola taşır; değeri gösterilmez."},
	"panel.developer_gate.password_hash": {"Geliştirici şifresi (hash)",
		"Hukuki ağırlıklı ayarları koruyan şifrenin hash'i. Değeri gösterilmez; varlığı kurulum kontrolünde raporlanır."},
	"panel.logging.dir": {"Günlük dizini",
		"Panelin kayıt ağacının kökü. Panel kendi yazacağı dizini yaratamaz."},
}

// ConfigFileSettings returns the list with the values the services
// reported filled in.
//
// One set of rows per reporting process, so two collectors are two sets;
// one set marked "not reported" for a program no process has reported
// for - it is not running, or it is a build older than the report. A
// secret entry never takes a value, whatever a report holds: the check is
// here as well as in the writer, because a row in the table is whatever
// some binary wrote, and this is the side that draws it.
func ConfigFileSettings(reports []heartbeat.ConfigReport) []ConfigFileSetting {
	var out []ConfigFileSetting
	for _, program := range configreport.Programs {
		var mine []heartbeat.ConfigReport
		for _, r := range reports {
			if r.Report.Program == program {
				mine = append(mine, r)
			}
		}
		if len(mine) == 0 {
			out = append(out, configFileRows(program, program, nil)...)
			continue
		}
		for _, r := range mine {
			out = append(out, configFileRows(program, r.Key(), r.Report.Values)...)
		}
	}
	return out
}

func configFileRows(program, service string, values map[string]string) []ConfigFileSetting {
	var out []ConfigFileSetting
	for _, entry := range configreport.For(program) {
		if entry.Live {
			continue
		}
		label := configFileLabels[program+"."+entry.Key]
		row := ConfigFileSetting{
			Service: service, Key: entry.Key,
			Label: label.Label, Help: label.Help, Secret: entry.Secret,
		}
		if !entry.Secret {
			row.Value, row.Known = values[entry.Key]
		}
		out = append(out, row)
	}
	return out
}

// ConfigFileNotice is what the panel shows above the list.
const ConfigFileNotice = "Aşağıdakiler sunucudaki yapılandırma dosyalarında durur, veritabanında " +
	"değil. Panelden değiştirilemezler — geliştirici tarafından da — çünkü servis " +
	"bunları açılışta bir kez okur. Değerleri her servis kendisi bildirir: dosyada " +
	"yazanı değil, çalışan sürecin yüklediğini. Dosya değiştirilip servis yeniden " +
	"başlatılmadıysa burada eski değer görünür, çünkü çalışan odur. Parola taşıyan " +
	"alanların yalnızca varlığı yazılıdır, değeri hiçbir yerde gösterilmez."

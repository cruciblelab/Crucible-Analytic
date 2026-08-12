# Crucible Analytic — Tam Plan

Bu belge, projenin bugünkü durumunu ve kalan işin tamamını tek yerde
toplar. `NOTES.md` *neden* öyle karar verildiğini anlatır; bu belge *ne*
yapılacağını, hangi sırayla, hangi dosyada ve neye "bitti" deneceğini
söyler.

Amaç: bu belgeyi okuyan birinin "hepsi bu kadar mı?" sorusuna, her
soruşunda aynı cevabı alması.

---

## 0. Tek paragrafta ürün

Crucible Analytic, bir web sitesinin trafiğini **iki bağımsız kaynaktan**
ölçen, kendi sunucunuzda çalışan bir analitik sistemidir. Birinci kaynak
sitenin önünde duran bir ters vekil (collector): her TCP/TLS bağlantısını
görür, JA4 parmak izi çıkarır, hız penceresi tutar, bot skoru hesaplar —
ama şifreli trafikte URL göremez. İkinci kaynak siteye gömülen küçük bir
JS (beacon): sayfa yolunu, referrer'ı, kampanyayı, olayları görür — ama
sadece JS çalıştıran istemcilerden. İkisi `beacon_events.ip` üzerinden
birleşir. Bu birleşim, hiçbir tek kaynaklı aracın veremeyeceği cevabı
verir: **JS çalıştırmayan ama siteye giren nedir.**

---

## 1. Şu an ne var (yazıldı, test edildi, gerçek veriyle doğrulandı)

~22.400 satır Go, 13 iç paket, 3 çalışan binary.

### 1.1 Toplama katmanı

| Paket | Ne yapar | Durum |
|---|---|---|
| `internal/ja4` | ClientHello ayrıştırma, JA4 parmak izi | FoxIO referans verisiyle doğrulandı |
| `internal/proxy` | TCP/TLS passthrough vekil, ClientHello koklama | Gerçek trafikle doğrulandı |
| `internal/fullproxy` | HTTP/1.1 + h2c sonlandıran tam vekil | Gerçek backend'e karşı doğrulandı |
| `internal/ratestore` | O(1) kayan pencere, IP başına hız | Yük testi yapıldı |
| `internal/scoring` | Bot olasılık skoru (JA4 + hız + ASN) | Birim testli |
| `internal/limiter` | Eşzamanlılık/RPS sınırları, 3 aşırı yük politikası, geo blok | Gerçek eşzamanlı yük testi yapıldı |
| `internal/asnlookup` | IP → ülke/ASN, IPv4+IPv6, LRU önbellek | Tam CSV ile ölçüldü: ~135 MB, ölçekli test |
| `internal/storage` | TimescaleDB toplu yazıcı (`traffic_snapshots`) | Gerçek TimescaleDB ile e2e |
| `internal/config` | TOML yapılandırma, çift mod | Birim testli |

### 1.2 Beacon katmanı (commit `e801830`)

| Dosya | Ne yapar |
|---|---|
| `internal/beacon/beacon.js` | 2.1 KB gzip'li istemci betiği |
| `internal/beacon/event.go` | Olay modeli, metin temizleme (NUL/UTF-8), kampanya beyaz listesi |
| `internal/beacon/visitor.go` | Çerezsiz ziyaretçi kimliği: `HMAC(günlük_tuz, site‖ip‖ua)`, IPv6 /64 |
| `internal/beacon/useragent.go` | Bot sınıflandırma (token sonek eşleme + istisna listesi) |
| `internal/beacon/clientip.go` | XFF'i sağdan sola yürüyen güvenilir vekil çözümü |
| `internal/beacon/server.go`, `writer.go` | Alım sunucusu + toplu yazıcı |

Gerçek headless Chromium ile uçtan uca doğrulandı: 3 satır, referrer
arama terimleri düşürüldü, `secret_token` atıldı, ziyaretçi kimliği
kararlı, headless için `is_bot_ua=true`.

### 1.3 Okuma API'si (commit `f7dce87`)

24 kayıtlı rota (bazıları çoklu kırılım dağıtıyor, efektif ~28 uç):

- **Collector tarafı**: `overview`, `sites`, `summary`, `timeseries`,
  `top-ips`, `ja4`, `countries`, `asns`, `score-distribution`,
  `snapshots`, `ips/{ip}`
- **Beacon tarafı**: `beacon/summary`, `timeseries`, `entry-pages`,
  `exit-pages`, `campaigns`, `events`, `raw`, + `beacon/` altında
  kırılımlar (sayfalar, referrerlar, cihazlar, tarayıcılar, ülkeler…)
- **Kesişim**: `crossover/summary`, `crossover/silent-ips`,
  `crossover/js-bots`
- `bots=exclude|include|only` filtresi her beacon ucunda

İki tarayıcı bağlamıyla doğrulandı: `bots` filtresi 2/4/2 sayfa
görüntüleme, ülkeler yalnızca collector birleşimiyle `TR` çözüldü,
`js_coverage: 0.33`.

### 1.4 Panel çekirdeği (commitler `d31604a`, `a911f87`, `b36b319`)

| Dosya | Ne yapar |
|---|---|
| `internal/panel/schema.sql` | 7 tablo: `panel_users`, `panel_sessions`, `panel_site_members`, `panel_audit_log`, `panel_api_tokens`, `panel_dev_access`, `panel_login_attempts` |
| `internal/panel/password.go` | argon2id, OWASP m=19456,t=2,p=1, PHC kodlu |
| `internal/panel/session.go` | `scs` + Postgres oturum, CSRF eşzamanlayıcı token, oturum sabitleme koruması |
| `internal/panel/totp.go` | TOTP, tekrar koruması (`totp_last_step`) |
| `internal/panel/devaccess.go` | Geliştirici erişimi: iste → onayla → tek kullanımlık |
| `internal/panel/roles.go` | owner/admin/viewer yetki matrisi |
| `internal/panel/audit.go` | Yalnızca ekleme yapılabilen denetim kaydı (GRANT ile) |

**Panel binary'si henüz yok.** `cmd/panel` yazılmadı, şablon yok, HTTP
yüzeyi yok. Var olan, kimlik doğrulama çekirdeği.

### 1.5 Mimari kararlar (yeniden tartışılmayacak)

1. **Dört binary, dört veritabanı rolü.** collector yalnızca
   `traffic_snapshots`'a yazar; API hiçbir şeye yazamaz (`SELECT` only);
   beacon yalnızca `beacon_events`'e ekler; panel yalnızca `panel_*`
   tablolarına yazar ve **analitik tablolara hiç erişemez** — analitiği
   salt okunur HTTP API üzerinden okur.
2. **Yerel depolama, merkezî değil.** Her sitenin collector'ı o VDS'teki
   TimescaleDB'ye yazar. Panel token'la HTTP üzerinden **çeker**. Hiçbir
   müşteri VDS'i veritabanı açmaz.
3. **Okuma anında oturumlaştırma** (30 dk boşluk, pencere fonksiyonu),
   alım anında değil — alım durum tutmamalı, çünkü ziyaretçi başına
   kardinalite saldırganın kontrolünde.
4. **Çerezsiz kimlik**, günlük döner tuz yalnızca bellekte.

---

## 2. Kalan iş — tam sıra

23 madde, beş grup. Sıra bağımlılıktan geliyor: A olmadan B'nin
onaracak bir şeyi yok, B olmadan C'nin gösterecek bir şeyi yok.

---

### A. Operasyonel ayarlar ve saklama süresi

> Bunlar olmadan aşağıdakilerin hiçbiri çalışmaz. Grup A, panelin
> "ayar" kelimesinin bir anlamı olmasını sağlar.

#### A1 — `panel_settings` tablosu

**Ne:** Site başına ve genel, tipli değer saklayan tek tablo.

**Dosyalar:** `internal/panel/schema.sql`, `internal/panel/settings.go`

**Şema tasarımı:**
```sql
CREATE TABLE IF NOT EXISTS panel_settings (
    scope      TEXT NOT NULL,          -- 'global' | 'site'
    site_id    TEXT NOT NULL DEFAULT '', -- scope='global' iken ''
    key        TEXT NOT NULL,          -- kapalı küme, Go'da enum
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by BIGINT REFERENCES panel_users(id) ON DELETE SET NULL,
    PRIMARY KEY (scope, site_id, key)
);
```

**Kritik karar:** `key` asla serbest metin değil. Go'da tanımlı bir
`SettingKey` tipi ve kapalı bir sabit kümesi var; bilinmeyen anahtar
yazma denemesi hata. Sebebi `NOTES.md` "Injection" bölümünde: bir ayarın
sütun veya tablo adlandırması tasarım hatasıdır.

**Değer doğrulama:** her anahtarın kendi `validate(json) error`
fonksiyonu var, sınırlar açıkça yazılı (örn. `flush_interval` 1..300).
Doğrulama **yazmadan önce**, okurken değil.

**Bitti ölçütü:** bilinmeyen anahtar reddediliyor; sınır dışı değer
reddediliyor; eşzamanlı iki yazma testi son yazanın kazandığını ve
`updated_by`'ın doğru olduğunu gösteriyor.

---

#### A2 — Üç profil, adlandırılmış varsayılan kümeleri olarak

**Ne:** Hafif / Dengeli / Tam Crucible / Özel.

**Dosyalar:** `internal/panel/profiles.go`

**Kritik karar:** Profil, kodun dallandığı bir mod **değil**. "Hafif"e
geçmek tek tek ayarları yazar. Panel sonradan biri değiştirilirse
"Hafif (değiştirilmiş)" gösterir. Aksi hâli aynı davranış için iki
doğruluk kaynağı demek olurdu.

| Profil | Beacon | JA4/parmak izi | IP zekâsı | Yaklaşık bellek |
|---|---|---|---|---|
| Hafif | ✔ | ✘ | ✘ | ~0 ek |
| Dengeli (varsayılan) | ✔ | ✔ | yalnız ülke | ~65-70 MB |
| Tam Crucible | ✔ | ✔ | tam (ülke+ASN) | ~135 MB |
| Özel | tek tek | tek tek | tek tek | — |

**Bitti ölçütü:** profil uygulandıktan sonra tek tek ayarların
veritabanında beklenen değerlerde olduğunu doğrulayan test; bir ayarı
değiştirince profilin "değiştirilmiş" olarak raporlandığını doğrulayan
test.

---

#### A3 — `asn_lookup` için yalnız-ülke modu

**Ne:** Bugün `asnlookup` dört aralık tablosu yükler (ülke v4/v6, ASN
v4/v6). Yalnız-ülke modu ikisini yüklemez.

**Dosyalar:** `internal/asnlookup/asnlookup.go`, `parse.go`

**Neden ayrı madde:** Dengeli ile Tam Crucible arasındaki bellek farkı
tam olarak budur — ~135 MB'den ~65-70 MB'ye. Profil bu düğmeye basar.

**Bitti ölçütü:** yalnız-ülke modunda ASN tablolarının hiç
ayrıştırılmadığını (yalnız boş kalmadığını) doğrulayan test; bellek
ölçümü.

---

#### A4 — Saklama süresi politikaları

**Ne:** `traffic_snapshots` ve `beacon_events` için TimescaleDB'nin kendi
`add_retention_policy`'si, artı bunu süren panel ayarı.

**Dosyalar:** `internal/storage/schema.sql`, `internal/beacon/schema.sql`,
`internal/panel/retention.go`

**Bu bir kusur, tercih değil.** Bugün projede hiçbir yerde saklama
politikası yok (`grep` 0 sonuç). İki tablo da sonsuza kadar büyüyor.
Müşteri VDS'inde bu, diskin sessizce dolması ve ilk belirtinin
**collector'ın yazamaması** — yani analitik tablosu yüzünden trafik
yolunun bozulması demek.

**Karar:** varsayılan 90 gün (okuma API'sinin `maxRange`'iyle uyumlu).
Cron `DELETE` değil, chunk düşürme — biri neredeyse bedava, diğeri
değil.

**Bitti ölçütü:** politikanın gerçekten kayıtlı olduğunu
`timescaledb_information.jobs`'tan okuyan entegrasyon testi; süre
kısaltmanın kaç satır sileceğini önden raporlayan fonksiyon.

---

#### A5 — Ayarları TOML'dan veritabanına taşı ⚠️ **en kritik madde**

**Ne:** Operasyonel anahtarları yapılandırma dosyasından `panel_settings`
tablosuna taşımak.

**Neden kritik:** Aşağıdaki 39 operasyonun tamamı, "çalışırken
değiştirilebilir ayar" varsayar. Bugün bu doğru değil. A5 yapılmazsa
onarım kataloğu bir kâğıt parçası olarak kalır ve gerçek prosedür hâlâ
`ssh` + `vim` + `systemctl restart` olur.

**Dosyada kalan (8 anahtar):**

| Anahtar | Neden kalıyor |
|---|---|
| `timescale_dsn` | Veritabanına nasıl ulaşılacağını veritabanına soramazsın |
| `network.listen_addr` | Değişimi zaten yeniden başlatma |
| `network.backend_addr`, `mode` | Aynı |
| `tls.cert_file`, `key_file` | Başlangıçta okunur, dosya sistemine bağlı |
| `site_id` | Hangi kurulum olduğunu adlandırır — kendini veritabanından yeniden adlandırabilen bir kurulum, başkasının sitesine yazmaya ikna edilebilen bir kurulumdur |

**Veritabanına taşınan:** `trusted_proxies`; `[limits]`'in tamamı;
`[asn_lookup]`'ın tamamı (iki blok listesi ve skorlama listesi dâhil);
`flush_interval_seconds`; beacon tampon/parti boyutları; beacon site
beyaz listesi; analitik profili ve site başına derinlik; saklama ve
sıkıştırma politikaları; bot skor eşiği; önbellek pencere ve TTL'leri;
log seviyesi ve log saklama süresi.

**Bitti ölçütü:** `collector.example.toml` sekiz anahtara inmiş; her
taşınan ayar için "dosyada yoksa veritabanından okunur" testi; geriye
dönük uyumluluk — eski dosyadaki değer varsa bir kez veritabanına
göç ettirilip dosyadan yok sayılır ve bu bir denetim kaydı üretir.

---

#### A6 — Servislerin ayarı canlı okuması

**Ne:** collector ve beacon, ayar satırını kısa aralıkla (bir dakika
uygun) yeniden okur ve canlı değerleri atomik olarak değiştirir.

**Dosyalar:** `internal/config/live.go` (yeni), `cmd/*/main.go`

**Hata modu — bilerek yazılıyor:** **Okuma başarısız olursa son bilinen
değerler korunur.** Naif hâli ("hata olursa varsayılana dön") bir
veritabanı kesintisinde müşterinin ayarlarını sessizce sıfırlar. Bu,
bayat bir ayardan daha kötü ve fark edilmesi çok daha zor bir sonuçtur.

**Maliyet, açıkça:** bu, veritabanını collector'ın *davranışının* da
bağımlılığı yapar, yalnız depolamasının değil. Kabul ediliyor, çünkü
alternatif SSH.

**Bitti ölçütü:** veritabanı bağlantısı kesilirken servisin son değerle
çalışmaya devam ettiğini gösteren test; ayar değişiminin bir aralık
içinde etki ettiğini gösteren test; `-race` altında temiz.

---

### B. Gözlemlenebilirlik — SSH'i gereksiz kılan şey

#### B1 — `panel_logs`

**Ne:** Dört servis de `log/slog` kullanıyor. İkinci bir handler
`panel_logs` tablosuna yazar ki panel SSH'siz gösterebilsin.

**Dosyalar:** `internal/panel/logs.go`, `schema.sql`

**Bariz tuzak:** bir log tablosu veritabanının en büyük tablosu olur —
tam da A4'te tarif edilen disk sorunu. O yüzden:

- Varsayılan olarak yalnız WARN ve üstü kalıcı.
- Site başına "ayrıntılı kayıt" anahtarı DEBUG'a çıkarır ve **kendi
  kendine söner** (varsayılan bir saat, 1..120 dk sınırlı). Unutulduğu
  için açık kalan ayrıntılı log, diskin dolma yoludur.
- Kendi saklama süresi, analitik tablolarınkinden çok daha kısa.
- Satır uzunluğu sınırlı, ve `internal/beacon`'ın ihtiyaç duyduğu aynı
  NUL/UTF-8 temizliği — log satırları kullanıcı kontrollü metin içerir
  ve tek bir düşmanca dize yazıcıyı bozmamalı.

**Bitti ölçütü:** düşmanca dize (NUL, geçersiz UTF-8, 1 MB tek satır)
yazıcıyı bozmuyor; ayrıntılı anahtar süresi dolunca kendi kapanıyor.

---

#### B2 — `panel_operations` — operasyon günlüğü

**Ne:** Denetim kaydı "kim ne yaptı"yı cevaplar. Bir arızayı teşhis etmek
"onlar yaparken ne oldu"yu gerektirir — farklı ve çok daha ayrıntılı bir
kayıt, o yüzden kendi kısa saklama süresi olan ikinci bir tablo.

**Her ayar değişimi bir operasyondur ve şunları taşır:**

- Değişimin ürettiği her log satırında taşınan bir korelasyon kimliği
- Aktör ve ait olduğu denetim kaydı
- Ayarın önceki ve sonraki değeri
- Her adım ve sonucu
- Başarısızlıkta tam hata zinciri **ve değişimin geri alınıp
  alınmadığı**

Son alan en önemlisi. "Bir şeyi ayarlarken hata olmuş" ancak yarım
uygulanmış bir değişiklik *yarım uygulanmış olarak kaydedilirse*
cevaplanabilir.

---

#### B3 — Onarım operasyonları (39 adet)

**Ne:** Adlandırılmış, tipli, sınırları doğrulanmış Go fonksiyonları.
Tam katalog aşağıda §4'te. Her biri günlüklenir.

**Kalıcı olarak yasak:** "SQL çalıştır" kutusu; yapılandırma metin
alanı; kabuk komutu; komut dizesi alan bir "yeniden başlat"; parametresi
kod, sorgu, dosya yolu veya panelin bağlanacağı bir alan adı olan
herhangi bir operasyon.

---

#### B4 — Sistem sağlığı sayfası

**Ne:** "VDS'e girmeyi azalt" için en yüksek değerli şey onarım değil,
kimse aramadan neyin bozuk olduğunu bilmek. Salt okunur bir sayfa, kurulum
başına:

- collector yazıyor mu, en son ne zaman başarılı oldu
- beacon olay alıyor mu, sonuncusu ne zamandı
- tablolar ne kadar büyük, saklama yapılandırılmış ve çalışıyor mu
- IP aralık tabloları yüklü mü, ne kadar eski
- okuma API'sine ulaşılıyor mu
- son yazma hataları, düşürülen olay sayısı, kısıtlanan girişler

**Bunların hepsi bugün zaten içeride ölçülüyor** — sayaçlar var, hiçbiri
yüzeye çıkmıyor.

---

#### B5 — Destek token'ı ve kimlik doğrulamalı sağlık ucu

**Ne:** Geliştirici ayarlarında üretilen, **her şeyi okuyan, hiçbir şey
yazmayan** bir token.

Sağlık-kapsamlı olması yeterince geniş değildi: bir kurulumu izlemek,
analitiğin kendisini gerektiren soruları cevaplamak demek — SEO çalışıyor
mu, gerçek ziyaretçi geliyor mu, yeni açılan site trafik alıyor mu, geçen
salı sayılar uçurumdan mı düştü. Bunların hiçbiri bir canlılık
kontrolünde görünmez.

Token, zaten var olan `api.Token{Sites: []string{"*"}}` şeklinde sıradan
bir joker token. Güvenliğini kapsamı değil, arkasındaki duvar sağlar:

- Okuma API'sinin veritabanı rolü yalnızca `SELECT` yapabilir. Yazacak
  bir şeyi olmayan API üzerinden token yazamaz — "salt okunur" hata
  yapabilecek uygulama kodu tarafından değil, PostgreSQL tarafından
  zorlanır.
- İki tarafta da yalnız SHA-256 saklanır.
- Yalnız okuma API'sine ulaşır. Panel oturumu değildir: ayar
  değiştiremez, üye ekleyemez, token üretemez, geliştirici erişimi
  onaylayamaz.

**Doğru anlaşılması gereken kısım, açıkça:** bu, devirden sonra
müşterinin ticari verisine kalıcı erişimdir. Trafik hacimleri, hangi
sayfaların sattığı, kampanyaların ne zaman düştüğü — başka birinin
şirketi hakkında ticari açıdan hassas bilgi. Salt okunur olması onu
"erişim değil" yapmaz.

Meşru tedarikçi izlemesini arka kapıdan ayıran şey yetkinin kendisi
değil, **sahibin biliyor ve hayır diyebiliyor olmasıdır.** O yüzden:

- Token, sahibin panelinde düz bir adla — **"Crucible destek erişimi"** —
  kendi API token'larının yanında listelenir; hiç açmadıkları bir
  geliştirici sayfasında gizlenmez.
- **Tek tıkla, bize sormadan iptal edilebilir.**
- **Son kullanımı gösterilir**, böylece "gerçekten verime bakıyorlar mı"
  sorusunun güvene değil kontrole dayalı bir cevabı olur.
- Üretilmesi, diğerleri gibi bir denetim kaydıdır.

İptal eden müşteri proaktif desteği kaybeder, başka hiçbir şeyi
kaybetmez. Bu takas onların. Ve iptal edilebilir olması, kurulumu
"onların" diye tanımlamayı dürüst kılan şeydir.

`/healthz` olduğu gibi kalır — kimlik doğrulamasız, verisiz, yük
dengeleyiciler için. Ayrıntılı sürüm ayrı ve kimlik doğrulamalı bir
rotadır.

**İzleme yönü — biz onları yokluyoruz, onlar bizi hiç aramıyor.** Daha
önceki bir taslak dışa dönük bir kalp atışı öneriyordu; reddedildi.
Müşterinin sunucusunun tedarikçisine istenmemiş bağlantı açması, bu
tasarımın kaçındığı arka kapının adil bir tanımıdır. Doğru şekil bir
**çekme**: kurulumdaki kimlik doğrulamalı sağlık ucunu bizim sistemimiz
programlı olarak yoklar. Müşterinin makinesinden istenmeden hiçbir şey
çıkmaz; yoklama sorun bildirdiğinde telefon eder ve geliştirici erişimi
isteriz — sanki onlar aramış gibi. Müşteri fark etmeden o telefonu
açabilmek, değerin büyük kısmıdır.

---

### C. Panelin HTTP yüzeyi

#### C1 — Türkçe katalog, şablonlar, gömülü HTMX, CSS

**Ne:** Tüm metinler tek katalogda (`internal/panel/messages.go` veya
`messages.tr.toml`, `go:embed` ile). `html/template`. HTMX ve CSS
binary'ye gömülü — CDN yok, npm yok, derleme adımı yok.

**Neden bu yığın:** "Kurulum ve çalıştırma yükünü de azaltacak şekilde
olmalı… 'şurada nginx'te bunu ayarla, burada şu var' istemiyorum."
Gömülü statik varlıklar tek dosya dağıtımı demek.

#### C2 — İlk çalıştırma tespiti ve geliştirici sihirbazı

Hiç hesap yokken geliştirici erişimiyle ulaşılır. Teknik zemini kapsar:
veritabanı bağlantısı ve şema uygulaması, hangi sitelerin olduğu,
collector modu ve backend, TLS, güvenilir vekiller, analitik profili,
saklama süresi. Kurulumun devre hazır olduğunu onaylayarak biter.

#### C3 — Sahip sihirbazı ve teknik kapı

Müşterinin gördüğü ilk şey. Hesabını oluşturur, siteyi kendi diliyle
adlandırır, saat dilimini ayarlar, gömülecek snippet'i gösterir,
meslektaş davetini önerir. **Asla teknik bir adım zorunlu kılmaz** —
çünkü onlar zaten yapıldı; teknik bir değere ihtiyaç duyduğunda boş alan
değil, geliştiricinin yapılandırdığını gösterir.

Sahip sihirbazı, teknik sihirbaza gösterişsiz bir bağlantı taşır. İlk
tıklama onu açmaz; uyarır:

> Bu bölüm geliştiriciniz tarafından tamamlandı. Yine de baştan yapmak
> isterseniz onaylayın.

Onaylamak tam teknik sihirbazı açar. Uyarı var çünkü yaygın durum birinin
merak edip gezinmesi ve çalışan bir kurulumu kazara yeniden
yapılandırmak en iyi ihtimalle bir destek çağrısı. Gizli sayfa değil de
onay olması bilinçli: **sunucu onların**, ve teknik bir sahip kendi
ayarlarına ulaşmak için bizden izin istemek zorunda kalmamalı.

#### C4 — Giriş, iki faktör, hesap ayarları, üye yönetimi

Çekirdek yazıldı (B grubu commitleri); kalan HTTP yüzeyi ve şablonlar.
Roller: owner / admin / viewer. Viewer teknik görünümleri hiç görmez ve
viewer bölümünde bir uyarı olur.

#### C5 — Geliştirici erişimi onay ekranı

Bekleyen istek afişi, onayla ve reddet. Sahibin gördüğü şey: kim, neden
(istekte yazılan gerekçe), ne kadar süre.

**Politikanın tamamı tek `WHERE` cümlesinde** (yazıldı, `devaccess.go`):

```sql
WHERE sha256 = $1
  AND used_at IS NULL
  AND denied_at IS NULL
  AND approved_at IS NOT NULL
  AND request_expires_at > now()
  AND (NOT auto_approved OR NOT EXISTS (SELECT 1 FROM panel_users))
```

Son satır kuralın kendisi: makineye kabuk erişimi, **kimsenin hesabı
yokken** girmeye yeter; **sonrasında yetmez.**

---

### D. Panelin kendisi

#### D1 — Site seçici, sonra site başına altı kartlık varsayılan görünüm

#### D2 — Detaya inişler: sayfalar, kaynaklar, kampanyalar, cihazlar, ülkeler, olaylar

#### D3 — Aynı sayfalar üzerinde geliştirici modu katmanları

Parmak izleri, ASN'ler, skorlar, kesişim görünümleri, ham dışa aktarma.
**Yeni sayfa değil, aynı sayfaya sütun.** Sayfalar sayfası sütun kazanır;
farklı bir sayfalar sayfasına dönüşmez.

#### D4 — Ayar sayfaları ve akan operasyon penceresi

Her ayar değişimi, o operasyonun kendi log satırlarını akıtan bir pencere
açar, sonra kapanır. İki düzeltme:

- **Her şeyi değil, o operasyonun satırlarını akıtır.** İlgisiz bir
  değişiklik sırasında tüm sistemin logunu gösteren pencere gürültüdür,
  ve gürültü insanların okumadan tıklamayı öğrendiği şeydir. Korelasyon
  kimliği bunu mümkün kılan şey.
- **Hiçbir şey meşgul görünsün diye şişirilmez veya uydurulmaz.**
  Belirtilen ikincil amaç — teknik olmayan müşterinin ayarlarla
  oynamadan önce iki kez düşünmesi — gerçek sistem logları tarafından
  zaten mükemmel biçimde karşılanıyor. Uydurulmuş satırlar tiyatro
  olurdu ve onları dikkatle okuyan ilk teknik müşteri, panelin söylediği
  her şeye güvenmeyi bırakırdı. Caydırıcılık, dürüstlüğün yan etkisi
  olmalı; özellik olursa kazandırdığından fazlasına mal olur.

#### D5 — Profil, veriyi çıkarır; sayfayı asla

Panel tam ürüne göre yazılır. Hafif profiller **veri** kaldırır, sayfa
değil. Her görünüm her profilde vardır. Mevcut profil bir görünümün
ihtiyaç duyduğunu toplamıyorsa, görünüm bunu düz söyler ve değiştirecek
ayara bağlantı verir:

> Bu veri şu anki modda toplanmıyor. → Dengeli moda geç

Üçü de kolayca yanlış yapılabilecek üç sonuç:

- **Görünüm asla gizlenmez.** Gizlemek, profilini yükselten müşterinin ne
  satın aldığını hiç keşfetmemesi ve bizi rakiple karşılaştıran
  müşterinin sahip olduğumuzdan kısa bir özellik listesi görmesi demek.
- **"Toplanmıyor" asla sıfır olarak çizilmez.** Bot tespiti kapalıyken
  "0 bot" göstermek, bu projenin başka yerde reddettiği türden bir
  yalandır — `beacon_events`'in boş-grup bayrağının ve `asnlookup`'ın
  `''`-çözülmedi demek olan sözleşmesinin yaptığı ayrımın aynısı.
  **Sıfır, baktık ve bulamadık demektir. Yokluk, bakmadık demektir.**
- **Profil, etkilediği her görünümde belirtilir**, böylece bir sayının
  ekran görüntüsü onu okumak için gereken bağlamı hep taşır.

**Ayrımı düz tutmak gerek: profil neyin *toplandığına*, geliştirici modu
neyin *gösterildiğine* karar verir.** Geliştirici modu kapalı bir Tam
Crucible kurulumu yine her şeyi toplar — sahip bugün parmak izlerine
bakmıyordur sadece. Anahtarı açmak, toplamayı başlatmaz; zaten var olan
veriyi görünür kılar.

#### D6 — Tasarım ilkesi: sade yüzey, sınırsız derinlik

- Varsayılan görünüm altı kart ve bir grafik, sıradan Türkçe. Parmak izi
  yok, ASN yok, skor yok, İngilizce jargon yok.
- **Her sayı bir kapıdır.** Kart tıklanabilir ve onu açıklayan bir yere
  götürür; hiçbir şey çıkmaz sokak değil.
- Teknik hiçbir şey istenmeden görünmez, ve istemek tek yerde tek
  anahtardır.
- **Derinliğe tıklayarak ulaşılır, okuyarak asla.** Bir görünüm
  kullanılabilir olmak için bir paragraf açıklama gerektiriyorsa görünüm
  yanlıştır — paragraf, gerekli olan tek terimin ipucu balonuna aittir.

---

### E. Birleştirme ve sertleştirme

#### E1 — `cmd/crucible`, alt komutlar ve tek yapılandırma dosyası

Bugün dört binary, üç örnek TOML. Hedef: `crucible collector`,
`crucible beacon`, `crucible api`, `crucible panel`,
`crucible dev-access request`, tek `crucible.toml`.

**Not:** ayrı veritabanı rolleri korunur — birleşme dağıtımı
kolaylaştırmak içindir, ayrıcalık ayrımını gevşetmek için değil.

#### E2 — SQL yakınındaki `Sprintf` testi

Paket kaynağını sorgu kurma kalıpları için tarar ve enterpolasyon yapan
çağrı noktalarının kümesinin bilinen bir listeyle eşleştiğini doğrular.
Kaba bir kontrol, ve iki yıl sonra sabaha karşı eklenen tek dikkatsiz
satırı yakalayacak olan tam da bu kaba kontroldür.

#### E3 — README'nin ürün olarak yeniden yazımı

Bugünkü README collector'ı anlatıyor. Ürün dört parça.

---

## 3. Kalıcı güvenlik kuralları

Bunlar madde değil, her maddeye uygulanan kısıtlar.

### 3.1 Enjeksiyon ve veritabanının patlama yarıçapı

**Bugün geçerli olan:**

- Her paketteki her sorgu bağlı parametre kullanıyor. Değerler asla SQL
  metni olmuyor.
- Bir dizenin sorguya biçimlendiği **tam olarak iki** yer var, ikisi de
  hiçbir isteğin ulaşamayacağı **kapalı bir paket sabiti kümesinden bir
  sütun adı** yerleştiriyor: `api.Store.countDistinct` ve
  `api.Store.beaconBreakdown`. İkisinde de bunu söyleyen bir yorum var ve
  `breakdownExpr` özellikle istekten türeyen bir dize kazara
  geçirilemesin diye ayrı bir tip.
- Rol ayrımı, tek bileşenin tamamen ele geçirilmesinde bile patlama
  yarıçapını sınırlıyor.

**Eklenecek:**

- E2'deki `Sprintf` testi.
- Ayar değerleri SQL'e yalnız bağlı parametre olarak ulaşır, asla
  tanımlayıcı olarak. Sütun veya tablo adlandırması gereken bir ayar
  tasarım hatasıdır; eşleme Go'da, doğrulanmış bir enum ile anahtarlanır.
- Panelin onarım operasyonları tipli parametrelerini, herhangi biri
  veritabanına ulaşmadan **önce** açık sınırlara karşı doğrular.

### 3.2 Kimlik doğrulama kararları (yazıldı)

| Karar | Neden |
|---|---|
| argon2id m=19456,t=2,p=1 | 64 MiB **bilerek değil**: her denemede 64 MiB ayıran bir giriş salvosu, collector'ın koruduğu sitenin kendisine karşı bir DoS'tur |
| PHC kodlu hash | Maliyeti sonra yükseltmek, göç olmadan eski hash'leri doğru doğrular |
| Geri okurken maliyet parametreleri sınırlı | 16 GiB'lik bir ayırmayı engeller |
| `RenewToken` her şeyden önce | Oturum sabitleme; tek satır, eksikken görünmez, elle yazılmış giriş kodunda en sık atlanan adım |
| `totp_last_step`, kontrol+kayıt tek `UPDATE`'te | Kod üç adım geçerli (saat kayması için); omuz üstünden görülen bir kod 90 saniye kullanılabilir kalırdı |
| SameSite=Lax + eşzamanlayıcı token | Lax çoğu tarayıcıda yeter; "çoğu" o cümlede gerçek iş yapıyor |
| Token yokken CSRF **kapalı** başarısız | Doğrulanmamışı kabul etmek yerine yazmayı engeller |

### 3.3 Tek ifadelik atomik durum geçişleri

Son sahip koruması (`FOR UPDATE`), geliştirici erişimi kullanımı, TOTP
adımı — hepsi tek ifade, ve eşzamanlı testlerle tam olarak bir
kazananın olduğu doğrulandı.

---

## 4. Onarım operasyonları kataloğu — 39 adet

Her giriş: adlandırılmış Go fonksiyonu, tipli parametreler, veritabanına
ulaşmadan önce açık sınırlara karşı doğrulanmış, korelasyon kimliğiyle
günlüklenmiş, geri almanın anlamlı olduğu her yerde geri alınabilir.

### A grubu — Toplama durdu ya da sayılar yanlış (13)

| # | Operasyon | Sınır / tip | Ne zaman |
|---|---|---|---|
| 1 | `FlushNow()` | parametresiz | "Kayıtlar on dakikadır gelmiyor" — flush zamanlayıcısı takıldığında; ilk denenecek çünkü bedava |
| 2 | `SetFlushInterval(seconds)` | 1..300 | Yavaş disk daha uzunu, teşhis daha kısasını ister |
| 3 | `PauseCollection(site)` / `ResumeCollection(site)` | site kimliği | Vekili durdurmadan yalnız **kaydı** durdur. Olay ortasında güvenli hamle: trafik siteye ulaşmaya devam eder, disk dolmayı bırakır, biz çalışırken kimse satış kaybetmez |
| 4 | `ReloadIPData()` | parametresiz | "Ülkeler yanlış" / "ülke sütunu tamamen boş" |
| 5 | `SetASNLookupMode(mode)` | `off`\|`country_only`\|`full` | Hem gizlilik ayarı hem CPU yiyen aramanın çözümü |
| 6 | `SetASNSource(source)` | derlenmiş kaynak enum'u | `local_csv`, sabit izinli dizin listesinden seçer — **serbest metin yol değil**, çünkü serbest metin yol bir dosya-okuma ilkelidir |
| 7 | `SetTrustedProxies(prefixes)` | `netip.Prefix`, metin olarak asla | **Katalogda en üstte olmayı hak ediyor:** Cloudflare arkasında boş liste, her ziyaretçiyi aynı IP gösterir ve **sistemdeki diğer her sayıyı aynı anda yanlışlar.** En sık gerçek yanlış yapılandırma bu ve bugün bir SSH oturumuna mal oluyor |
| 8 | `SetLimits(maxConcurrent, maxRPS)` | 1..100000 | Collector'ın kendisi darboğaz olduğunda |
| 9 | `SetOverloadPolicy(policy)` | `fail_open`\|`fail_closed`\|`throttle` | — |
| 10 | `SetThrottleQueueSize(n)` | 0..10000 | — |
| 11 | `SetBotScoreThreshold(site, score)` | 0..100 | "Gerçek müşterilerim bot sayılıyor" — bu bir ayar sorunu, kabuk gerektirmemeliydi |
| 12 | `SetBlockedCountries` / `SetBlockedASNs` | ISO 3166-1 derlenmiş liste / atanmış ASN aralığı | Yanlış giriş gerçek trafiği sessizce atar, o yüzden operasyon kabul etmeden önce kural başına son 24 saatin isabet sayısını gösterir |
| 13 | `SetKnownBotASNs(asns)` | aynı doğrulama | Skorlama sinyali |

### B grubu — Disk ve veritabanı (8)

| # | Operasyon | Sınır / tip | Ne zaman |
|---|---|---|---|
| 14 | `ShowTableSizes()` | salt okunur | Her seferinde ilk soru olduğu için grubun başında |
| 15 | `SetRetention(table, days)` | kapalı tablo enum'u, 1..3650 | Kaç satır sileceğini söyleyen ikinci bir onay olmadan **kısaltmayı reddeder** — iki yıllık veriye karşı "30 yap" yıkıcıdır ve öyle hissettirmeli |
| 16 | `RunRetentionNow()` | parametresiz | Zamanlanmış işi bekleme |
| 17 | `SetCompressionPolicy(table, afterDays)` | 1..3650 | Genelde silmekten iyi cevap: müşteri geçmişini korur, diskin çoğunu geri alır |
| 18 | `ApplyPendingMigrations()` | parametresiz | Projenin kendi `ALTER TABLE … IF NOT EXISTS` bloklarını çalıştırır. DDL binary'ye derlenmiş dosyadan gelir, **asla parametreden**, ve yapısı gereği idempotent. **Bugün `psql`'e uzanmamızın en yaygın sebebini tek başına ortadan kaldırır:** şema dosyasına sütun ekleyen ama kimsenin yeniden uygulamadığı bir yükseltme |
| 19 | `CreateMissingIndexes()` | derlenmiş liste | `CREATE INDEX CONCURRENTLY IF NOT EXISTS` |
| 20 | `ReindexTable` / `AnalyzeTable` / `VacuumTable` | kapalı tablo enum'u | `AnalyzeTable`, "dashboard birden yavaşladı"nın çözümü olarak her şeyden sık çıkar |
| 21 | `ShowSlowQueries()` | salt okunur | `pg_stat_statements` varsa; yoksa temiz atlanır |

### C grubu — Erişim ve kilitlenme (7)

| # | Operasyon | Sınır / tip | Ne zaman |
|---|---|---|---|
| 22 | `SendOwnerPasswordReset(userID)` | tek kullanımlık bağlantı | Hesapta zaten kayıtlı adrese. **Şifreyi hiç görmeyiz, koymayız, seçmeyiz.** Ayrım önemli: şifre koyan bir operasyon, sahibi taklit eden bir operasyondur |
| 23 | `DisableTOTP(userID, reason)` | yazılı gerekçe zorunlu | Gerçekten sıkışmış durum: telefon kayıp, kurtarma kodu yok. **Katalogun en gürültülü operasyonu bilerek:** gerekçe ister, denetim kaydı yazar, sahibin panelinde yalnız sahibin kapatabileceği bir afiş kaldırır. Kötüye kullanıma en açık giriş, o yüzden sessizce yapılamayan giriş |
| 24 | `EndAllSessions(userID)` | — | Çalınan dizüstü düğmesi |
| 25 | `UnlockLoginThrottle(emailOrIP)` | — | Kendi şifresini tahmin etmeye çalışırken kendini kilitleyen müşteri |
| 26 | `GrantOwnership(userID, siteID)` | — | "Tek sahip şirketten ayrıldı" — aksi hâlde kurulum kalıcı olarak sahipsiz kalır |
| 27 | `RevokeAPIToken(id)` / `RevokeAllTokens(siteID)` | — | Sızan token'ın hak ettiği hızda |
| 28 | `SetDeveloperMode(userID, enabled)` | — | Teknik müşterinin kendi geliştirici görünümünü telefonda açmak; anahtarı bulmasını tarif etmek yerine |

### D grubu — Beacon (5)

| # | Operasyon | Sınır / tip | Ne zaman |
|---|---|---|---|
| 29 | `ShowBeaconStatus(site)` | salt okunur | Son olay, son bir saatteki olay sayısı, reddedilen sayısı **ve her reddedilme sınıfının sebebi**. Hep ilk soru: "JS verisi hiç gelmiyor" genelde beyaz listede olmayan bir sitedir, ve bugün bunu öğrenmek SSH'te log okumak demek |
| 30 | `ShowBeaconSnippet(site)` | salt okunur | Site kimliği doldurulmuş birebir `<script>` etiketi. Bütün bir destek çağrısı kategorisini yok eder |
| 31 | `SetBeaconSites(sites)` | site kimliği karakter kümesi | Beyaz liste |
| 32 | `SetBeaconBuffer(size, batch, flushSeconds)` | sınırlı | — |
| 33 | `TestBeaconIngest(site)` | — | Sentetik olarak işaretli tek bir olay yazar (gerçek rakamları asla kirletmez), sonra ulaşıp ulaşmadığını ve ne kadar sürdüğünü raporlar. Müşteriden "biz bakarken sitene gir" demeden tüm yolu ispatlar |

### E grubu — Derinlik, kayıt, profil (4)

| # | Operasyon | Sınır / tip |
|---|---|---|
| 34 | `SetAnalyticsProfile(site, profile)` | `hafif`\|`dengeli`\|`tam` |
| 35 | `SetVerboseLogging(site, minutes)` | 1..120, **kendi kendine söner** — kazara açık kalan ayrıntılı kayıt, diskin dolma yoludur |
| 36 | `SetLogRetention(days)` / `SetLogLevel(service, level)` | kapalı enum |
| 37 | `ExportDiagnosticBundle()` | değerleri maskeli | Ayarlar, sağlık, son WARN+ loglar ve tablo boyutları tek dosyada. Bir insanın her şeye aynı anda bakması gereken durumlar için — o insanın toplamak için kabuğa ihtiyacı olmadan |

### F grubu — Süreç (2)

| # | Operasyon | Sınır / tip |
|---|---|---|
| 38 | `RestartService(service)` | kendi dört servisimiz üzerinde kapalı enum. **Komut dizesi yok, argüman yok, yol yok.** "Temiz çık, systemd yeniden başlatsın" olarak uygulanır: panel hiçbir şey **spawn etmez**. Önemli özellik bu — yalnızca *durdurabilen* bir onarım yüzeyi, ne yapılırsa yapılsın rastgele bir şey başlatan bir yüzeye çevrilemez |
| 39 | `ReloadConfig(service)` | dosyada kalanı yeniden okur, yeniden başlatmadan |

**Listenin şekli asıl mesele:** neredeyse hepsi ayar değişimi ve salt
okunur inceleme; gerçekten güçlü olan bir avuç (22, 23, 26, 15) sessizce
yapılmayı imkânsız kılan bir şeye sarılı.

---

## 5. SSH'siz yapılamayacaklar — yumuşatmadan

Panel, çalışmayan bir paneli onaramaz. Hiçbir katalog bunu değiştirmez.
Fiziksel kalanlar:

- Süreç başlamıyor ya da açılışta çöküyor
- Veritabanı başlamıyor ya da bağlantı kabul etmiyor
- **Disk işletim sistemi seviyesinde dolu** — yazma başarısız olduğunda,
  onarımı kaydedecek operasyon günlüğü de yazılamaz
- TLS sertifika yenilemesi ve dosya sistemiyle ilgili her şey
- Binary yükseltme
- Yedekten dönme
- Yukarıdaki sekiz yapılandırma anahtarı: DSN, dinleme adresleri, TLS
  yolları, `site_id`
- **Panelin kendisinin bozuk bileşen olması**

Bu liste hakkında dürüst gözlem: **her madde kurulum zamanı ya da makine
seviyesi bir mesele; hiçbiri "analitik yanlış / veri gelmiyor / ayarı
değiştir" meselesi değil.** Burada yapılan iddia tam olarak bu.
"SSH'siz her şey" ulaşılabilir değil ve vaat edilen de o değil;
ulaşılabilir olan şu: **müşterinin gerçekten telefon açtığı problem
sınıfının tamamı panelden onarılabilir**, ve geriye kalan, makinenin
kendisinin bir insana ihtiyaç duyduğu sınıf.

---

## 6. Açık teknik borç (bilinen, kabul edilmiş)

Bunlar plan maddesi değil; bilinen sınırlar. Biri gerçekten sorun
olursa madde olur.

| Konu | Durum |
|---|---|
| `/api/v1/overview`'da beacon sayıları yok | Siteler arası ikinci bir sorgu ve "bir sitede yalnız bir kaynak çalışıyorsa karışık satır ne demek" kararı gerektirir |
| Oturumlar aralık sınırında kesiliyor | Aralık kapsamlı oturumlaştırma için standart; panel tam günleri tercih etmeli. Sonuç: oturum sayıları komşu aralıklar arasında toplanabilir değil |
| Oturum ağırlıklı uçlar tüm aralığı sıralıyor | Tek sitenin hacminde yerel veritabanında sorun değil; olmadığı gün cevap sürekli toplam (continuous aggregate) |
| Kesin kümülatif istek sayısı yok | `traffic_snapshots` bir istek kaydı değil, kayan pencerenin periyodik **örneği**. Ardışık örnekler örtüşüyor. Denenen ve reddedilen çözüm: örneklenmiş hızı zamana göre integre etmek — aritmetik doğruydu, sentetik test tam geçti, gerçek çalıştırma 38 isteği 3 olarak raporladı. **Ders: sentetik test aritmetiği doğruladı, öncülü değil.** Kesin toplam, `ratestore`'da monotonik sayaç ister — üç pakete yayılan gerçek bir tasarım değişikliği |
| ASN skorlama ağırlığı düz bonus | `KnownBotJA4`'ün düz bonus şeklini yansıtıyor; ağırlıklı şekil gerekene kadar yapılmayacak |
| Ülke/ASN kural motoru yalnız engelleme listesi | Kural başına politika motoru gerçek ek karmaşıklık; gerçek ihtiyaç çıkana kadar bekliyor |

---

## 7. Kapsam dışı — varsayılmasın diye

- **Mobil uygulama.**
- **Son müşteriye uyarı** (e-posta, webhook). Sağlık yoklaması bizim
  tarafımızda; müşteriye giden otomatik uyarı bu yayda yok.
- **Merkezî analitik.** Topoloji bunu sonradan mümkün bırakıyor (panel
  çektiğini kendi deposuna aynalayabilir — itme değil çekme yoluyla
  "merkezî analitik"), ama şimdi yapılmıyor. Yapılırsa çözülmesi gereken
  şey yazılı: N collector paylaşılan `ip_country_ranges` /
  `ip_asn_ranges` tablolarını kendi tazeleme takvimlerinde
  `TRUNCATE`+`COPY` ile birbirini ezer; tek görevli tazeleyici ya da her
  yerde `local_csv_path` gerekir.

---

## 8. Sıra ve bağımlılık haritası

```
A1 panel_settings ─┬─> A2 profiller ──────────────┐
                   ├─> A4 saklama süresi ─────────┤
                   └─> A5 TOML→DB göçü ─> A6 canlı okuma
                                                   │
A3 yalnız-ülke modu ───────────────────────────────┤
                                                   ▼
                              B1 panel_logs ─> B2 operasyon günlüğü
                                                   │
                                                   ├─> B3 39 operasyon
                                                   ├─> B4 sağlık sayfası
                                                   └─> B5 destek token'ı
                                                   │
                                                   ▼
              C1 şablonlar ─> C2 gel. sihirbazı ─> C3 sahip sihirbazı
                           └> C4 giriş/üye ─> C5 onay ekranı
                                                   │
                                                   ▼
                      D1 site seçici ─> D2 detaylar ─> D3 gel. katmanları
                                                   └─> D4 ayar + akan pencere
                                                   │
                                                   ▼
                                E1 tek binary ─> E2 Sprintf testi ─> E3 README
```

**En kritik tek bağımlılık A5.** Yapılmazsa B3'ün değiştirecek bir şeyi
yoktur ve tüm "SSH'siz onarım" iddiası çöker.

---

## 9. Her madde için "bitti" ne demek

Bu projede bir madde, şu üçü olmadan bitmiş sayılmıyor — bugüne kadar
böyle yapıldı, devam ediyor:

1. **Birim testleri** ve `go test -race ./...` temiz.
2. **Gerçek bağımlılıkla doğrulama.** Sahte değil: gerçek TimescaleDB,
   gerçek headless Chromium, gerçek eşzamanlı yük. Bu projede bir
   defekt (kümülatif istek sayısı) yalnızca gerçek çalıştırmayla
   yakalandı; sentetik test tam geçmişti.
3. **Yazılı gerekçe.** Karar `NOTES.md`'ye, kullanım README'ye.

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

34 madde beş grupta (A–E), artı ertelenen 3 madde (F). Sıra
bağımlılıktan geliyor: A olmadan B'nin onaracak bir şeyi yok, B olmadan
C'nin gösterecek bir şeyi yok.

⚠️ işaretli maddeler, ilk taslakta **hiç yoktu** ve sonradan bulunan
gerçek boşluklar: A7 (IP/KVKK), A8 (zaman dilimi), A9 (ziyaretçi
gizlilik kartı), B6 (çok müşterili yalıtım), B7 (sürüm), C6
(yapılandırılabilir kart seti), C7 (boş durumlar / API kesintisi /
e-posta yolu), D7 (arama motoru botları).

---

### A. Operasyonel ayarlar ve saklama süresi

> Bunlar olmadan aşağıdakilerin hiçbiri çalışmaz. Grup A, panelin
> "ayar" kelimesinin bir anlamı olmasını sağlar.

#### A1 — `panel_settings` tablosu ✅ **yapıldı**

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

**Yazıldı ve doğrulandı:** bilinmeyen anahtar reddediliyor; sınır dışı
değer reddediliyor; kapsam uyuşmazlığı (genel ayara site verilmesi)
reddediliyor; site değeri genel değeri geçiyor; **elle veritabanına
sokulmuş sınır dışı bir değer okunurken varsayılana düşüyor** — eski bir
yapının veya elle düzenlemenin, servise yazıldığı sınırların dışında bir
değer verememesi için. Saklanan bir değeri geri okurken de doğrulamak,
argon2 maliyet parametrelerini sınırlamakla aynı gerekçe.

---

#### A1.5 — Günlük kaydı yaşam döngüsü ✅ **yapıldı**

**Ne:** Logların ömrü tek sayı değil **üç aşama**, ve aşamalar tüm
logların eşit değerde olmamasından doğuyor.

`access` ve `ingest` devasa ve yaklaşık bir hafta ilginç. `security`,
`auth` ve `audit` küçük ve **bir yıl sonra sorulan** tam olarak bunlar —
"kim girdi, ne zaman". Tek bir saklama süresi ya ikinci grubu atardı ya
birinci grubu sonsuza kadar tutardı; ve diski dolduran birinci grup, ki
dolan disk collector'ı durdurur — analitik özelliğinin trafik yolunu
düşürmesi, bu projenin her yerde reddettiği şey.

| Aşama | Ne olur | Varsayılan |
|---|---|---|
| **Düz metin** | Okunuyor, dokunulmuyor | ilk 7 gün |
| **Arşiv** | Yerinde sıkıştırılır, **okunabilir kalır** | 7. günden sonra |
| **Silme** | Kategorisinin süresi dolunca | sıradan 14 gün, önemli 365 gün |

Ölçülen: **247 KB → 83 KB**, ve tek başına sıkıştırma testinde
**%6**. Yani bir yıllık güvenlik kaydı, beş haftalık sıkıştırılmamış
kadar yer tutuyor.

**Ayarlar geliştirici modunda** (`panel_settings` kayıtları):
`logs.retention_days`, `logs.important_retention_days`,
`logs.archive_after_days`, `logs.level`, `logs.verbose_until`.

**Bilerek yazılan kurallar:**

- **Yazılmakta olan gün asla ellenmez.** Dosyaları açık; açık bir dosyayı
  sıkıştırmak, sürecin hâlâ eklediği tanıtıcıyı budar.
- **Arşiv, saklamadan önce gelmeli.** `LogLifecycle`, arşivleme süresi
  saklama süresini geçerse onu kırpar — aksi hâlde bir gün, hiç
  sıkıştırılmadan silinir ve bu geri alınamaz.
- **Sıkıştırma geçici dosyaya yazılıp yeniden adlandırılır.** Orijinalin
  üzerine yazmak, yarıda kalan bir çökmede budanmış bir log bırakır; ve
  sessizce yarısı eksik bir log, büyük bir logdan kötüdür.
- **Tanınmayan dosya ve dizinlere dokunulmaz.** Bu paketin yazmadığı bir
  şey, başkasının koyduğu bir şeydir.

**Bakım açılışta bir kez çalışır**, arka planda zamanlayıcıyla değil:
silme ile yazma yarışmasın diye. Aylarca çalışan süreç için panelin
onarım operasyonu (§4, B grubu) var — görünmeyen bir zamanlayıcıdan
iyidir.

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
log seviyesi ve log saklama süresi; **`[campaign]`'in tamamı**
(`drop_params`, `extra_params`, `store_click_ids`).

`[campaign]` özellikle önemli, çünkü diğerlerinden farklı bir sebeple
oradadır: hukuki bir cevabın karşılığıdır. Müşterinin avukatı "utm_term
saklanmasın" dediğinde bunun bir SSH oturumu gerektirmesi, A5'in var
olma sebebinin ta kendisidir.

**Saklama modu değişimi geçmişe dönük değildir.** Bir ayarı kapatmak,
o ana kadar toplanmış veriyi silmez. Panel bunu açıkça söylemeli, ve
"artık toplamıyoruz" ile "hiç toplamadık" ayrımı (§D5) burada da
geçerli. Geçmişi de temizlemek gerekiyorsa bu ayrı bir onarım
operasyonudur (§4, B grubu), ayarın yan etkisi değil.

**Bitti ölçütü:** `collector.example.toml` sekiz anahtara inmiş; her
taşınan ayar için "dosyada yoksa veritabanından okunur" testi; geriye
dönük uyumluluk — eski dosyadaki değer varsa bir kez veritabanına
göç ettirilip dosyadan yok sayılır ve bu bir denetim kaydı üretir.

---

#### A6 — Servislerin ayarı canlı okuması ✅ **mekanizma yapıldı, kablolama sürüyor**

**Yapıldı:** `internal/settings` — servislerin `panel_settings`'i yoklayıp
canlı değer değiştirdiği kaynak. Beacon'da bağlı: `campaign.drop_params`,
`campaign.extra_params`, `campaign.store_click_ids`, `beacon.sites`.

**Aşılması gereken mimari engel vardı** ve kararı yazıya geçiriyorum:
her servisin veritabanı rolü kendi tablosuyla sınırlı — `beacon_writer`
yalnızca `INSERT ON beacon_events` yapabiliyordu, `panel_settings`'i
okuyamıyordu. Üç seçenek vardı:

| Seçenek | Neden seçilmedi / seçildi |
|---|---|
| **`GRANT SELECT ON panel_settings`** | **Seçildi.** Tek tablo, salt okuma, kişisel veri yok, kimlik bilgisi yok. Beacon'a analitiği, kullanıcıları, oturumları veya token'ları okuma yetkisi vermiyor. |
| HTTP üzerinden panelden çekmek | Tüm internetin zaten ulaşabildiği sürece yeni bir kimlik doğrulama yüzeyi eklerdi |
| Panelin anlık görüntü dosyası yazması | Karşılığı olmayan bir senkronizasyon sorunu getirirdi |

**Grant verilmezse her şey çalışmaya devam eder** — süreç yapılandırma
dosyasıyla yürür. Ayar okuması başarısızlığı bu yüzden ölümcül değil,
kayıtlı.

**Canlı uygulanabilirlik ayarın özelliği, tercih değil.** Kampanya
politikası saf bir fonksiyon, istekler arasında değiştirilebilir; tampon
boyutu ise kanal oluşturulurken sabitlenmiş bir kapasite. `Definition`
artık `Live` bayrağı taşıyor ve **panel bunu söylüyor** — yoksa müşteri
değeri değiştirir, hiçbir şey olmaz ve paneli bozuk sanır.

Panelin tanımladığı `Live` anahtarlar ile servislerin gerçekten okuduğu
anahtarların aynı olduğunu doğrulayan bir test var: birinde olup
diğerinde olmayan anahtar, ya hiçbir şey yapmayan bir ayar ya da kimsenin
değiştiremediği bir ayardır.

**Log seviyesi de canlı.** `slog.LevelVar` üzerinden: seviye her kayıtta
okunuyor, yani değişiklik bir sonraki satırda etki ediyor.
`logs.verbose_until` **kendi kendine sönüyor** — "debug aç, sorunu
tekrarla, kapat" bir destek çağrısının gerçekten uzandığı tek log ayarı
ve son adımı unutmak diskin dolma yoludur. Pencere ortasında yeniden
başlayan süreç ayrıntılı olarak geri gelir; bozuk bir zaman damgası
"açık değil" sayılır, çünkü hatalı bir değer bir kurulumu sonsuza kadar
debug'da tutmamalı.

**Kalan kablolama** (mekanik, A6-devam): collector limitleri, geo blok
listeleri, bot skor eşiği, flush aralığı.

##### Eski not



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

**Bitti ölçütü (A6):** veritabanı bağlantısı kesilirken servisin son değerle
çalışmaya devam ettiğini gösteren test; ayar değişiminin bir aralık
içinde etki ettiğini gösteren test; `-race` altında temiz.

---

#### A7 — IP saklama modu: tam veya maskeli

**Ne:** IP adresi KVKK/GDPR anlamında kişisel veridir ve bugün iki
tabloda da tam olarak saklanıyor. Bu bir ayar olur, iki değerli:

| Mod | Ne saklanır | Ne kaybedilir |
|---|---|---|
| `tam` | Bugünkü hâl | — |
| `maskeli` | IPv4 son oktet sıfırlanır, IPv6 /64'e kırpılır | Kesişim birleşimi zayıflar, "şu IP ne yaptı" görünümü anlamsızlaşır |

**Karar:** Müşteriye kurulum sırasında sorulur. "KVKK ile uğraşmak
istemiyorum, sorun çıkmasın" diyen müşteri için **tam maskeli** seçilir.
Hukuki metinleri gerçek hukukçular hazırlayacak; bizim işimiz teknik
karşılığını sunmak.

**Dikkat — bunun bir maliyeti var ve gizlenmemeli:** maskeli modda
`beacon_events.ip` ile `traffic_snapshots.ip` birleşimi zayıflar. O
birleşim projenin ayırt edici tezi (§0). Maskeli mod seçildiğinde
kesişim görünümleri "bu kurulumda IP maskeleme açık olduğu için sınırlı"
der — sıfır göstermez, gizlenmez (§D5 kuralı).

**Bitti ölçütü:** maskeleme yazma anında uygulanıyor (sonradan değil —
maskelenmemiş veri diske hiç değmiyor); mod değişimi geçmişe dönük
değil ve panel bunu açıkça söylüyor.

---

#### A8 — Zaman dilimi ⚠️ **planda hiç yoktu**

**Ne:** Bugün okuma API'sinde hiçbir zaman dilimi kavramı yok.
`time_bucket($6::interval, time)` çağrılarının hiçbiri zaman dilimi
almıyor — yani her gruplama UTC.

**Neden sorun:** İstanbul'daki bir mağaza sahibi "dün kaç ziyaretçi"
dediğinde UTC dünü değil, İstanbul dününü kastediyor. Üç saatlik kayma
şu anlama gelir: gece 00:00-03:00 arası trafik bir önceki güne yazılır,
"bugün" kartı yanlış, günlük grafiğin sınırları kayık, haftalık
karşılaştırma tutarsız.

**Yapılacak:** Site başına zaman dilimi ayarı (sihirbazda zaten
soruluyor, ama hiçbir yere bağlanmıyor). `time_bucket` çağrıları
zaman dilimi argümanı alır; gün sınırları o dilimde hesaplanır.

**Dosyalar:** `internal/api/store.go`, `store_beacon.go`, `params.go`

**Bitti ölçütü:** UTC+3'te 23:30'da gelen bir olayın "bugün"e
yazıldığını doğrulayan test; DST geçişi olan bir dilimle (örn.
Europe/Berlin) gün sınırı testi.

---

#### A9 — Ziyaretçi gizlilik kartı ve veri silme talebi ⚠️ **yeni özellik**

**Ne:** Müşterinin çerez/gizlilik sayfasına gömülen küçük bir JS kartı.
Sitenin **ziyaretçisi** kendisi hakkında ne tutulduğunu görür ve
"analitik verilerimi sil" diyebilir. Kart Crucible markası taşımaz —
sitenin kendi gizlilik sayfasının parçası gibi görünür.

**Kimlik iddia edilmez, türetilir.** "Asla taklit olmamalı" şartının
tamamının cevabı budur. Kart bir kimlik göndermez; sunucu, isteğin
kendisinden (IP + user agent + o günün tuzu) beacon'ın yazdığı
`visitor_id`'nin **aynısını** hesaplar. Başkasının verisini silmeyi
isteyemezsin, çünkü başkasının IP+UA'sını sunamazsın.

**Kapsam, dürüstçe.** Günlük tuz yalnız bellekte ve 24 saatte dönüyor.
Dünkü satırlar, artık var olmayan bir tuzdan türemiş farklı bir
`visitor_id` taşıyor — **onları bulamayız.** Bu bir eksiklik gibi
görünüyor; aslında tasarımın gizlilik özelliğinin ta kendisi ve kartta
böyle yazılmalı:

> Bugünkü kayıtlarınız silindi. Daha eski kayıtlar sizinle
> ilişkilendirilemiyor — bu teknik olarak imkânsız — ve N gün içinde
> kendiliğinden siliniyor.

Çoğu analitik sağlayıcısının söyleyemeyeceği bir cümle, ve reklam değil,
mimarinin sonucu.

**Gelecek için: localStorage opt-out.** Silme geçmişi kapatır, opt-out
geleceği kapatır. `beacon.js` localStorage'da bir bayrak arar; varsa hiç
göndermez. Sunucuda durum yok, kimlik yok, çerez yok — ve taklit
edilecek bir şey de yok, çünkü yalnızca kendi verini bastırabilir.

**Silme, beacon'a DELETE yetkisi vermeden.** Kritik kısım: beacon rolü
bugün yalnız `INSERT` yapabiliyor ve öyle kalmalı. DELETE vermek, ele
geçirilmiş bir beacon'ın tüm analitiği silebilmesi demektir. Bunun
yerine kuyruk:

1. Beacon, `beacon_erasure_requests` tablosuna bir satır **ekler** —
   zaten yapabildiği tek şey.
2. Satır, o an hesaplanmış `visitor_id`'yi ve talep zamanını taşır.
3. `DELETE` yetkisi olan ayrı bir süreç kuyruğu boşaltır.

**Beacon'ın yetkisi hiç genişlemez.** Talep "24 saat içinde işlenir" —
KVKK'nın 30 günlük yasal süresinin çok içinde.

**Token neden var:** kart, silme talebini tek bir kimliksiz POST olarak
göndermez. Önce kısa ömürlü bir onay token'ı alır (sunucuda yalnız
SHA-256'sı durur), ziyaretçi onaylar, talep token'la gelir. İki adımlı
olması CSRF'i ve kazara/otomatik tetiklenmeyi engeller.

**Açık uç bırakmama:** kart yalnız o siteyi ve yalnız türetilmiş kimliği
okur; varlık/sayım sızdırmaz (veri yoksa "bugün size ait kayıt yok" der,
bu da zaten kendisi hakkında bir bilgi); hız sınırlı; başka hiçbir
tabloya erişimi yok.

**Bitti ölçütü:** başka bir IP/UA'dan gelen isteğin farklı bir
`visitor_id` türettiğini ve dolayısıyla başkasının satırına
dokunamadığını gösteren test; beacon rolünün hâlâ `DELETE`
yapamadığını doğrulayan rol testi; token olmadan silme talebinin
reddedildiğini gösteren test.

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

#### B6 — Çok müşterili VDS'te yalıtım ⚠️ **belirtilmişti, planlanmamıştı**

**Şart, kullanıcının kendi sözleriyle:** "Tek VDS'te 3 farklı müşteri 3
farklı web sitesi olabilir ama hepsi ayrı kendi içinde olacak."

Bu bugün yalnızca `panel_site_members` ile kısmen sağlanıyor. Sızıntı
yüzeyi denetlenmedi. Denetlenmesi gerekenler:

- **Site seçici**, kullanıcının üye olmadığı sitelerin *varlığını* bile
  sızdırmamalı. "Site bulunamadı" ile "erişiminiz yok" aynı cevabı
  vermeli.
- **Denetim kaydı** site kapsamlı okunmalı; A sitesinin admini B
  sitesinin kayıtlarını görmemeli. `panel_audit_log`'ta hesap düzeyi
  eylemler (`site_id = ''`) ayrı ele alınmalı.
- **`panel_logs` en tehlikeli yer.** Log satırları paylaşılan bir
  süreçten geliyor ve serbest metin. Bir collector log satırı başka bir
  müşterinin alan adını, IP'sini ya da site kimliğini içerebilir. Log
  gösterimi site kimliğine göre filtrelenmeli **ve** filtrelenemeyen
  satırlar (süreç düzeyi) yalnız geliştirici moduna görünmeli.
- **Hata sayfaları ve yığın izleri** müşteriye asla ham gitmemeli.
- `overview` ucu birden çok siteyi tek istekte topluyor — token'ın
  kapsamıyla kesişimi test edilmeli.

**Bitti ölçütü:** her uç için "A sitesinin kullanıcısı B sitesinin
verisine ulaşamaz" testi; log filtresinin başka müşterinin alan adını
içeren satırı sızdırmadığını gösteren test.

---

#### B7 — Sürüm kavramı ⚠️ **hiç yoktu**

**Ne:** Projede hiçbir yerde sürüm numarası yok. `ApplyPendingMigrations`
hangi sürümden hangi sürüme gidildiğini bilmiyor; sağlık yoklaması
müşterinin hangi yapıda olduğunu söylemiyor.

**Neden gerekli:** On iki müşteriyi izlerken "hangisi hâlâ eski yapıda"
temel bir soru. Ayrıca bir hata raporunu doğru sürüme bağlamak
gerekiyor.

**Yapılacak:** derleme zamanı `-ldflags` ile gömülen sürüm + commit;
`crucible version`; sağlık sayfasında ve sağlık yoklaması cevabında
sürüm; `panel_operations`'ta operasyonun hangi sürümde çalıştığı.

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

#### C2.5 — Sihirbazın son adımı: panelden yapılamayacaklar ✅ **yapıldı**

**Ne:** Kurulum sihirbazının en sonunda, **panelin asla yapamayacağı**
işlerin listesi ve onları **gerçekten kontrol eden** bir doğrulayıcı.

**Neden liste değil de kontrol:** Kimsenin doğrulamadığı bir liste,
herkesin işaretlediği bir listedir. Değerin tamamı, "Kontrol et"e
basınca gerçek sorguların çalışması, gerçek dizinlerin okunması, gerçek
istek atılmasında — böylece "kuruldu" bir iddia değil, bir gözlem olur.

**On manuel adım**, her biri *panelin bunu neden yapamayacağını* söyler.
Gerekçe olmadan liste keyfî bir angarya listesi gibi okunur:

| Adım | Panel neden yapamaz | Kontrol eden |
|---|---|---|
| PostgreSQL + TimescaleDB kurulumu | Üzerinde çalıştığı veritabanını kuramaz | — |
| Dört rol ve yetkileri | **Bir rol kendine yetki veremez** — verebilseydi ayrımın anlamı kalmazdı | 3 kontrol |
| Şema dosyaları | Hiçbir servis DDL çalıştırmaz; bu bilinçli | 3 kontrol |
| Sekiz bootstrap anahtarı | Veritabanına nasıl ulaşılacağını veritabanına soramazsınız | — |
| Günlük dizini ve izinleri | Kendi yazacağı dizini oluşturamaz | ✔ |
| systemd unit'leri | **Panelin süreç başlatma yetkisi yoktur ve olmamalıdır** | 3 kontrol |
| TLS sertifikası | Dosya sisteminde, kök yetkisi ister | — |
| `/_ca/` yönlendirmesi | Sitenin web sunucusunda, bizde değil | — |
| Yedekleme | Bu sistem yedek almaz ve göremez | — |
| Disk planlaması | — | ✔ |

**On üç otomatik kontrol.** İkisi bilerek **negatif** ve bu ikisi
listenin en önemli satırları:

- `grants.panel_isolation` — panel rolü analitik tablolara **erişemiyor**
  mu? Panel analitiği salt okunur HTTP API üzerinden okur; birinin fazla
  yetki verdiği bir kurulum, o gün gelene kadar tamamen sağlıklı görünür.
- `grants.api_read_only` — API rolü gerçekten **yazamıyor** mu? Destek
  token'ının güvenliği tümüyle buna dayanıyor.

`schema.columns` de projenin **zaten bir kez yaşadığı** hatayı yakalıyor:
`CREATE TABLE IF NOT EXISTS` var olan tabloya hiçbir şey yapmaz, yani
şema dosyasına eklenen bir sütun ancak dosya yeniden çalıştırılırsa
mevcut kuruluma ulaşır.

**Bilerek yazılan kurallar:**

- **Yapılandırılmamış kontrol `skip` döner, `pass` değil.** "Baktık,
  iyiydi" ile "bakmadık" farklı olgular — projenin her yerde koruduğu
  ayrım.
- **Yalnız `required` başarısızlıklar devri engeller.** Bitirilemeyen
  sihirbaz, etrafından dolaşılan sihirbazdır.
- **Yedekleme uyarı döner, asla `pass`.** Kontrol edilemeyen bir şeye
  "geçti" demek yalan olurdu; "kaldı" demek ise kurulumcunun kendi
  araçlarıyla halletmiş olabileceği bir şey yüzünden devri engellerdi.
- **Log dizini okunmaz, gerçekten yazılır.** Mod doğru görünürken dosya
  sistemi salt okunur bağlanmış olabilir.
- **Kontrol edeni olmayan adımlar ayrı gösterilir.** Doğrulanmış ve
  doğrulanmamış maddeyi aynı gösteren bir liste, okuyucuya ikisine de
  güvenmemeyi öğretir.

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

#### C6 — Görünür kart seti, kurulum başına yapılandırılır ⚠️ **yeni**

**Karar (kullanıcı):** Müşterinin panelinde hangi kartların göründüğü
sabit değil, **ayardır** — geliştirici sihirbazında biz belirleriz.

Mantığı şu: sıradan, teknik olmayan bir kişi "DDoS sayısı", "bot parmak
izi" ne demek bilmez. Kuruluma başlarken **müşteriye ne istediğini
sorarız**, sihirbazı ona göre yaparız, istediklerini açarız. Müşteri
panelde yalnız onları görür. Sonradan geliştirici ayarlarını açıp
girerse, oradan kendisi de ekleyebilir — "İnsan", "Bot", "Tarayıcıyla
gezenler", "Tüm bağlantılar" gibi.

**Bu, plandaki iki eksene bir üçüncüsünü ekliyor** ve üçünü karıştırmamak
gerek:

| Eksen | Neyi belirler | Nerede ayarlanır |
|---|---|---|
| **Profil** | Ne *toplanır* | Ayarlar (Hafif / Dengeli / Tam) |
| **Kart seti** | Ne *gösterilir* (bu kurulumda) | Geliştirici sihirbazı, sonra geliştirici ayarları |
| **Geliştirici modu** | Teknik katmanlar *açık mı* | Kullanıcı başına anahtar |

**Bu, çözdüğü asıl sorun:** collector "ziyaretçi"si (ayrı IP) ile beacon
"ziyaretçi"si (HMAC kimlik) **farklı sayılar** ve sistematik olarak
farklı — aynı IP iki farklı tarayıcıyla gelirse collector 1, beacon 2
sayar; bot'lar beacon'da varsayılan olarak dışlanır, collector'da
sayılır. Panelde ikisini etiketsiz yan yana koymak, müşterinin güvenini
ilk bakışta kaybettirir. Kart setini yapılandırılabilir yapmak bunu
kökten çözer: varsayılan olarak müşteriye **tek bir anlaşılır sayı**
gösterilir, geri kalanı isteyene açılır.

**Bitti ölçütü:** kart seti `panel_settings`'te; sihirbaz onu yazıyor;
geliştirici ayarlarından değiştirilebiliyor; kapalı bir kartın verisi
API'den hiç istenmiyor (boşuna sorgu yok).

#### C7 — Boş durumlar, API kesintisi, ve e-posta yolu

Üç küçük ama gerçek boşluk:

**Boş durumlar.** Kurulumdan sonraki ilk saat, müşterinin ürünün
çalışıp çalışmadığına karar verdiği andır ve planda hiç yoktu. Kritik
ayrım (§D5'in aynı kuralı): **"snippet henüz hiç görülmedi"** ile
**"snippet çalışıyor, henüz ziyaretçi yok"** aynı şey değil ve ikisi de
"0" olarak çizilmemeli. Birincisi bir kurulum hatası, ikincisi normal
bir pazartesi sabahı.

**Okuma API'si düştüğünde.** Panelin tek sert bağımlılığı bu ve ne
göstereceği yazılmamıştı. Karar: panel çalışmaya devam eder (ayarlar,
üyeler, sağlık hepsi `panel_*` tablolarından okunur), yalnız analitik
kartları "veri kaynağına şu an ulaşılamıyor" der ve sağlık sayfasına
bağlantı verir. Sıfır göstermez.

**E-posta yolu — projede hiç yok** (`grep`: sıfır SMTP/mail). Ama iki
planlı özellik e-posta gerektiriyor: `SendOwnerPasswordReset` (22.
operasyon) bir bağlantı yolluyor, ve sahip sihirbazı "meslektaş davetini
öneriyor".

**Karar: e-posta zorunlu olmasın.** Gerekçe kullanıcının kendi
kısıtından geliyor — "kurulum ve çalıştırma yükünü azaltacak şekilde
olmalı, 'şurada şunu ayarla' istemiyorum". Bir SMTP sunucusu
yapılandırmak tam olarak o yük.

- **Varsayılan (e-postasız):** şifre sıfırlama bağlantısı operasyonu
  çalıştıran kişinin ekranında görünür, sahibe telefonla/WhatsApp'la
  iletilir. Davet bağlantısı, davet eden yöneticinin ekranında görünür,
  meslektaşına nasıl isterse öyle yollar.
- **İsteğe bağlı SMTP:** yapılandırılmışsa bağlantılar e-postayla gider.
  Ürün onsuz eksiksiz çalışır.

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

#### D7 — Arama motoru botları sayfası ⚠️ **fırsat, boşluk değil**

**Ne:** "Googlebot siteme ne sıklıkla geliyor, hangi sayfalara bakıyor,
ne zaman son geldi."

**Neden bu bizde özel:** Collector, JS çalıştırmayan istemcileri de
görüyor — Googlebot, Bingbot, YandexBot dâhil. Beacon tabanlı hiçbir
analitik aracı (Umami, Plausible, GA'nın kendisi) bunu **göremez**;
crawler JS çalıştırmaz. Search Console gecikmeli ve örneklenmiş
gösterir; biz gerçek zamanlı ve tam gösterebiliriz.

**Neden planda olmalıydı:** Kullanıcının destek token'ını isterken
verdiği ilk gerekçe "SEO çalışıyor mu" idi. O soruya doğrudan cevap
veren tek görünüm bu ve D2'nin listesinde yoktu. Elimizdeki verinin en
iyi ürün karşılığı.

**Not:** teknik olmayan müşteri için bile anlaşılır — "Google siteni
son 7 günde 340 kez ziyaret etti" cümlesi jargon değil.

#### D8 — Sahibe dönük uyarı şeridi

Planda B4 sağlık sayfası **geliştirici** tarafı için var. Sahibin,
geliştirici moduna hiç girmeden bir şeyin bozuk olduğunu öğrenmesi için
karşılığı yoktu.

Panelin üstünde, yalnız gerçekten bir şey varken görünen dar bir şerit:

> Beacon 3 gündür veri almıyor. → Ne yapmalı

Kapsam dışı olan **e-posta/webhook uyarısı** değil bu; müşteri zaten
paneldeyken görmesi gereken şey. Gürültü olmaması için katı kural:
yalnız eyleme dönüştürülebilir ve kesin durumlar (veri akışı durdu,
disk kritik, snippet hiç görülmedi). "Trafiğiniz düştü" gibi yorum
gerektiren hiçbir şey buraya girmez.

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

### F. Sonraya bırakılanlar (karar verildi: bu yayda değil)

Bunlar **kapsam dışı değil, ertelenmiş**. Fark önemli: §7'dekiler
yapılmayacak, buradakiler sonra yapılacak.

#### F1 — Yedekleme

**Neden gerekli:** §5 "yedekten dönme"yi SSH gerektiren işler arasında
sayıyor — ama projede **yedek alma hiç planlanmamış.** Geri yüklenecek
bir şey olmadan geri yükleme prosedürü anlamsız. Müşterinin analitik
geçmişi ürünün kendisi; tek diske emanet edilmiş durumda.

**Kapsam:** zamanlanmış `pg_dump`, belgelenmiş geri yükleme adımları,
panelde `ShowLastBackup()` göstergesi, sağlık sayfasında "son başarılı
yedek" satırı.

#### F2 — Kurulum betiği

Postgres + TimescaleDB, **dört veritabanı rolü ve GRANT'ları**, systemd
unit dosyaları, TLS. Tek betik. Rol ayrımı bu projenin güvenlik
temelinin yarısı ve şu an elle kuruluyor — yani yanlış kurulabilir.
Betik, 13. müşteriyi yarım günden on dakikaya indirir ve rol ayrımını
kurulumun garantisi hâline getirir.

#### F3 — Filo izleme paneli

Bizim tarafımızda: kurulum listesi, destek token'ları, yoklama
zamanlayıcısı, tek ekranda hepsinin sağlığı. Müşteri sayısı azken elle
de yoklanabilir; sayı artınca gerekli olur.

---

## 2.5 Ara iş (bu fazda yapıldı): sunucu otoritesi ve log ağacı

Plandaki A–E sırasına ait olmayan, ama ikisi de her maddeyi etkileyen iki
iş. Ara iş olarak adlandırıldı çünkü faz değiştirmiyorlar; altlarına
yazılacak her şeyin üzerinde durduğu zemini değiştiriyorlar.

### AI-1 — İstemciye asla güvenme, yalnız sunucuya güven

**Kural, tam hâliyle:**

> İstemciye güvenilmez. Yalnız sunucunun kendi vardığı sonuç sayılır.
> İstek "1+1=2" diyor olması bir şey ifade etmez — önemli olan sunucunun
> ne hesapladığıdır. **Her istek ve arkasındaki her kullanıcı potansiyel
> bir saldırgandır.**

Kod bu şekilde zaten çalışıyor, ve bu denetlendi:

| Yer | İstemci ne iddia ediyor | Sunucu ne yapıyor |
|---|---|---|
| `beacon/server.go` | `data-site` ile site kimliği | Yapılandırılmış beyaz listeye karşı kontrol eder |
| `beacon/clientip.go` | `X-Forwarded-For` ile kendi IP'si | Yalnız **doğrudan eş** güvenilir vekil listesindeyse okur |
| `api/auth.go` | `Authorization` ile token | SHA-256 ile karşılaştırır, düz metni hiç tutmaz |
| `panel/session.go` | Çerez ile kimlik | Kullanıcı satırını **her istekte** yeniden okur |

**Eksik olan doğrulama değil, kayıttı.** Bir karar yanlış gittiğinde —
Cloudflare arkasındaki müşterinin tüm ziyaretçileri tek IP göründüğünde,
kimsenin açıklayamadığı bir site kimliği geldiğinde, müşterinin itiraz
ettiği bir giriş reddedildiğinde — soru hep aynı: **istemci ne iddia
etti, sunucu ne sonuca vardı?** Yalnız sonucu taşıyan bir log satırı bunu
cevaplayamaz; yalnız iddiayı taşıyan ise daha kötüdür, çünkü iddiaya
inanılmış gibi okunur.

O yüzden güven kararları **iki yarısıyla birlikte** ve kendi dosyasında
kaydediliyor (`internal/logging/trust.go`): `claimed`, `verdict`,
`reason`, `peer`. Verdict kapalı bir küme (`accepted` / `rejected` /
`ignored` / `throttled`), böylece "dün kaç reddetme oldu" bir metin
araması değil bir sayım.

**Kimlik doğrulamada:** her deneme kaydedilir, başarılı olanlar dâhil.
Yalnız başarısızlıkları kaydeden bir dosya, 03:00'teki başarılı girişin
bir saat önce kırk kez başarısız olmuş bir adresten geldiğini
gösteremez — ki gerçek bir ele geçirmenin şekli tam olarak budur.

### AI-2 — Tek dizinde, modüler, ayrıntılı log ağacı

**Şekil:**

```
<dir>/<servis>/<YYYY-AA-GG>/<kategori>.log
```

Üç eksen, üçü de **yazarken değil okurken** önemli olduğu için seçildi:

- **Servis** — dört süreç bir makineyi paylaşıyor.
- **Gün** — saklama süresi "dosya yeniden yaz" değil "dizin sil" olur, ve
  "14'ünde ne oldu" bir dizin listesidir.
- **Kategori** — giriş denemeleri, alım reddetmeleri ve sıradan işleyiş
  farklı kişiler tarafından farklı zamanlarda okunur.

**Dokuz kategori:** `app`, `error`, `security`, `auth`, `access`,
`ingest`, `rejected`, `audit`, `query`. Kapalı bir sabit kümesi — değer
bir dosya adına dönüşüyor, ve çağıranın verdiği bir dosya adı bir dizin
geçişi (path traversal) ilkelidir.

**Kararlar ve gerekçeleri:**

- **WARN ve üstü `error.log`'a da yansıtılır.** "Bugün bir şey ters gitti
  mi" dokuz dosyada arama değil, tek dosya olmalı.
- **JSON satırları.** İnsan okuyabilir, panelin log görüntüleyicisi
  (B1) ikinci bir format öğrenmeden ayrıştırabilir.
- **Her değer temizlenir.** Log satırları kullanıcı kontrollü metin
  içerir; içindeki bir satır sonu kaydı ikiye böler ve ikinci yarısı —
  tamamen saldırganın seçtiği — kendi başına bir kayıt olarak
  ayrıştırılır. Bu **log enjeksiyonudur**: saldırganın, operatörün ne
  yaptığını öğrenmek için okuduğu dosyaya sahte kayıt yazması.
- **Sır gibi görünen her anahtar maskelenir.** Mekanizma bu değil —
  mekanizma sırrı loglamamak — ama bir log dosyasındaki şifre, çoğu
  hatanın olmadığı biçimde kalıcıdır: yedeklerde, panelin bize
  gönderdiğinde, operatörün terminal geçmişinde yaşar.
- **İzinler `0700` / `0600`.** Log satırları IP ve user agent taşır,
  yani analitik tablolarıyla aynı okumaya göre kişisel veridir.
- **Yazma hatası servisi durdurmaz.** Bir log dosyası açılamadığı için
  istek işleyemeyen servis, gözlemlenebilirliğini bir erişilebilirlik
  riskine çevirmiştir — tam tersi olmalı. Hatalar stderr'e düşer, servis
  devam eder.

**En yüksek değerli tek satır:** `security.log`'daki "forwarding header
ignored". Debug seviyesinde, çünkü doğru yapılandırılmış bir kurulumda
hiç çıkmaz ve yanlış yapılandırılmışta **her istekte** çıkar — ki
istenen teşhis sinyali tam olarak budur. "Neden tüm ziyaretçilerim aynı
IP'de" sorusunun cevabı bu satırdır, ve bugün bu cevabı bulmak SSH'te
log okumak demek.

**B1 ile ilişkisi:** dosya ağacı ve `panel_logs` tablosu birbirinin
alternatifi değil, aynı işin iki yarısı. Ağaç kaynaktır ve süreç
veritabanına ulaşamadığında bile yazar; tablo panelin gösterdiğidir.
B1 yazılırken ağaçtan beslenecek.

---

## 3. Kalıcı güvenlik kuralları

Bunlar madde değil, her maddeye uygulanan kısıtlar. **AI-1'deki
sunucu-otoritesi kuralı bunların birincisidir** ve aşağıdakilerin hepsi
onun özel hâlleridir.

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

## 6.1 Açık risklerin fazlara dağıtımı

Her fazın sonunda kalan riskler burada toplanıyor ve **hangi fazın işi
olduğu** yazılıyor. Bir riski "kalan" diye bırakmak, onu unutmakla aynı
şey değil — ama ancak sahibi belliyse.

**Bu fazda kapatıldı:**

| Risk | Nasıl |
|---|---|
| `syscall.Statfs` build kısıtı yok — **panel paketi Windows'ta derlenmiyordu** | Ölçüm `preflight_disk_linux.go` / `_other.go` olarak ayrıldı. Linux dışında dürüst "ölçülemedi" döner; derleme hatası da tahmin de değil |
| Disk kontrolü tek birime bakıyordu | Log ve veri **ayrı birimlerde olur** ve ilk dolan log birimidir. İkisi de ölçülüyor |
| Rol adı yazım hatası sessizce atlanıyordu | `grants.roles_exist` uyarıyor. Yanlış yazılmış rol, iki yalıtım kontrolünü **doğrulanmış gibi geçirirdi** — doğrulanmamış kontrolden kötü tek sonuç bu |
| `logs.level` okunuyor ama uygulanmıyordu | `slog.LevelVar` ile canlı. `logs.verbose_until` **kendi kendine sönüyor**, yeniden başlatma penceresini korur, bozuk zaman damgası "açık değil" sayılır |

**Sahibi belirlenen, sonraki fazlara dağıtılan:**

| Risk | Faz | Neden orada |
|---|---|---|
| Panelde "Kontrol et" düğmesi yok | **C1–C7** | `RunPreflight` çağrılabilir; HTTP yüzeyi C'nin işi |
| Doğrulanamayan 5 adım ayrı gösterilmeli | **C2.5** | `UncheckedSteps()` hazır; ayrı gösterme şablon işi |
| Sıkıştırılmış log paneldeki görüntüleyicide açılmalı | **B1** | `.log.gz` okuma, log görüntüleyicinin parçası |
| Bakım yalnız açılışta çalışıyor | **B3** | Uzun ömürlü süreç için onarım operasyonu; arka plan zamanlayıcısı bilerek reddedildi |
| Collector tarafı canlı ayarlar (limitler, geo, skor eşiği, flush) | **A6-devam** | Mekanizma hazır, kablolama mekanik |
| `GRANT SELECT` elle veriliyor | **F2** | Kurulum betiği; preflight zaten uyarıyor |
| Log hacmi ölçülmedi | **B4** | Sağlık sayfası zaten disk ve tablo boyutu gösterecek |
| Anahtar listesi iki yerde | — | Bağımlılık tek yöne baksın diye bilinçli; test uyuşmayı yakalıyor |

## 6.2 Kampanya fazından kalan riskler ve verilen kararlar

Kampanya çalışması beş risk bıraktı. İkisi kapatıldı, üçü **bilinçli
olarak açık bırakıldı** — çünkü kusur değil, ya karar ya ölçüm
bekliyorlar. Bir riski kapatmak, onu kodlamak demek değildir; neyin
kapatılmayacağına da karar vermek gerekir.

**Kapatıldı:**

| Risk | Neden bu fazda | Ne yapıldı |
|---|---|---|
| Parametre kaydırması sessizce yanlış cevap verebilir | Bu projenin **zaten bir kez ödediği** hata modu (kümülatif istek sayısı: aritmetik doğru, öncül yanlış, yalnız gerçek çalıştırma yakaladı). En riskli anı şimdi — 17 sorgu yeni numaralandı ve konvansiyon henüz alışkanlık değil. | İki koruma: kaynağı okuyup konvansiyonu doğrulayan yapısal test, ve 22 okuma metodunun tamamının filtreyi uyguladığını canlı veritabanında kanıtlayan davranışsal test. |
| `extra_params` büyük/küçük harf uyuşmazlığı | **Gerçek kusurdu**, risk değil. Sessizce hiçbir şey yapmıyordu — hata yok, log yok, veri yok. | Ekstralar yazıldığı harfle korunuyor; `drop_params` harf katlamaya devam ediyor (standart isimler zaten küçük harf ve bilinmeyen isim açılışta reddediliyor). |

**Bilinçli olarak açık:**

| Risk | Neden şimdi değil |
|---|---|
| `utm_term` varsayılan olarak açık | Bu bir **hukuki cevap bekliyor**, teknik bir eksik değil. Mekanizma yazıldı ve doğrulandı; avukatın cevabı gelmeden varsayılanı çevirmek, kararı onun yerine vermek olur. |
| Kısmi indeks kampanyasız sorguları hızlandırmıyor | **Ölçüm bekliyor.** Gerçek hacimde sorun olduğuna dair hiçbir veri yok, ve olmayan bir performans sorunu için indeks eklemek diskin bedelini kesin, faydasını varsayımsal yapar. |
| Filtre "kampanyası olmayan"ı seçemiyor | Sorgu dizesinde üç durum (ayarlanmamış / boş / değer) iki duruma sığmıyor, ve cevap kırılımın kendi boş grubundan **zaten alınabiliyor**. Gerçek bir kayıp değil; sınır koda yorum olarak yazıldı. |

## 6.5 Hâlâ karara bağlanmamış tek şey: "site" tam olarak nedir

`site_id` bugün collector'ın yapılandırma dosyasındaki bir dize. Ama
gerçek bir mağaza `example.com`, `www.example.com` ve
`shop.example.com` üzerinden gelebilir; bazıları `.com` ve `.com.tr`
ikizi tutar.

**Öneri:** `site_id` birim olarak kalır; beacon'ın site beyaz listesi
`site_id` başına bir **alan adı listesi** kabul eder. Böylece üç alan
adı tek siteye yazar ve panelde tek sayı görünür. Ayrı görmek isteyen
ayrı `site_id` verir.

Bu, bugünkü şemayı hiç değiştirmiyor — yalnız beyaz listenin şeklini
değiştiriyor (`sites = ["mysite"]` yerine `site_id → [alan adları]`
eşlemesi). A5'te beyaz liste zaten veritabanına taşınıyor, dolayısıyla
maliyeti neredeyse sıfır. Onaylanırsa A5'in içine girer.

---

## 7. Kapsam dışı — varsayılmasın diye

Buradakiler **yapılmayacak**. Ertelenenler için §F'ye bak — ikisi farklı
şey.

- **Mobil uygulama.**
- **Son müşteriye otomatik uyarı** (e-posta, webhook). Sağlık yoklaması
  bizim tarafımızda. Not: §D8'deki panel içi uyarı şeridi bunun yerine
  geçmiyor, farklı bir şey — müşteri zaten paneldeyken gördüğü.
- **Faturalama / abonelik.** Tek VDS'te üç müşteri olabilir ama
  müşterinin ödeme durumu bu sistemin bileceği bir şey değil.
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
A1 panel_settings ─┬─> A2 profiller ───────────────┐
                   ├─> A4 saklama süresi ──────────┤
                   ├─> A7 IP modu (tam/maskeli) ───┤
                   ├─> A8 zaman dilimi ────────────┤
                   └─> A5 TOML→DB göçü ─> A6 canlı okuma
                                                    │
A3 yalnız-ülke modu ────────────────────────────────┤
A9 gizlilik kartı (beacon+kuyruk, bağımsız) ────────┤
                                                    ▼
                               B1 panel_logs ─> B2 operasyon günlüğü
                                                    │
                        ┌───────────────────────────┤
                        ├─> B3 39 operasyon         │
                        ├─> B4 sağlık sayfası       │
                        ├─> B5 destek token'ı       │
                        ├─> B6 çok müşterili yalıtım│
                        └─> B7 sürüm ───────────────┤
                                                    ▼
              C1 şablonlar ─> C2 gel. sihirbazı ─> C3 sahip sihirbazı
                           ├> C4 giriş/üye ─> C5 onay ekranı
                           ├> C6 kart seti (C2'de yazılır)
                           └> C7 boş durum / API kesintisi / davet bağlantısı
                                                    │
                                                    ▼
              D1 site seçici ─> D2 detaylar ─┬─> D3 gel. katmanları
                                             ├─> D4 ayar + akan pencere
                                             ├─> D7 arama motoru botları
                                             └─> D8 sahibe uyarı şeridi
                                                    │
                                                    ▼
                                E1 tek binary ─> E2 Sprintf testi ─> E3 README
                                                    │
                                                    ▼
                              F1 yedekleme ─ F2 kurulum betiği ─ F3 filo
```

**En kritik tek bağımlılık A5.** Yapılmazsa B3'ün değiştirecek bir şeyi
yoktur ve tüm "SSH'siz onarım" iddiası çöker.

**A9 bağımsız yürüyebilir.** Ziyaretçi gizlilik kartı beacon tarafında
duruyor ve panelin hiçbir parçasını beklemiyor; sıraya bakmadan
yapılabilir.

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

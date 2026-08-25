# Kurulum Kılavuzu

Sıfırdan çalışan bir kuruluma kadar, sırayla. Bu belge **kurulumu yapan
geliştirici** için yazıldı — müşteri için değil.

Buradaki her komut depodaki gerçek bayrak ve dosyalardan alınmıştır.
Uydurma yok; bir şey yapılamıyorsa "yapılamıyor" yazıyor.

---

## 0. Önce şunu bilin

**Bu yazılım kendi kendini kurmaz ve bu bilinçli bir karar.** Şema
uygulamaz, veritabanı rolü oluşturmaz, kendine yetki vermez. Bir rol
kendine yetki verebilseydi rol ayrımının hiçbir anlamı kalmazdı — ve rol
ayrımı bu sistemin güvenlik temelinin yarısı.

Bu yüzden aşağıda elle yapılacak adımlar var. Panel bunları **kontrol
eder**, yapmaz: kurulum sihirbazının son adımı gerçek sorgular atıp ne
bulduğunu söyler, ve eksik olan her satırın yanına çalıştırılacak komutu
yazar.

**Ne kadar sürer:** ilk kurulum, elinizde bir sunucu ve bir veritabanı
varsa, 30–45 dakika. Çoğu zaman veritabanı rollerinde geçer.

---

## 1. Ne kuruyorsunuz

Beş süreç, **tek veritabanı**, dört ayrı veritabanı rolü:

```
                    ziyaretçi
                        |
                        v
        +---------------------------------+
        |  collector  (:8443)             |  TLS/TCP vekil
        |  her bağlantıyı görür           |  JA4, bot skoru
        +---------------------------------+
                        |                        müşterinin sitesi
                        +----------------------> (:8080 arka uç)
                        |
   sitedeki snippet     |
        |               |
        v               v
   +---------+    +-----------------+
   | beacon  |    |  PostgreSQL     |
   | (:8081) +--->|  + TimescaleDB  |
   +---------+    +--------+--------+
                           ^
                           | yalnız SELECT
                  +--------+---------+
                  | analytics-api    |  (:8080, salt okunur)
                  | (:8080)          |
                  +--------+---------+
                           ^ HTTP + Bearer
                           |
                     +-----+------+
                     |   panel    |  (:8090) müşteri buraya giriyor
                     +------------+
```

**Neden beş süreç:** panelin veritabanı rolü analitik tablolarına
**hiç** erişemez. Sayıları HTTP üzerinden salt okunur API'den alır —
harici bir panelin alacağı yoldan. İnternetin tamamının ulaşabildiği
bileşenin aynı zamanda en geniş veritabanı yetkisine sahip olmaması
için.

**İki veri kaynağı, biri diğerini kapatmaz:**

| | collector | beacon |
|---|---|---|
| Neyi görür | **Her bağlantıyı** | Yalnız JavaScript çalıştıran ziyaretçileri |
| Neyi göremez | URL, başlık, referrer | Bot trafiğinin çoğunu |
| Zorunlu mu | Hayır | Hayır |

İkisini birden çalıştırmak zorunda değilsiniz. Yalnız beacon kurarsanız
sıradan bir analitik aracınız olur; yalnız collector kurarsanız hiçbir
JavaScript aracının göremediği trafiği görürsünüz. İkisi birlikte
çalıştığında panel her ikisini de gösterir ve **hangi kartın hangi
kaynaktan geldiğini söyler**.

---

## 2. Ön gereksinimler

| Gereken | Sürüm | Not |
|---|---|---|
| Go | **1.25.0+** | `go.mod`'daki sürüm. Eski bir araç zinciri ileride bir özellikte patlamaz, modülü baştan reddeder. Sunucuda gerekmez — başka yerde derleyip binary'yi kopyalayabilirsiniz. |
| PostgreSQL | **16.6 ile test edildi** | Aşağıya bakın. |
| TimescaleDB | **2.17.2 ile test edildi** | Saklama politikaları ve sıkıştırma bunu ister. |
| `psql` | — | Şemaları uygulamak için. |

**Sürümler hakkında dürüst olalım:** `docker-compose.yml`
`timescale/timescaledb:2.17.2-pg16` imajını sabitliyor; o imajın
çalışırken bildirdiği sürümler PostgreSQL **16.6** ve TimescaleDB
**2.17.2**, ve bütün entegrasyon testleri gerçekten bunun karşısında
koşuyor. Daha eskisi çalışmaz demiyorum — **denenmedi** diyorum. Elinizde
eski bir sunucu varsa şemaları uygulayıp `go test -tags integration ./...`
çalıştırmak, tahmin etmekten ucuz.

**İsteğe bağlı, yalnız geliştirme için:** `node` + `playwright` +
chromium (tarayıcı testleri), `docker` (yerel veritabanı).

Sunucuda **hiçbir JavaScript çalışma zamanı, npm, derleme adımı
gerekmez.** Panelin tarayıcıya gönderdiği her şey binary'nin içinde
gömülü.

---

## 3. Derleme

```bash
git clone https://github.com/cruciblelab/crucible-analytic.git
cd crucible-analytic

VERSION=$(git describe --tags --always)
for b in collector beacon analytics-api panel devpass; do
  go build -ldflags "-X main.version=$VERSION" -o "bin/$b" "./cmd/$b"
done
```

**Öneri: sunucuda derlemeyin.** Başka bir makinede derleyip yalnız
`bin/` dizinini kopyalayın. Sunucuda Go bulunmaması, sunucuda
derleyicinin bulunmamasıdır — küçük ama bedava bir kazanç.

Çapraz derleme çalışıyor ve test ediliyor:

```bash
GOOS=linux GOARCH=arm64 go build ./...
```

---

## 4. Veritabanı

### 4.1 Tek veritabanı — bu bir tercih değil, gereklilik

**Beş sürecin hepsi aynı veritabanına bağlanır.** Rolleri farklıdır,
veritabanı aynıdır.

Bunun sebebi kurulum sihirbazının rol yalıtımı kontrolü: panelin
bağlantısı üzerinden `has_table_privilege(...)` sorguluyor. Panel ayrı
bir veritabanındaysa o sorgu **hata verir**, kontrol "bakamadım" der, ve
"bakamadım" devir teslimi bloke eder. Yani panel ayrı bir veritabanına
kurulursa müşteriye devretme adımı **kalıcı olarak** çalışmaz.

Doğrulanmış davranış:

```
analytics veritabanında:        SELECT has_table_privilege(...)  -> t
başka bir veritabanında:        ERROR: relation "traffic_snapshots" does not exist
```

### 4.2 Veritabanını ve rolleri oluşturun

```sql
-- Veritabanı
CREATE DATABASE analytics;
\c analytics
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- Dört rol. Parolaları değiştirin.
CREATE ROLE collector         LOGIN PASSWORD 'degistirin';
CREATE ROLE beacon_writer     LOGIN PASSWORD 'degistirin';
CREATE ROLE analytics_reader  LOGIN PASSWORD 'degistirin';
CREATE ROLE panel_user        LOGIN PASSWORD 'degistirin';

GRANT CONNECT ON DATABASE analytics
  TO collector, beacon_writer, analytics_reader, panel_user;
GRANT USAGE ON SCHEMA public
  TO collector, beacon_writer, analytics_reader, panel_user;
```

### 4.3 Şemaları uygulayın

**Sıra önemli değil, ama hepsi gerekli.** Her biri elle uygulanır;
hiçbir servis DDL çalıştırmaz.

```bash
DSN="postgres://postgres@localhost:5432/analytics"

psql "$DSN" -f internal/storage/schema.sql     # traffic_snapshots
psql "$DSN" -f internal/beacon/schema.sql      # beacon_events
psql "$DSN" -f internal/panel/schema.sql       # panel_* tabloları
psql "$DSN" -f internal/asnlookup/schema.sql   # yalnız asn_lookup açacaksanız
```

`asnlookup` şemasını **yalnız** `asn_lookup.enabled = true` yapacaksanız
uygulayın. Kapalıyken o tablolara hiç dokunulmaz.

### 4.4 Yetkiler

```sql
-- collector: kendi tablosuna yazar
GRANT SELECT, INSERT ON traffic_snapshots TO collector;

-- beacon: yalnız kendi tablosuna yazar
GRANT INSERT ON beacon_events TO beacon_writer;

-- API: hiçbir şey yazamaz. HER İKİ tabloya da SELECT verin.
GRANT SELECT ON traffic_snapshots, beacon_events TO analytics_reader;

-- panel: yalnız kendi tablolarına. Analitik tablolarına ASLA.
GRANT SELECT, INSERT, UPDATE, DELETE ON
  panel_users, panel_sessions, panel_site_members,
  panel_settings, panel_api_tokens, panel_dev_access,
  panel_owner_claims, panel_login_attempts
  TO panel_user;

-- Denetim kaydı yalnız eklenebilir. UPDATE/DELETE bilerek yok:
-- ele geçirilmiş bir panel süreci ne yaptığını silemesin diye.
GRANT SELECT, INSERT ON panel_audit_log TO panel_user;

-- Dizi (sequence) yetkileri. Yalnız panel_user'a, ve tek tek adlarıyla.
--
-- Bu veritabanındaki BÜTÜN diziler panelin: traffic_snapshots ve
-- beacon_events'te BIGSERIAL yok, dolayısıyla collector ve
-- beacon_writer'ın hiçbir diziye ihtiyacı yok. "ALL SEQUENCES IN SCHEMA
-- public" yazmak onlara panelin dizileri üzerinde yetki verirdi —
-- karşılığında hiçbir şey vermeyen bir genişleme.
--
-- Adları tek tek yazmanın sebebi aynı: "ALL", bugün doğru olsa bile,
-- yarın public'e dizi ekleyen herkesi kapsar.
GRANT USAGE, SELECT ON
  panel_users_id_seq, panel_audit_log_id_seq, panel_api_tokens_id_seq,
  panel_dev_access_id_seq, panel_owner_claims_id_seq,
  panel_login_attempts_id_seq
  TO panel_user;

-- Canlı ayar okuma. İSTEĞE BAĞLI ama şiddetle önerilir:
-- bu grant olmadan collector ve beacon yalnız kendi dosyalarını okur,
-- ve panelden yapılan hiçbir ayar değişikliği onlara ulaşmaz.
GRANT SELECT ON panel_settings TO collector, beacon_writer;
```

**API'ye iki tabloyu da vermeyi unutmayın.** Yalnız `traffic_snapshots`
verirseniz `/beacon/` ve `/crossover/` uçları 500 döner, geri kalan her
şey çalışır — hata ayıklaması can sıkıcı bir arıza.

### 4.5 Panelin analitiği okuyamadığını doğrulayın

```sql
SELECT has_table_privilege('panel_user', 'traffic_snapshots', 'SELECT');  -- f olmalı
SELECT has_table_privilege('panel_user', 'beacon_events',     'SELECT');  -- f olmalı
SELECT has_table_privilege('analytics_reader', 'traffic_snapshots', 'INSERT');  -- f olmalı
```

Üçü de `f` dönmüyorsa kurulum sihirbazının kontrol adımı zaten size
söyleyecek — ama şimdi öğrenmek daha ucuz.

**Tablonun tamamını bir kerede görmek isterseniz** — bu sorgu §4.4'ün
gerçekten ne ürettiğini basar, ve kurulumu bitirmeden önce bakılacak tek
şeydir:

```sql
SELECT r.rolname AS rol, t.tbl,
       has_table_privilege(r.rolname, t.tbl, 'SELECT') AS sel,
       has_table_privilege(r.rolname, t.tbl, 'INSERT') AS ins,
       has_table_privilege(r.rolname, t.tbl, 'UPDATE') AS upd,
       has_table_privilege(r.rolname, t.tbl, 'DELETE') AS del
FROM (VALUES ('collector'),('beacon_writer'),
             ('analytics_reader'),('panel_user')) r(rolname)
CROSS JOIN (VALUES ('traffic_snapshots'),('beacon_events'),
                   ('panel_users'),('panel_audit_log'),
                   ('panel_settings')) t(tbl)
ORDER BY 1, 2;
```

Beklenen tablo — §4.4 doğru uygulandıysa çıkacak olan budur:

| rol | tablo | sel | ins | upd | del |
|---|---|---|---|---|---|
| analytics_reader | beacon_events | t | f | f | f |
| analytics_reader | traffic_snapshots | t | f | f | f |
| analytics_reader | panel_* | f | f | f | f |
| beacon_writer | beacon_events | **f** | t | f | f |
| beacon_writer | panel_settings | t | f | f | f |
| beacon_writer | traffic_snapshots | f | f | f | f |
| collector | traffic_snapshots | t | t | f | f |
| collector | panel_settings | t | f | f | f |
| collector | beacon_events | f | f | f | f |
| panel_user | panel_audit_log | t | t | **f** | **f** |
| panel_user | panel_users, panel_settings | t | t | t | t |
| panel_user | traffic_snapshots, beacon_events | **f** | **f** | **f** | **f** |

Kalın yazılanlar tesadüf değil, tasarım:

- **beacon kendi yazdığını okuyamaz.** Yazmak için okumaya ihtiyacı yok;
  okuyabilseydi olay tablosunun tamamı, ziyaretçilerden veri alan
  sürecin erişiminde olurdu.
- **Denetim kaydı `UPDATE` ve `DELETE` almıyor.** Ele geçirilmiş bir
  panel süreci ne yaptığını silemesin diye.
- **Panel hiçbir analitik satırını göremiyor.** Bütün sistemin dayandığı
  satır bu; §4.1'in sebebi de bu.

Bu blok gerçek bir TimescaleDB'ye (16.6 / 2.17.2) uygulanarak
doğrulandı, çıkan matris yukarıdaki tablodur.

---

## 5. Sırlar

Üçünü de kuruluma başlamadan üretin.

### 5.1 Geliştirici şifresi

Hukuki ağırlığı olan ayarları (IP saklama modu, log saklama süresi)
korur. **Her seferinde sorulur**, oturum tutmaz.

*Analitik saklama süresi bu listede değil: o ayar panelde hiç yok,
yalnız yapılandırma dosyasından değişiyor — bkz. §12.*

```bash
./bin/devpass
```

Şifreyi kendisi değil, yalnız `argon2id` hash'ini `panel.toml`'a
yazarsınız. Şifreyi kaybederseniz yenisini üretip satırı
değiştirirsiniz — hash'ten geri döndürülemez, zaten amacı budur.

**Öneri:** bu şifreyi müşteriye vermeyin. Onu siz tutarsınız; müşteri
ayarı görür, kilitli olduğunu ve neden kilitli olduğunu okur, ve
değiştirmek istediğinde size ulaşır. Bu, hukuki sorumluluğun kimde
olduğunu belirsiz bırakmamak içindir.

### 5.2 IP jeton anahtarı

Yalnız `privacy.ip_storage = "full"` kullanacaksanız gerekir.

```bash
./bin/devpass -ipkey
```

**Bu anahtar collector ve beacon yapılandırmalarında birebir aynı
olmalı.** Farklıysa iki veri kaynağı arasındaki kesişim sorgusu hiçbir
şey bulmaz ve **bunu söyleyen bir hata mesajı olmaz.** Kurulum kontrolü
anahtarın varlığını görebiliyor, aynılığını göremiyor — bilinen sınır.

### 5.3 API jetonu

```bash
./bin/analytics-api -hash-token
```

Jetonun kendisini `panel.toml`'a, **hash'ini** `analytics-api.toml`'a
yazarsınız. Panelin jetonu `sites = ["*"]` olmalı — panel bütün siteleri
sunuyor.

**Sonucunu bilerek yazıyorum:** panelin jetonu her siteyi okuyabildiği
için, bir müşterinin sayılarını diğerinden ayıran tek şey panelin kendi
yetki kontrolü. Müşteriye jeton verecekseniz **asla** `["*"]` vermeyin;
yalnız kendi sitesini listeleyin.

---

## 6. Yapılandırma dosyaları

Dört örnek dosya var; kopyalayıp düzenleyin:

```bash
cp config.example.toml        /etc/crucible/collector.toml
cp beacon.example.toml        /etc/crucible/beacon.toml
cp analytics-api.example.toml /etc/crucible/analytics-api.toml
cp panel.example.toml         /etc/crucible/panel.toml

chmod 600 /etc/crucible/*.toml
chown crucible: /etc/crucible/*.toml
```

**Dosya izinleri gerçekten önemli:** bu dosyalar veritabanı parolasını,
IP hash anahtarını ve geliştirici şifresinin hash'ini taşır.

### Önce site kimliğine karar verin — bu geri alınamaz

`site.com` ile `blog.site.com`, iki snippet'e **aynı** site kimliğini
yazarsanız tek sitedir; **farklı** yazarsanız iki ayrı sitedir. Ürün
sizin yerinize karar vermiyor, ve bu kurulumun sonradan
düzeltilemeyecek **tek** kararı.

Sebebi ziyaretçi kimliğinin üretimi:

```
visitor_id = HMAC(günlük_tuz, site_id ‖ ip ‖ user_agent)
```

Site kimliği hash'in **içinde**. İki ayrı kimlikle toplanan veride
ikisini de gezen aynı kişi, kayıtlarda kalıcı olarak iki ziyaretçidir.
Sonradan birleştirecek ortak bir kimlik yok: tuz döndü, hash tersine
çevrilmiyor.

| Sonradan toplanabilir | Sonradan toplanamaz |
|---|---|
| Sayfa görüntüleme, oturum, özel olay — hepsi olay sayısı | Tekil ziyaretçi — kişi sayısı |

Yani alt alan adlarını ayıran bir kurulum, sonradan "sitemizin ziyaretçi
sayısı" için tek bir sayı isterse eline geçen "her birinin ziyaretçisi,
toplanmış" olur; bu daha büyük bir sayıdır ve aynı sorunun cevabı
değildir.

**Kimsenin özel bir talebi yoksa:** tüm mülk için **tek site kimliği**
kullanın, alt alan adlarını sayfa kırılımından görün. Bu yön geri
alınabilir; diğeri değil.

### Her dosyada mutlaka değiştirilecekler

| Dosya | Alan | Not |
|---|---|---|
| `collector.toml` | `site_id` | Zorunlu. `[a-zA-Z0-9_-]{1,64}` |
| | `network.backend_addr` | Trafiğin gideceği gerçek yer |
| | `storage.timescale_dsn` | `collector` rolüyle |
| `beacon.toml` | `sites` | Kabul edilen site kimlikleri — **zorunlu**, snippet herkese açık |
| | `timescale_dsn` | `beacon_writer` rolüyle |
| | `trusted_proxies` | Aşağıya bakın — **en pahalı yanlış yapılandırma** |
| `analytics-api.toml` | `timescale_dsn` | `analytics_reader` rolüyle |
| | `[[tokens]]` | Hash'ler |
| `panel.toml` | `panel_dsn` | `panel_user` rolüyle, **analitikle aynı veritabanı** |
| | `analytics_api_url`, `analytics_api_token` | |
| | `timezone` | Müşterinin saat dilimi |
| | `beacon_url` | Snippet'i yazdırmak için |
| | `[roles]` | Dört rol adı — **boş bırakırsanız devir teslim bloke olur** |
| | `[developer_gate] password_hash` | `devpass` çıktısı |

### `trusted_proxies` — kataloğun en üst maddesi

Beacon bir ters vekilin arkasındaysa (Cloudflare, nginx, herhangi
biri), o vekilin ağlarını buraya yazmalısınız:

```toml
trusted_proxies = ["173.245.48.0/20", "2400:cb00::/32"]
```

**Boş bırakırsanız ne olur:** her ziyaretçi vekilin adresi olarak
görünür. Bu yalnız adresi kaybetmez — **ziyaretçi sayısı, coğrafya ve
iki veri kaynağı arasındaki kesişim, hepsi aynı anda yanlış olur.** En
sık yapılan gerçek yapılandırma hatası budur.

**Ama fazla geniş de yazmayın.** `0.0.0.0/0` yazarsanız her ziyaretçi
kendi IP'sini uydurabilir.

Bu ayar artık **panelden de değiştirilebilir** (A5.1) — dosya yalnız
geri düşüş katmanı.

---

## 7. Servisleri çalıştırma

Depoda systemd unit dosyası **yok**. Aşağıdaki şablon çalışan bir
örnektir; deponun parçası değildir, yani kendi ihtiyacınıza göre
düzenlemeniz beklenir.

```ini
# /etc/systemd/system/crucible-panel.service
[Unit]
Description=Crucible Analytic paneli
After=network.target postgresql.service

[Service]
Type=simple
User=crucible
Group=crucible
ExecStart=/opt/crucible/bin/panel -config /etc/crucible/panel.toml
Restart=on-failure
RestartSec=5s

# Sertleştirme
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/crucible /var/lib/crucible

[Install]
WantedBy=multi-user.target
```

Diğer dördü için aynı şablon; yalnız `Description`, `ExecStart` ve
`-config` değişir.

**Collector'ın farkı:** 443'ü dinleyecekse ya `CAP_NET_BIND_SERVICE`
verin ya da yüksek bir porta bağlayıp önüne vekil koyun.

```ini
AmbientCapabilities=CAP_NET_BIND_SERVICE
```

### Log dizini

```bash
mkdir -p /var/log/crucible
chown crucible: /var/log/crucible
chmod 700 /var/log/crucible
```

**700 gerçekten gerekli.** Log satırları adres ve tarayıcı bilgisi
taşır; bu kişisel veridir. Kurulum kontrolü izin biti yanlışsa uyarır.

---

## 8. Ters vekil ve TLS

Panel varsayılan olarak `127.0.0.1:8090` dinler. **Bu bilinçli:** bir
ters vekilin arkasında çalışması bekleniyor.

```nginx
server {
    listen 443 ssl http2;
    server_name panel.example.com;

    ssl_certificate     /etc/letsencrypt/live/panel.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/panel.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8090;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

**`analytics-api`'yi internete açmayın.** Sadece panelin ulaşması
yeterli; `127.0.0.1` üzerinde bırakın.

`secure_cookies` varsayılan olarak açık. Yalnız düz HTTP üzerinden
`localhost`'ta geliştirme yaparken kapatın — üretimde asla.

---

## 9. İlk çalıştırma: kurulumdan devir teslime

Bu, sıradaki tek yoldur. Adımları atlamaya çalışmayın; panel zaten
bırakmaz.

### 9.1 Paneli başlatın

```bash
systemctl start crucible-panel
```

Panel şunlardan biri eksikse **başlamaz** ve nedenini stderr'e *ve* log
ağacına yazar: ayrıştırılamayan şablon, tanımsız metin anahtarı,
ulaşılamayan veritabanı, bilinmeyen saat dilimi.

### 9.2 Geliştirici bağlantısı üretin

Hiç hesap yokken panelin ön sayfası giriş formu değil, "kurulum
bekleniyor" sayfasıdır ve size bu komutu söyler:

```bash
./bin/panel -config /etc/crucible/panel.toml -dev-link
```

Ekrana **bir kez** bir bağlantı yazar. Saklanan yalnız SHA-256'sı; o
bağlantıyı bir daha göremezsiniz.

Bu bağlantı:

- tek kullanımlıktır,
- süresi doludur (varsayılan 2 saat),
- kullanıldığı adres denetim kaydına geçer,
- ve **ilk hesap oluşturulduğu anda otomatik onayı biter.** Sonrasında
  aynı komut çalışır ama bağlantı, sahip panelden onaylayana kadar
  çalışmaz.

### 9.3 Sihirbazı yürütün

Bağlantı sizi sekiz adımlı sihirbaza götürür:

| # | Adım | Ne yapar |
|---|---|---|
| 1 | Başlangıç | Kapsamı anlatır |
| 2 | Veritabanı ve şema | **Hiçbir şey değiştirmez.** Gerçek sorgular atar, bulduğunu söyler |
| 3 | Siteler | Beacon'ın kabul edeceği site kimlikleri — **yazar** |
| 4 | Müşteri ne görecek | Panoda hangi blokların görüneceği — **yazar**, aşağıya bakın |
| 5 | Yapılandırma dosyaları | Dosyalardaki değerleri **gösterir**, değiştiremez |
| 6 | Saklama süreleri | Saklama sürelerini yazar — **geliştirici şifresini her seferinde sorar** |
| 7 | Kontrol | Kontrolleri düğmeye basınca **gerçekten çalıştırır** |
| 8 | Devir teslim | §9.4 |

**4. adım sihirbazdaki tek teknik olmayan soru, ve onu müşteriye
sormanız için var.** Siteyi yaptıran kişi bot, parmak izi, DDoS gibi
şeyleri bilmek zorunda değil; panelinde okuyamayacağı bir sayı,
olmayan bir sayıdan kötüdür — çünkü yanlış bir sonuca davet eder.
Yanına oturun, ne görmek istediğini sorun, yalnız onları işaretleyin.

Üç şey bilerek böyle:

- **İşaretlenmeyen blok yalnız gizlenmiyor, sorgusu hiç atılmıyor.**
  Ölçüldü: sayfalar kırılımı 3,3 ms, ülkeler 2,0 ms. İki kart ve bir
  tablo isteyen bir müşteri, her sayfa açılışında beş sorgudan
  kurtuluyor. Yalnız collector kartı seçilmişse beacon özeti bile
  çekilmiyor.
- **Hiç dokunmazsanız varsayılan altı kart ve altı kırılım gösterilir.**
  Form zaten o hâlde açılıyor, yani adımı geçmek bir şeyi kapatmaz.
- **Hepsini kaldırmak "hiçbiri" demektir** ve bu bilerek söylenebilir:
  yalnız collector çalıştıran bir kurulumda beacon kırılımları
  kapatılmazsa müşteri "snippet kurulmamış" diyen altı tablo görür.

Sonradan değiştirmek için bu adıma geri dönebilirsiniz.

**Her adım değiştirdiğini anında kaydeder.** Yarıda bırakırsanız yarım
bir kurulum kalır — saklanan bir taslak değil.

7. adım **14 kontrol** çalıştırır. Kontrol listesi eksik olan her satırın
yanına çalıştırılacak komutu yazar.

**Bu 14'ün içinde "servisler ayakta mı" yok.** Kontrol kodu yazılmış ve
test edilmiş durumda, ama panel binary'si ona hiçbir adres vermiyor —
`panel.toml`'da böyle bir alan yok. Yani sihirbaz collector'ın, beacon'ın
ve API'nin çalıştığını **doğrulamaz**; onu §13'ten elle yapın.

Zorunlu kontrollerin hepsi geçmeden devir teslim adımı açılmaz. Burada
"geçti" ile "bakılamadı" aynı şey değil: **bakılamayan zorunlu bir
kontrol de bloke eder**, çünkü doğrulanmamış bir yalıtımı devretmek tam
olarak kimsenin fark etmediği durumdur. Uyarılar bloke etmez.

### 9.4 Devir teslim

Son adımda müşterinin e-postasını yazarsınız; panel **tek kullanımlık
bir sahiplenme bağlantısı** üretir.

Kaybederseniz kabuktan yeniden üretebilirsiniz:

```bash
./bin/panel -config /etc/crucible/panel.toml -owner-link musteri@example.com
```

Bağlantı kullanıldığında tek işlemde: hesap oluşturulur, **yapılandırılmış
her siteye sahiplik** verilir, davet tüketilir. Yarısı olmaz.

**Sahiplenme asla superadmin üretmez.** Siteye sahip olmak ile kurulumu
işletmek farklı işler; ikincisi kabuktan, bilerek oluşturulur.

### 9.5 Bundan sonra

Devir tesliminden sonra sunucuya girmeniz gerekirse müşteri **onaylamak
zorunda**:

```bash
./bin/panel -config /etc/crucible/panel.toml -dev-link -dev-reason "yavaşlık şikayeti"
```

Müşteri panelinde bir afiş görür (her sayfada), gerekçeyi okur, onaylar
ya da reddeder. Reddedilen bir istek bir daha onaylanamaz — yeniden
istemeniz gerekir.

**Panel kimin istediğini bilmez ve bunu söyler.** İstek, sunucuda kabuk
erişimi olan biri tarafından üretiliyor; gerekçe o kişinin yazdığı bir
cümle. Sayfa bunu ilk isteğin üstünde yazıyor.

---

## 10. Snippet

```bash
./bin/beacon -config /etc/crucible/beacon.toml -snippet https://example.com mysite
```

Çıkan `<script>` etiketini müşterinin sitesine ekleyin. `mysite`,
`beacon.toml`'daki `sites` listesinde **bulunmalı** — snippet herkese
açık olduğu için o liste zorunlu.

---

## 11. Bot verisi

Bu proje bilinen-bot parmak izi veri kümesini **dağıtmaz** (üçüncü
tarafa ait, şartları bu depoya yazılamaz). Kurulum kendi makinesine
indirir:

```bash
./bin/collector -config /etc/crucible/collector.toml -update-bot-data
```

**Hiç çalıştırmamak desteklenen bir durumdur:** bilinen-bot sinyali
olmaz, diğer bütün sinyaller çalışır, ve collector açılışta bunu söyler.

Cron önerisi (haftada bir):

```cron
0 4 * * 1 /opt/crucible/bin/collector -config /etc/crucible/collector.toml -update-bot-data
```

---

## 12. Ayarları panele taşıyın

Bazı ayarlar artık dosya yerine panelden değiştiriliyor. Dosyadakileri
bir kez veritabanına kopyalayın:

```bash
./bin/panel -config /etc/crucible/panel.toml \
    -migrate-settings collector -migrate-from /etc/crucible/collector.toml

./bin/panel -config /etc/crucible/panel.toml \
    -migrate-settings beacon -migrate-from /etc/crucible/beacon.toml
```

Komut:

- panelde zaten değeri olan bir ayarın **üstüne asla yazmaz**,
- neyi neden atladığını satır satır söyler,
- taşıdığı her değeri denetim kaydına, dosya ve satır adıyla yazar.

**Dosyayı silmeyin.** Hâlâ geri düşüş katmanı: veritabanı
okunamadığında süreç o değerlerle çalışır. Değişen tek şey, değişikliğin
artık panelden yapılıyor olması.

### Yeniden başlatmadan değişenler

Aşağıdakiler çalışan süreçte, bir yoklama aralığı içinde etkili olur —
saldırı sürerken SSH'a ihtiyaç duymamanız için:

| Ayar | Ne yapar |
|---|---|
| `limits.*` (her iki serviste ayrı) | Eşzamanlı bağlantı tavanı, saniyedeki istek, aşırı yük politikası, kuyruk |
| `asn_lookup.blocked_countries` | Ülke engel listesi (ISO 3166-1 alpha-2, örn. `["RU","KP"]`) |
| `asn_lookup.blocked_asns` | ASN engel listesi (pozitif sayılar) |
| `asn_lookup.known_bot_asns` + `apply_to_scoring` | Bot skoruna ASN katkısı |
| `beacon.trusted_proxies` | Hangi ağların ilettiği başlıklara güvenileceği |
| `logs.level`, `logs.verbose_until` | Log seviyesi ve kendiliğinden sönen debug penceresi |

**AS 0 kabul edilmez** — hem dosyada hem panelde reddedilir. Sebebi:
0, ASN çözümlemesinin "bulamadım" değeri; bir AS0 kuralı tek bir ağı
değil, çözümlenemeyen **her** adresi engellerdi.

Hiçbir şey engellemeyen kurulum bu özelliğin bedelini ödemez: sunucular
bağlantının ülkesini çözmeden önce "engellenen bir şey var mı" diye
sorar ve bu soru tek bir atomik okumadır.

**Yeniden başlatma isteyenler** panelde öyle işaretlidir (tampon
boyutları, önbellek pencereleri, `asn_lookup.enabled`) — süreç o
değerleri kanallarını ve tablolarını kurarken sabitler. Panel bunu
söyler; kabul edip sessizce yok saymaz.

### Panelde hiç olmayan ayar: saklama süresi

Yukarıdaki her şey dosyadan panele taşındı. **Analitik saklama süresi
ters yöne gitti** ve gerekçesi tam tersi olduğu için ayrıca yazıyorum.

Ziyaret kayıtlarının ne kadar tutulacağı, bu projedeki tek **hukuki**
ağırlıklı ayar. Paneldeki diğer her değer başarımı, doğruluğu veya diski
belirler; bu, bir insanın gezinme geçmişinin hiç duymadığı biri
tarafından ne kadar tutulacağını belirler.

Eskiden geliştirici parolası arkasında paneldeydi. O güçlü bir kilit —
ama müşterinin hâlâ içinde durduğu bir odanın kapısındaydı: değer HTTP
üzerinden görünüyor, HTTP üzerinden değişiyordu ve sızmış tek bir parola
onu başkasının kararı yapmaya yetiyordu. Artık her iki servisin de
`[retention]` bölümünde:

```toml
[retention]
days = 90                          # varsayılan; 1..730 arası
per_site = { "musteri-a" = 30 }    # daha azını isteyen tek müşteri
interval_hours = 1
```

**Tavan 730 gün** (2 yıl), eskiden 3650'di. Sebep: on yıl "sakla" ile
"sonsuza kadar sakla" arasındaki farkın kaybolduğu nokta olarak
seçilmişti — bu aritmetik hakkında bir cümle, bu ürünün altında
çalıştığı hukuk hakkında değil. Bir yıl değil iki yıl, çünkü eski
analitiğin dürüst kullanımı "geçen yılın aynı ayı" ve 365'lik tavan tam
da o karşılaştırmayı gereken son gün imkânsız kılar.

**Sınırın dışında bir değer yazan dosyayla servis başlamaz.** Kırpmaz,
görmezden gelmez, reddeder. En çok eski kurulumdan gelen için önemli:
3650 eskiden geçerliydi ve sınır dışı değerin eski davranışı sessizce 90
güne düşmekti — beş yıl sakladığını sanan bir kurulum üç ay saklıyor
olurdu ve bunu müşteriden öğrenirdi.

**Site başına saklama süresi taşınmayı sağ atlattı**, dosyada. "Bu
müşteri 30 gün istedi" gerçek bir taleptir. Hypertable en uzun süreyi
isteyen siteye göre tutar, daha kısa isteyenler satır satır temizlenir.

---

## 13. Gerçekten çalışıyor mu

Sihirbazın kontrol adımı bunların çoğunu zaten yapıyor. Elle
doğrulamak isterseniz:

```bash
# 1. Panel ayakta ve giriş sayfası geliyor mu
curl -sI http://127.0.0.1:8090/giris | head -1

# 2. API ayakta mı
curl -s http://127.0.0.1:8080/healthz

# 3. Jeton çalışıyor mu (panelin jetonuyla)
curl -s -H "Authorization: Bearer $JETON" http://127.0.0.1:8080/api/v1/sites

# 4. Collector satır yazıyor mu
psql "$DSN" -c "SELECT count(*), max(time) FROM traffic_snapshots;"

# 5. Beacon satır yazıyor mu
psql "$DSN" -c "SELECT count(*), max(time) FROM beacon_events;"

# 6. Saklama politikaları kuruldu mu
psql "$DSN" -c "SELECT * FROM timescaledb_information.jobs
                WHERE proc_name = 'policy_retention';"
```

**En iyi doğrulama:** müşterinin hesabıyla panele girip site sayfasını
açın. Sayılar geliyorsa zincirin tamamı çalışıyor demektir — toplama,
yazma, okuma API'si, jeton, yetki, render.

Sayı gelmiyorsa sayfa **neden** gelmediğini söyler ve üç ayrı cevabı
birbirine karıştırmaz: snippet hiç kurulmamış / bu dönem boş / API'ye
ulaşılamıyor.

---

## 14. Öneriler

**Sırayla kurun, hepsini birden değil.** Önce veritabanı + panel; devir
teslimi çalıştırın. Sonra collector. Sonra beacon. Her adımda bir şey
bozulursa hangi adımın bozduğunu bilirsiniz.

**`asn_lookup`'ı ilk günden açmayın.** Kapalıyken veri kümesi hiç
indirilmez ve o tablolara dokunulmaz. Coğrafya gerçekten gerektiğinde
açın; açtığınızda `internal/asnlookup/schema.sql`'i uygulamayı
unutmayın.

**Beacon'da `asn_lookup`'ı, collector aynı makinedeyse hiç açmayın.**
Collector zaten her IP için ülke/ASN çözüyor; beacon'da açmak aynı
tabloların ikinci bir kopyasını belleğe yükler (yüz megabayt
mertebesinde) ve karşılığında hiçbir şey vermez.

**IP saklama modunu varsayılan bırakın (`masked`).** Hukukçu kararı.
`full`'e geçmek iki koşul ister: geliştirici şifresi **ve** anahtarın
önceden config'de olması.

**Saklama süresini müşteriyle konuşun.** Varsayılan 90 gün.
Değiştirmek geliştirici şifresi ister, ve **geçmişe dönük değildir** —
süreyi kısaltmak o ana kadar toplanmış veriyi silmez, yeni politika
uygulanana kadar bekler.

**`govulncheck`'i her yükseltmede çalıştırın.**

```bash
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Güvenlik denetiminin en yüksek önemli iki bulgusu bağımlılıklardaydı ve
hiçbir kod incelemesi onları bulamazdı. Bu depoda **CI yok** — çalışmayı
hatırlamak şu an bir insana bağlı, ve bu bilinen bir eksik.

**Yedek alın.** Yedeği yapılandıran şey bu yazılım değil; kurulum
kontrolü yalnız "yapılandırılmış mı" diye sorar.

```bash
pg_dump -Fc analytics > /yedek/analytics-$(date +%F).dump
```

Yedeklenmesi gerekenler: veritabanı, `/etc/crucible/*.toml` (parolalar
ve anahtarlar orada), ve `/var/lib/crucible/known_bots.json`.

---

## 15. Sık yapılan hatalar

| Belirti | Sebep |
|---|---|
| Devir teslim düğmesi hiç çıkmıyor | `panel.toml`'daki `[roles]` boş. Kontroller "bakamadım" diyor, o da bloke ediyor |
| Panelde her ziyaretçi aynı IP | `trusted_proxies` boş ya da yanlış |
| `/beacon/` uçları 500 dönüyor | `analytics_reader`'a yalnız `traffic_snapshots` verilmiş |
| Kesişim sorguları hiçbir şey bulmuyor | Collector ve beacon'ın `ip_hash_key`'i farklı |
| Panelden ayar değişiyor, servis umursamıyor | `GRANT SELECT ON panel_settings` verilmemiş |
| İzolasyon kontrolü "bakamadım" diyor | Panel ayrı bir veritabanında — §4.1 |
| Panel açılmıyor, "unknown time zone" | `timezone` bu makinede tanınmıyor; zoneinfo paketi eksik olabilir |
| Beacon olayları reddediyor | `site_id`, `sites` listesinde yok |

---

## 16. Bilinen eksikler

Dürüst olmak, sonradan sürpriz olmaktan iyidir.

- **CI yok.** `govulncheck` ve testler elle çalıştırılıyor.
- **"Servisler ayakta mı" kontrolü binary'ye bağlı değil.**
  `preflight.checkService` yazılmış ve testleri var, ama `cmd/panel`
  ona hiç adres vermiyor ve `panel.toml`'da o adresleri yazacak bir alan
  yok. Kurulum sihirbazı bu yüzden 14 kontrol gösteriyor ve hiçbiri
  "collector çalışıyor mu" sorusunu sormuyor.
- **Kurulum betiği ve systemd unit'i depoda yok.** §7 bir şablon
  veriyor, paket vermiyor.
- **E-posta yolu yok.** Üye eklemek, hesabı **zaten olan** birini
  ekliyor. Davet e-postası ve parola sıfırlama henüz yok.
- **İki faktör kurtarma kodu yok.** Kaybeden kişiyi sahip ya da
  işletmeci kurtarır; tek sahip kaybederse kabuk gerekir.
- **Parola değişikliği diğer cihazlardaki oturumları kapatmıyor.**
- **Kontrol sonuçları ve elle-yapılacaklar listesi yalnız Türkçe.**
  Panelin geri kalanı Türkçe ve İngilizce.
- **Panelde kırılımların altısı var, otuzu değil.** Sayfa, kaynak,
  kampanya, cihaz, ülke ve olay kırılımları site sayfasında; parmak izi,
  ASN, skor dağılımı ve kesişim görünümlerinin panelde karşılığı yok.
  API uçları hazır ve jetonla doğrudan çağrılabilir.
- **Saklama süresi panelden değiştirilemez, bilerek.** İki servis de
  kendi yapılandırma dosyasından okuyor; değiştirmek sunucuya erişmeyi
  gerektiriyor. Gerekçesi §12'de.

Tamamı ve güncel hali için `PLAN.md` §0.5 ve `SECURITY.md`.

---

## 17. Nereye bakmalı

| Soru | Dosya |
|---|---|
| Bu sistem ne yapıyor, uçlar neler | `README.md` |
| Neden böyle yapıldı, hangi karar niye | `NOTES.md` |
| Ne bitti, ne kaldı, hangi risk kimin | `PLAN.md` |
| Güvenlik: ne denetlendi, ne açık | `SECURITY.md` |
| Hangi veri neden saklanıyor | `VERI-ENVANTERI.md` |
| Üçüncü taraf veri ve lisanslar | `THIRD-PARTY.md` |

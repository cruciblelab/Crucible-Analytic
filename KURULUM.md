# Kurulum Kılavuzu

Sıfırdan çalışan bir kuruluma kadar, sırayla. Bu belge **kurulumu yapan
geliştirici** için yazıldı — müşteri için değil.

Buradaki her komut depodaki gerçek bayrak ve dosyalardan alınmıştır.
Uydurma yok; bir şey yapılamıyorsa "yapılamıyor" yazıyor.

---

> **Takıldığınız bir kelime mi var?** [`SOZLUK.md`](SOZLUK.md) bu
> projede geçen her teknik terimi tek yerde açıklıyor — ne demek, burada
> nerede geçiyor, ve olmazsa ne kırılır.

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
                  | analytics-api    |  salt okunur
                  | (:8082)          |
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

## 1.5 İki kurulum yolu var — hangisi?

Bu kılavuzun tamamı **elle kurulumu** anlatıyor: bir VDS, systemd, ters
vekil, `release/install.sh`. On beş bölüm, ve dürüstçe uzun.

Bir de **konteyner yolu** var, ve müşteri başına bir yığın kuracaksanız
istediğiniz muhtemelen odur:

```bash
git clone <depo> && cd crucible-analytic
docker build -t crucible-analytic:$(git describe --tags --always) .

cd docker
cp .env.example .env && $EDITOR .env     # site adı, arka uç, parola
docker compose up -d

# İlk giriş: tek kullanımlık geliştirici bağlantısı
docker compose run --rm panel-cli panel -dev-link -base-url https://panel.example.com
```

`init` servisi bu kılavuzun 4., 5. ve 6. bölümlerini bir kez yapıyor:
dört rol, bütün şemalar, GRANT'ler, sertleştirme, doğrulama, ve
yapılandırma dosyalarını üretilmiş parolalarla yazma. Aynı
`release/install.sh` — konteyner için ayrı bir kurulum betiği yok, çünkü
kimsenin koşmadığı ikinci bir betik zamanla birincisinden ayrılır.

### Konteynerde neyin değiştiğini bilerek yapın

Üç şey elle kurulumdan farklı, ve üçü de bilinçli:

| Ne | Sunucuda | Konteynerde |
|---|---|---|
| Kayıtlar | `/var/log/crucible-analytic` altında dosya ağacı | stdout — konteyneri çalıştıran şey topluyor |
| Bağlanma adresi | `127.0.0.1:8082` — dışarıdan erişilemez | `0.0.0.0:8082` — ama porta **yayımlanmıyor** |
| Sırlar | `/etc/crucible-analytic` dizini | `conf` adlı kalıcı birim |

Ortadaki satır tek gerçek güvenlik değişikliği ve altını çiziyorum:
konteyner içinde `127.0.0.1` o konteynerin kendisidir, yani panel bir
sonraki konteynerdeki API'ye ulaşamaz. Loopback bağlamanın yerini
**compose ağı** alıyor: yalnız collector ve beacon porta yayımlanıyor,
diğer ikisi yalnız ağın içinden erişilebilir. `docker/compose.yml`
dosyasına `panel` altına bir `ports:` satırı eklemek, bir giriş formunu
internete açmak demektir. Bunu bir test kontrol ediyor
(`release/ports_test.go`).

**`conf` birimini silmeyin.** `ip_hash_key` orada, ve o anahtar saklanmış
her satırın takma adını üretiyor: değişirse collector ile beacon'ın
yazdığı iki yarı birbirini bulamaz olur, hata vermeden.

---

## 2. Ön gereksinimler

| Gereken | Sürüm | Not |
|---|---|---|
| Go | **1.25.13+** | `go.mod`'daki sürüm. **Yama sürümü kasıtlı:** 1.25.0 ile derlenen bir ağaçta `govulncheck` standart kütüphanede 34 erişilebilir zafiyet buluyor, 1.25.13 ile sıfır *(ölçüldü 2026-08-26)*. Kod değişmedi; CVE'ler sonradan yayımlandı. Eski bir araç zinciri ileride bir özellikte patlamaz, modülü baştan reddeder. Sunucuda gerekmez — başka yerde derleyip binary'yi kopyalayabilirsiniz. |
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

**Kolay yol: hazır paketi kullanın.** Elinizde bir sürüm paketi varsa bu
bölümü atlayın — binary'ler, şemalar, systemd birimleri ve örnek
yapılandırmalar içinde. Açtıktan sonra ilk iş bütünlüğünü doğrulamak:

```bash
tar xzf crucible-analytic-*.tar.gz
cd crucible-analytic-*/
sha256sum -c SHA256SUMS
```

Kendiniz paketlemek isterseniz, deponun kökünde:

```bash
VERSION=$(git describe --tags --always) ./release/build.sh
```

Aynı commit'ten alınan iki yapı **aynı baytları** üretir (`-trimpath`,
`CGO_ENABLED=0`, `-buildvcs=false`, go.mod'daki Go sürümü). Yani
indirdiğiniz binary'nin bu kaynaktan çıktığını kendiniz doğrulayabilirsiniz
— kimsenin kontrol edemeyeceği bir iddia, edilmeye değmez.

Elle derlemek isterseniz:

```bash
git clone https://github.com/cruciblelab/crucible-analytic.git
cd crucible-analytic

VERSION=$(git describe --tags --always)
for b in collector beacon analytics-api panel devpass; do
  go build -ldflags "-X main.version=$VERSION" -o "bin/$b" "./cmd/$b"
done
```

**Sürüm damgası beş binary'de de çalışıyor** *(G2'de kapandı)*. Her biri
`-version` ile cevap veriyor — destek "hangi yapıdasınız" diye
sorduğunda okunacak satır bu:

```bash
$ bin/collector -version
collector v0.4.1 (go1.25.13 linux/amd64)
```

Damgasız derlerseniz de bir şey söyler: Go'nun her yapıya gömdüğü commit
kullanılır (`a1b2c3d4e5f6`, çalışma ağacı kirliyse `-dirty` ekiyle).
Yalnız `-buildvcs=false` ile depo dışında derlenmiş bir yapı `unknown`
der.

*Bu satır önceden "sürüm damgası yalnız panelde çalışıyor" diyordu ve
doğruydu: `main.version` değişkeni yalnız `cmd/panel`'de tanımlıydı, ve
Go linker'ı olmayan bir sembole `-X` verilince uyarmıyor — sessizce
hiçbir şey yapıyor. Yani belgedeki komutun beş yinelemesinden dördü
işlevsizdi (`go tool nm` ile ölçüldü).*

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

### 4.2–4.4 Veritabanı, roller ve yetkiler

**Tek komut:**

```bash
sudo ./release/install.sh
```

Veritabanını ve dört rolü oluşturur, şemaları uygular, yetki matrisini
`release/sql/grants.sql`'den uygular, **ve sonucu veritabanına
doğrulatır.** Doğrulama geçmezse kurulum bitmez.

**Neden bu adım betikle:** rol ayrımı bu sistemin güvenlik temelinin
yarısı. Elle yazılan bir GRANT yanlış yazılabilir, ve yanlış bir GRANT
**hata vermez** — müşteriye hizmet veren, çalışan, ama tasarımın
dayandığı özelliği taşımayan bir kurulum üretir.

**Yetki bloğu burada tekrarlanmıyor, bilerek.** Tek kaynak
`release/sql/grants.sql` ve her satırının gerekçesi orada yazılı. Aynı
bloğun hem belgede hem betikte durması, ayrışmaya davettir — ve ayrışma
hep aynı yönde olur: betik koştuğu için düzeltilir, belge bir sonraki
işletmeciye başka bir şey vermesini söylemeye devam eder.

Elle yapmak isterseniz betiğin yaptığı sıra şudur:

```bash
DSN="postgres://postgres@localhost:5432/analytics"   # SUPERUSER olarak

psql "$DSN" -f internal/panel/schema.sql
psql "$DSN" -f internal/storage/schema.sql
psql "$DSN" -f internal/beacon/schema.sql
psql "$DSN" -f internal/asnlookup/schema.sql   # yalnız asn_lookup açıksa

psql "$DSN" -f release/sql/grants.sql
psql "$DSN" -f release/sql/verify.sql          # her satır t olmalı
```

**Şemaları superuser olarak uygulayın — servis rollerinden biriyle
değil.** Bir tablonun **sahibi**, GRANT'lardan bağımsız olarak o tablo
üzerinde her yetkiye örtük olarak sahiptir, kalıcı olarak. Şemaları
`collector` olarak uygularsanız o rol bütün tabloların sahibi olur ve
yalıtım **hiç yoktur** — üstelik her şey doğru görünür: GRANT'lar
doğrudur, `\dp` çıktısı doğrudur, ve panel analitiği okuyabilir.

*Bu, bu fazın kendi kurulumunda başına geldi: ilk koşu superuser olarak
collector'ı kullandı, bütün GRANT'lar doğru uygulandı, ve
`verify.sql` collector'ın `beacon_events`'e erişebildiğini bildirdi.
`verify.sql` artık sahipliği de kontrol ediyor.*

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
                   ('panel_settings'),('panel_smtp'),
                   ('panel_logs'),('panel_operations')) t(tbl)
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
| panel_user | panel_users, panel_settings, panel_smtp | t | t | t | t |
| panel_user | traffic_snapshots, beacon_events | **f** | **f** | **f** | **f** |
| collector, beacon_writer, analytics_reader | panel_smtp | **f** | **f** | **f** | **f** |

Kalın yazılanlar tesadüf değil, tasarım:

- **beacon kendi yazdığını okuyamaz.** Yazmak için okumaya ihtiyacı yok;
  okuyabilseydi olay tablosunun tamamı, ziyaretçilerden veri alan
  sürecin erişiminde olurdu.
- **Denetim kaydı `UPDATE` ve `DELETE` almıyor.** Ele geçirilmiş bir
  panel süreci ne yaptığını silemesin diye.
- **Panel hiçbir analitik satırını göremiyor.** Bütün sistemin dayandığı
  satır bu; §4.1'in sebebi de bu.
- **E-posta hesabını yalnız panel okuyabiliyor.** `panel_smtp`, bu
  veritabanındaki tek geri okunabilir sır olan giden posta şifresini
  tutuyor — şifrelenmiş olarak, anahtarı panelin yapılandırma dosyasında
  (§5). Ayarların içinde değil, kendi tablosunda olmasının sebebi tam
  olarak yukarıdaki `panel_settings` satırı: collector ve beacon o
  tabloyu okuyabiliyor, ve posta şifresi internete bakan iki sürecin
  eline geçmesi için hiçbir sebep yok.

Bu blok gerçek bir TimescaleDB'ye (16.6 / 2.17.2) uygulanarak
doğrulandı, çıkan matris yukarıdaki tablodur.

### 4.5 Kimsenin vermediği yetkiler

Yukarıdaki matris `GRANT`'ların ne yaptığını gösteriyor. Bir de
**hiç `GRANT` edilmediği hâlde açık gelen** üç şey var; hiçbiri bir yetki
listesinde görünmez, çünkü hiçbiri verilmemiştir. `release/install.sh`
bunları kapatıyor (`release/sql/harden.sql`), ve `verify.sql` kapandığını
doğruluyor — elle kuruyorsanız o dosyayı kendiniz uygulayın:

```
psql "$DSN" -v dbname=analytics -f release/sql/harden.sql
```

**1. Arka plan işi zamanlama.** TimescaleDB `add_job()` üzerindeki
`EXECUTE`'u `PUBLIC`'e verir. Bu kurulumda ölçüldü: `panel_user` — panel
dışında hiçbir tablo yetkisi olmayan rol — bir iş zamanlayabildi. İş,
sahibi olan rol olarak çalışır, yani yetki yükseltmesi değildir; ama
oturumdan, bağlantı havuzundan ve **servisin yeniden başlatılmasından**
sağ çıkar. Bu ürün hiçbir iş zamanlamaz. Sıkıştırma ya da saklama
politikası istiyorsanız superuser olarak siz uygularsınız.

**2. TimescaleDB telemetrisi.** Varsayılan `basic`; 24 saatte bir
`telemetry.timescale.com` adresine sürüm, uzantı listesi, işletim
sistemi, hypertable ve satır sayıları gider. İçinde ziyaretçi verisi
yoktur. Yine de kapatılıyor: bu ürünün müşteriye verdiği söz trafiğinin
kendi makinesinden çıkmaması, ve altındaki veritabanının günlük olarak
dışarı bağlantı açması o sözle çelişir. Açık kalmasını isteyen bir
kurulum bunu bilerek yapmalı.

**3. Veritabanına `PUBLIC` bağlanabilmesi.** PostgreSQL her yeni
veritabanında `CONNECT`'i `PUBLIC`'e verir, yani kümedeki **herhangi bir
rol** — başka bir uygulamanın rolü, eski bir göçten kalan — buraya
bağlanabilir. Tabloda yetkisi olmaz; ama TimescaleDB'nin kataloğu
tasarım gereği herkese okunabilir olduğu için hypertable'ları, chunk
adlarını ve zaman aralıklarını sayabilir.

### 4.6 Uzak veritabanı kullanıyorsanız: `sslmode`

Örnek DSN'lerin hepsi `localhost` gösteriyor ve orada şifreleme
gerekmez — baytlar ağ arayüzüne hiç çıkmaz.

Veritabanını **başka bir makineye** taşırsanız DSN'lere `sslmode`
eklemeniz gerekir:

```
postgres://panel_user:PAROLA@db.example.com:5432/analytics?sslmode=verify-full
```

Sebebi şu: libpq'nun varsayılanı `prefer`'dır. TLS'i dener ve sunucu
sunmuyorsa **sessizce şifresiz devam eder.** Hata vermez, yapılandırmada
iz bırakmaz — veritabanı parolanız ve her analitik satır ağdan açık
geçer, ve bunu gösteren hiçbir şey olmaz.

`require` şifrelemeyi zorunlu kılar; `verify-full` ayrıca sunucunun
sertifikasını ve adını doğrular ve tercih edilmesi gereken odur.

Kurulum sihirbazının kontrol adımı bunu sizin yerinize ölçüyor: bağlantının
gerçekten şifreli olup olmadığını veritabanına sorar, ve sunucu uzaktaysa
şifresiz bir bağlantıyı uyarı olarak gösterir.

---

## 5. Sırlar

Üçünü de kuruluma başlamadan üretin.

### 5.0 Müşteri kilitlenirse ne olacak — hiçbir şey yapmanız gerekmiyor

Hesap oluşturulduğunda müşteriye **sekiz tek kullanımlık kurtarma kodu**
bir kez gösteriliyor. Parolasını unutur ya da telefonunu kaybederse,
giriş sayfasındaki "Giremiyorum" bağlantısından kendi başına geri
giriyor. **Sizi aramasına gerek yok, e-posta sunucusu gerekmiyor,
ayarlanacak bir şey yok.**

Kodlarını da kaybederse: üye listesinden kodlarını yenileyip birini
kendisine iletirsiniz. Ayrı bir mekanizma değil, aynı form.

Müşteriye söylemeniz gereken tek şey: **o sayfadaki kodları saklasın.**
Sayfa kapandığında kodların kendisi kayboluyor, panelde yalnız
karşılıkları duruyor.

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

**Birim dosyaları depoda var** — `release/systemd/` altında, ve
`release/install.sh` bunları `/etc/systemd/system/` içine kurar. Bu
bölüm uzun süre "depoda systemd unit dosyası yok" diyordu; G2 fazından
beri doğru değildi ve düzeltildi. Var olmayan bir eksiği anlatan bir
belge, okuyana gerçek eksikler hakkındakilere de inanmamayı öğretir.

Kurulan altı dosya:

| Dosya | Ne |
|---|---|
| `crucible-collector.service` | Siteyi önleyen TLS vekili |
| `crucible-beacon.service` | Sayfa snippet'inin POST ettiği uç |
| `crucible-analytics-api.service` | Salt okunur JSON API |
| `crucible-panel.service` | Müşterinin paneli |
| `crucible-upgrader.service` | Şema yükseltmesini uygulayan altıncı binary |
| `crucible-upgrader.timer` | Yukarıdakini çalıştıran şey |

Dördü uzun ömürlü servis; `install.sh` bunları kurar ama **başlatmaz**:

```bash
systemctl enable --now crucible-collector crucible-beacon \
                       crucible-analytics-api crucible-panel
```

### Yükseltici: servisi değil, zamanlayıcıyı etkinleştirin

```bash
systemctl enable --now crucible-upgrader.timer
```

`crucible-upgrader.service`'in `[Install]` bölümü **yok**, bilerek. Onu
tek başına etkinleştirmek açılışta bir kez koşturur ve bir daha asla —
yani paneldeki yükseltme düğmesi "istendi" der, satırı yazar, ve o satırı
kimse okumaz. Düğmenin çalışması zamanlayıcıya bağlı.

Zamanlayıcı açılıştan 2 dakika sonra ve her 30 saniyede bir bakar.
Otuz saniye, birinin düğmeye yeni bastığı ve bir sayfaya baktığı için;
bakma işi hemen hemen her seferinde hiçbir şey bulmayan tek bir indeks
aramasıdır.

Ne yaptığını görmek için:

```bash
systemctl list-timers crucible-upgrader.timer
journalctl -u crucible-upgrader -n 50
```

### Yükselticinin kendi hesabı var — ve bu bir tercih değil

`install.sh` iki sistem hesabı açar: dört servis için `crucible`,
yükseltici için `crucible-upgrader`.

Sebebi tek bir dosya: `upgrader.toml`, bu dağıtımda **DDL koşabilen tek
DSN'i** taşır (`schema_admin` rolü, tabloların sahibi). Panel `crucible`
olarak koşuyor. Yükseltici de `crucible` olsaydı, `upgrader.toml`'un
`crucible` tarafından okunabilmesi gerekirdi — ve panel, okuması bile
yasak olan veritabanını yeniden yazan kimlik bilgisini okuyabilirdi. Beş
ayrı veritabanı rolünün satın aldığı her şey tek bir dosya izniyle geri
alınmış olurdu.

Elle kurduysanız aynısını yapın:

```bash
useradd --system --no-create-home --shell /usr/sbin/nologin crucible-upgrader
chgrp crucible-upgrader /etc/crucible-analytic/upgrader.toml
chmod 0640              /etc/crucible-analytic/upgrader.toml
```

### Yapılandırmayı servisler okuyabiliyor mu

`install.sh` bunu kendisi ayarlar; elle kuruyorsanız atlanan adım
budur ve atlandığında **hiçbir servis başlamaz**:

```bash
chgrp crucible /etc/crucible-analytic /etc/crucible-analytic/*.toml
chmod 0751     /etc/crucible-analytic
chmod 0640     /etc/crucible-analytic/*.toml
```

Sahip `root` kalır, hesap yalnız okuma alır. Servis yapılandırmasını
okur, asla yeniden yazmaz: panelini ele geçiren biri `panel.toml`'u
düzenleyebilseydi bir sonraki yeniden başlatmayı kendi seçtiği
veritabanına yönlendirebilirdi.

Dizin `0751` — son hane, `crucible-upgrader`'ın `crucible` grubunda
olmadan kendi dosyasına ulaşabilmesi için. `r`siz `x`, "adını biliyorsan
açabilirsin" demektir, "etrafa bakabilirsin" değil.

**Collector'ın farkı:** 443'ü dinlediği için `CAP_NET_BIND_SERVICE` ile
gelen tek birim odur; diğer beşi hiçbir yetenek istemez.

### Log dizini

```bash
mkdir -p /var/log/crucible-analytic
chown crucible: /var/log/crucible-analytic
chmod 700 /var/log/crucible-analytic
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
| `sources.country`, `sources.asn`, `sources.fallback_order` | Hangi IP aralığı veri kümesinin kullanılacağı ve biri erişilemezse sıradakiler. Bir sonraki zamanlanmış yenilemede etkili olur — indirme anında, isteğin ortasında değil |
| `beacon.trusted_proxies` | Hangi ağların ilettiği başlıklara güvenileceği |
| `logs.level`, `logs.verbose_until` | Log seviyesi ve kendiliğinden sönen debug penceresi |

**Çekim kaydı.** `asn_lookup` açıkken her veri kümesi çekimi
`ip_range_fetches` tablosuna bir satır yazar: hangi kaynak, hangi adres
ailesi, kaç satır, kaç bayt, ve başarısızsa tam hata zinciri. "Coğrafya
verim güncel mi, değilse neden" sorusunun cevabı burada — daha önce
yalnız sunucunun kendi günlüğündeydi, yani kabuğu olmayan müşterinin
bakamayacağı tek yerde. 90 gün saklanır, çeken servis kendi süpürür.

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
curl -s http://127.0.0.1:8082/healthz

# 3. Jeton çalışıyor mu (panelin jetonuyla)
curl -s -H "Authorization: Bearer $JETON" http://127.0.0.1:8082/api/v1/sites

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
ve anahtarlar orada), ve `/var/lib/crucible-analytic/known_bots.json`.

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
| Hangi veri neden saklanıyor | `README.md` → "Privacy model" ve §879'daki `privacy.ip_storage` satırı |
| Lisans, atıf ve dağıtım kapsamı | `LICENSE`, `NOTICE`, `THIRD-PARTY.md` |
| Üçüncü taraf veri ve lisanslar | `THIRD-PARTY.md` |

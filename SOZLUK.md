# Sözlük

Bu projede geçen her teknik terim, tek yerde.

Amacı bir ansiklopedi olmak değil: **bu depoda bir şeyi okurken
takıldığınız kelimeyi açıklamak.** O yüzden her madde üç şeyi söylüyor —
ne demek, bu projede nerede geçiyor, ve olmazsa ne kırılır. Üçüncüsü
genellikle en yararlısı, çünkü bu üründeki arızaların çoğu çökme değil:
sessizce yanlış çalışan bir şey.

Terimler alfabetik değil, **konuya göre** sıralı. Aradığınız kelimeyi
tarayıcının arama kutusuyla bulun; komşularını okumak genellikle işe
yarar, çünkü bu terimler tek tek değil öbek öbek anlam kazanıyor.

> Adı geçen paket, tablo ve binary'lerin gerçekten var olduğunu
> `release/glossary_test.go` kontrol ediyor. Bir sözlük, anlattığı
> şeyden ayrı düştüğü anda zarar vermeye başlar.

---

## İçindekiler

1. [Ürünün parçaları](#1-ürünün-parçaları)
2. [Veritabanı](#2-veritabanı)
3. [Yetkiler ve roller](#3-yetkiler-ve-roller)
4. [TLS, vekil ve parmak izi](#4-tls-vekil-ve-parmak-izi)
5. [Bot tespiti ve skorlama](#5-bot-tespiti-ve-skorlama)
6. [Gizlilik](#6-gizlilik)
7. [Kimlik ve oturum](#7-kimlik-ve-oturum)
8. [Panel kavramları](#8-panel-kavramları)
9. [Yapılandırma](#9-yapılandırma)
10. [Kurulum ve dağıtım](#10-kurulum-ve-dağıtım)
11. [Test ve doğrulama](#11-test-ve-doğrulama)
12. [Bu projeye özgü deyimler](#12-bu-projeye-özgü-deyimler)

---

## 1. Ürünün parçaları

**collector** — TLS vekili. Müşterinin sitesinin **önünde** durur, her
bağlantıyı görür, parmak izini alır, skorlar, ve baytları sitesine
iletir. Beş binary'nin internetle konuşanı ve bu yüzden **HTTP sunucusu
yok** — saldırganın baytına dokunan süreçte dinleyen fazladan bir soket
istemiyoruz. Ayakta olup olmadığı `service_heartbeat` satırından
anlaşılır.

**beacon** — Sayfaya gömülen JavaScript parçacığının POST ettiği uç.
Collector'ın göremediğini görür: sayfa görüntülemeleri, oturumlar,
kampanya parametreleri. `beacon_events` tablosuna yazar, oradan okuyamaz
— yalnız `INSERT` yetkisi var.

**analytics-api** — Salt okunur HTTP API. Analitik tablolarını **yalnız
bu** okur. Panel sayıları buradan alır, bir dış panel nasıl alacaksa
öyle. Bearer jetonla korunur.

**panel** — Müşterinin giriş yaptığı web arayüzü. Kendi veritabanı
roluyla çalışır ve o rolün **analitik tablolarına hiç erişimi yoktur**;
sayıları HTTP üzerinden `analytics-api`'den okur. Bu, sistemin güvenlik
temelinin yarısı.

**devpass** — Tek işi olan küçük bir araç: geliştirici parolasını
argon2id hash'ine çevirir, ya da rastgele bir `ip_hash_key` üretir.
Panel formu olsaydı, panel kendini sınırlayan parolayı değiştirebilirdi.

**upgrader** — Altıncı binary, ve kurulumda **DDL çalıştırabilen tek
şey**. Panel şema göçü yapamaz — `grants.sql`'de hiçbir servis rolü için
`ALTER`, `CREATE` veya `OWNER` yok, ve bu B6 ile H5'in bilerek kurduğu
şey. O yüzden panel `panel_upgrade_requests`'e bir istek satırı yazar,
upgrader o satırı görüp şemayı uygular ve sonucu geri yazar.

Sormak ve cevaplamak ayrı yetkiler: panel `INSERT`+`SELECT` tutar,
upgrader `SELECT`+`UPDATE`. Hiçbiri ikisini birden tutmaz, yani ele
geçirilmiş bir panel yükseltme *isteyebilir* ama bir yükseltmenin
sonucunu **uyduramaz**.

systemd zamanlayıcısı çalıştırır, tek geçiş yapar ve çıkar — dinlenen
bir makinede DDL yetkili açık bir bağlantı kalmaz. Kendi yapılandırma
dosyasını okur, ve o dosya kurulumdaki tek DDL yetkili DSN'i taşır.
Taşıdığı şemanın parmak izi isteğinkiyle uyuşmazsa **reddeder**: eski
bir upgrader, yeni bir panelin istediği göçü uygulamaz.

**snippet** — Müşterinin sayfasına eklenen küçük JavaScript. Beacon'a
olay POST eder. `beacon -snippet <url> <site>` komutu üretir.

---

## 2. Veritabanı

**PostgreSQL** — Altındaki ilişkisel veritabanı. 16.6 ile test edildi.

**TimescaleDB** — PostgreSQL eklentisi. Zaman serisi tabloları için
*hypertable* ve otomatik *saklama politikası* getirir. 2.17.2 ile test
edildi.

**hypertable** — TimescaleDB'nin zaman aralıklarına bölünmüş tablosu.
Dışarıdan normal bir tablo gibi görünür; içeride *chunk*'lardan oluşur.
`traffic_snapshots` ve `beacon_events` bu projede hypertable.

**chunk** — Bir hypertable'ın bir zaman aralığını tutan parçası. Eski
veriyi silmenin ucuz yolu `DELETE` değil, chunk'ı düşürmektir: bir dosya
bağlantısını koparmak ile sayfaları yeniden yazmak arasındaki fark.

**saklama politikası (retention policy)** (`internal/retention`) — "Şu
yaştan eski chunk'ları düşür" diyen, TimescaleDB'nin kendi
zamanlayıcısında koşan iş. Bu projede tavan **730 gün**. Olmazsa iki
tablo da sonsuza kadar büyür ve dolan disk collector'ı durdurur — yani
analitik özelliği, müşterinin sitesini düşürür.

**DSN** — Veritabanı bağlantı dizesi:
`postgres://kullanıcı:parola@makine:port/veritabanı`. Her servisin kendi
rolüne ait bir DSN'i var.

**rol (role)** — PostgreSQL'de kullanıcı. **Küme genelindedir**, tek bir
veritabanına ait değil — bu yüzden `collector` adında bir rol makinede
zaten olabilir. Dört servis rolü: `collector`, `beacon_writer`,
`analytics_reader`, `panel_user`.

**süperkullanıcı (superuser)** — Her yetkiye sahip rol. Kurulumu yapan
odur; **hiçbir servis süperkullanıcı değildir** ve `verify.sql` bunu
kontrol eder. Süperkullanıcı satır düzeyi güvenliği de atlar.

**sahiplik (ownership)** — Bir tabloyu **kim yarattıysa** onun
sahibidir, ve sahibe her şey açıktır. Yetkiden farklı bir kavram, ve bu
projede pahalı bir ders oldu: geliştirme veritabanını `collector`
yaratmıştı, yani her şeyin sahibiydi, yani on entegrasyon paketi hiçbir
kurulumun vermediği yetkiyle koşuyordu. TimescaleDB de saklama
politikası için yetkiye değil sahipliğe bakar.

**GRANT / REVOKE** — Yetki verme / geri alma. `release/sql/grants.sql`
her rolün ne yapabileceğini söyler; `harden.sql` kimsenin yapmaması
gerekeni kapatır.

**satır düzeyi güvenlik (RLS — row-level security)** — `GRANT`'in
söyleyemediği kuralı söyler: "yalnız kendi satırın". Bu projede tek
yerde kullanılıyor — `service_heartbeat`, dört servisin yazdığı tek
tablo. `ENABLE ROW LEVEL SECURITY` unutulursa politikalar tanımlı
görünür ve hiçbir şey uygulanmaz.

**SECURITY DEFINER** — Fonksiyonu çağıran değil, **tanımlayan** rolün
yetkileriyle çalıştıran fonksiyon. Bu projede saklama politikasını
uygulamak için kullanılıyor: collector hypertable'ın sahibi olamaz, ama
sahibi adına çalışan dar bir fonksiyonu çağırabilir. Tehlikesi
`search_path`'tir (aşağıda).

**search_path** — PostgreSQL'in nitelenmemiş adları hangi şemalarda
arayacağı. `SECURITY DEFINER` bir fonksiyonda sabitlenmezse, çağıran
hangi `add_retention_policy`'nin koşacağına karar edebilir. Bu projedeki
her sarmalayıcı sabitliyor, ve `verify.sql` sabitlediğini doğruluyor.

**arka plan işi (background job)** — TimescaleDB'nin zamanlayıcısında
koşan iş. `add_job` **keyfî bir fonksiyon adı** alır ve iş, süreci
yaratan uygulama yeniden başlasa da yaşamaya devam eder: yani bir dakika
ele geçirilmiş bir süreç, aylarca koşan bir şey bırakabilir. Bu yüzden
`harden.sql` iş zamanlama fonksiyonlarını PUBLIC'ten alır.

**telemetri (telemetry)** — TimescaleDB'nin varsayılan olarak açık olan,
günde bir kez `telemetry.timescale.com`'a sürüm ve tablo sayısı bildiren
işi. İçinde ziyaretçi verisi yok — ama bu ürünün iddiası verinin
müşterinin makinesinden çıkmadığı, o yüzden kapatılıyor.

**PUBLIC** — PostgreSQL'de "her rol". Yeni bir veritabanına `CONNECT`,
yeni bir fonksiyona `EXECUTE` varsayılan olarak PUBLIC'e verilir — yani
kimsenin vermediği ama orada duran yetkiler. `harden.sql` bunları
kapatır.

### Tablolar

| Tablo | Kim yazar | Ne tutar |
|---|---|---|
| `traffic_snapshots` | collector | Her aktif adres için, her boşaltmada bir satır: parmak izi, skor, hız |
| `beacon_events` | beacon | Sayfa görüntüleme ve olaylar |
| `ip_asn_ranges`, `ip_country_ranges` | collector, beacon | Adres → ASN/ülke aralıkları (tazelenen önbellek) |
| `service_heartbeat` | dört servis de | Servis başına bir satır: sürüm, çalışma süresi, sayaçlar, son hata |
| `panel_users`, `panel_sessions`, `panel_site_members` | panel | Hesaplar, oturumlar, kimin hangi siteye erişebildiği |
| `panel_settings` | panel | Canlı ayarlar (collector ve beacon **okur**) |
| `panel_audit_log` | panel | Kim ne yaptı — silinemez, değiştirilemez |
| `panel_dev_access`, `panel_owner_claims` | panel | Geliştirici erişim istekleri, sahiplik davetleri |
| `panel_recovery_codes`, `panel_login_attempts` | panel | Kurtarma kodları, hız sınırı için giriş denemeleri |
| `panel_smtp` | panel | Giden posta hesabı (parola şifreli) |
| `panel_api_tokens` | panel | Panelin kendi API jetonları |

---

## 3. Yetkiler ve roller

### İki kişi: geliştirici ve müşteri

Bu belgede en çok geçen iki kelime, ve hiçbir yerde tanımlı
olmadıkları için bir kez burada.

**geliştirici** — Bu ürünü **kuran, yöneten, teknik olarak sorumlu
olan** kişi. Ürünü yazan biz değiliz kastedilen; ürünü bir makineye
kurup çalışır hâlde tutan kişi. Bugün o kişi biziz. Yarın, ürün açık
kaynak olduğu için, onu ticari ya da ticari olmayan biçimde kullanan
herhangi biri. Sunucuda kabuğu olan, `install.sh`'ı çalıştıran,
sihirbazı yürüten, `devpass` parolasını bilen kişi budur.

**müşteri** — **Geliştiricinin müşterisi.** Site sahibi. Panele girer,
sayılarına bakar, ayarlarını yönetir. Kabuğu yoktur, olması da
beklenmez. Ürünün "SSH'siz onarılabilsin" iddiası bu kişi için var.

Bir kurulumda geliştirici ile müşteri **aynı kişi de olabilir** —
birisi ürünü kendi sitesi için kurmuşsa öyledir. Ayrım rollerde değil,
**hangi parolayı bildiğinde**: geliştirici parolası yapılandırma
dosyasından gelir, müşteri parolası veritabanından.

### Erişim: yetkiyle değil, parolayla

**Bu, panelin en kolay yanlış yapılacak kararı, o yüzden açıkça
yazılıyor.**

Site rolleri (Sahip / Yönetici / İzleyici) müşterinin **kendi
tarafındaki** ayrımdır, ve müşteri onları kendisi dağıtır — `Sahip`'te
`CapManageMembers` vardır, yani sahip kendine ya da istediğine
`Yönetici` verebilir, `Yönetici`'de de `CapUseDeveloperMode` vardır.

Bundan çıkan sonuç: **bir müşteri kendine yetki vererek yetenek
kazanabilir.** Bakmakla biten şeyler için sorun değil — kimse bir
grafiğe bakarak başkasına iş çıkarmaz.

Ama **geliştiriciye iş çıkarabilen hiçbir şey yetenekle korunamaz.**
Müşteri o yeteneği kendine verebilir, ve verdiğini kimse durduramaz;
durdurmaya çalışmak da yanlış olur, çünkü kurulum müşterinin.

Böyle şeyler **geliştirici parolasının** arkasında durur:
`developer_gate` (hukuki ağırlığı olan ayarlar), ve aynı gerekçeyle
kilitlendiğinde şema yükseltmesi. Parola yapılandırma dosyasından
gelir; veritabanından değil, yetkiden hiç değil. Müşteri kendine
veremeyeceği tek şey odur.

**collector (rol)** — `traffic_snapshots` üzerinde `SELECT, INSERT`.
`DELETE` **yok**: trafik yolundaki bir sürecin geçmişi silememesi
bilinçli.

**beacon_writer** — `beacon_events` üzerinde yalnız `INSERT`. Yazdığını
okuyamaz.

**analytics_reader** — İki analitik tablosunda `SELECT`, başka hiçbir
şey. `analytics-api`'nin rolü.

**panel_user** — Bütün `panel_*` tablolarında tam yetki, analitik
tablolarında **hiçbir yetki**. `verify.sql`'in en önemli olumsuz
doğrulaması bu.

**Sahip / Yönetici / İzleyici (Owner / Admin / Viewer)** — Panel
içindeki site rolleri. Sahip devredebilir ve üye yönetir; yönetici
günlük işi yürütür; izleyici yalnız bakar. Veritabanı rolleriyle
karıştırmayın: bunlar `panel_site_members` tablosundaki satırlar.

**yetenek (capability)** — Panelin izin kontrolünün birimi:
`CapViewAnalytics`, `CapManageMembers`, `CapManageSettings`,
`CapManageTokens`, `CapViewAudit`, `CapUseDeveloperMode`,
`CapDeleteSite`. Menüdeki filtreleme kozmetiktir — asıl kilit her
işleyicinin içinde.

**yetki matrisi (privilege matrix)** — Dört rolün ne yapıp ne
yapamadığının tamamı. `grants.sql` uygular, `verify.sql` **veritabanına
sorarak** doğrular, ve doğrulama geçmezse kurulum bitmez. Çalışan bir
`GRANT` bloğu ile doğru bir yetki matrisi aynı şey değildir.

---

## 4. TLS, vekil ve parmak izi

**TLS** — Tarayıcı ile sunucu arasındaki şifreleme. HTTPS'in "S"si.

**ClientHello** — TLS el sıkışmasının ilk mesajı. **Şifrelenmeden önce**
gönderilir, yani araya giren biri şifreyi çözmeden de okuyabilir. İçinde
istemcinin desteklediği şifre takımları, uzantılar ve eğriler var — yani
istemcinin bir tür imzası.

**JA4** (`internal/ja4`) — ClientHello'dan türetilen istemci parmak izi.
Örnek: `t13d1516h2_8daaf6152771_b186095e22b6`. Aynı yazılım aynı parmak
izini üretir, yani "bu Chrome mu yoksa `curl` mı" sorusuna User-Agent'a
inanmadan cevap verir. Bu ürünün varlık sebebi: **User-Agent yalan
söyleyebilir, TLS el sıkışması söyleyemez.**

**passthrough (geçirgen) mod** (`internal/proxy`) — Collector'ın
varsayılanı. ClientHello'yu okur, parmak izini alır, ve baytları
**çözmeden** siteye iletir. Yani collector müşterinin trafiğini göremez
— göremediği bir şeyi sızdıramaz.

**full (tam) mod** (`internal/fullproxy`) — Collector TLS'i sonlandırır:
sertifikayı o taşır, isteği çözer, arka uca düz HTTP ile iletir. Daha
çok görür, karşılığında daha çok sorumluluk alır.

**arka uç (backend_addr)** — Collector'ın vekillik ettiği yer:
müşterinin kendi sitesi. Passthrough modda burası TLS el sıkışmasını
**kendisi bitirebilmek** zorunda.

**ters vekil (reverse proxy)** — Önde durup TLS'i sonlandıran ve
arkadaki servise düz HTTP ile ileten sunucu (nginx, Caddy). Panel ve
okuma API'si bunun arkasında durmalı.

**loopback bağlama** — `listen_addr = "127.0.0.1:8082"`. Servis yalnız
makinenin kendisinden erişilebilir olur; ağdan erişilemez. **Konteynerde
bu kontrol çalışmaz** — orada `127.0.0.1` o konteynerin kendisidir,
yerini compose ağı alır.

**SNI** — TLS el sıkışmasında istemcinin "hangi alan adına bağlanıyorum"
demesi. ClientHello'nun içinde, şifrelenmemiş.

**güvenilen vekil (trusted_proxies)** — `X-Forwarded-For` gibi
başlıklara **yalnız** bu adreslerden gelirse inanılır. Listeye
almadığınız birinden gelen başlık, ziyaretçinin kendi yazdığı bir
metindir.

---

## 5. Bot tespiti ve skorlama

**bot skoru (bot_score)** (`internal/scoring`) — 0–100 arası bir sayı.
Yüksek olan "bu otomatik" demektir. Tek bir işaretten değil, birkaçının
birleşiminden gelir: parmak izi bilinen bir araca mı ait, adres bilinen
bir bot ASN'sinde mi, hız insan gibi mi.

**bilinen bot (known bot)** (`internal/botdata`) — Otomasyon araçlarına
ait JA4 parmak izleri listesi. **Veri üründe gömülü değil**, kurulumdan
sonra indirilir: mekanizmayı gönderiyoruz, veriyi değil. `collector
-update-bot-data`.

**ASN** (`internal/asnlookup`) — Otonom Sistem Numarası. Bir IP
aralığının hangi kuruma ait olduğunu söyler — bir veri merkezi mi, bir
ev interneti mi. Bulut sağlayıcısından gelen trafik, mobil operatörden
gelenden farklı bir şeydir.

**kaynak kütüphanesi (source library)** (`internal/ipsources`) — Bu
yapının indirmeyi ve ayrıştırmayı bildiği IP aralığı veri kümelerinin
listesi: kimlik, etiket, "bunu neden seçersin", URL'ler, lisans. Panelin
ayar seçenekleri buradan üretilir, o yüzden kaynak eklemek tek yerde
olur.

Neden panelde serbest URL kutusu **yok**: bir kaynak URL değil, *URL +
ayrıştırıcı*dır. Kutu ancak zaten desteklenen biçimdeki bir şeyi
gösterebilirdi — yani "yeni sağlayıcı ekleme" işini yapamazdı, ama
yapabildiğini sandırırdı. Yapabileceği tek şeyin (aynı biçim, başka
sunucu) karşılığı zaten `asn_lookup.local_csv_path`.

**yedek sıralaması (fallback order)** — Seçilen kaynak erişilemezse
sırayla denenecekler. Yanlış türdekiler atlanır: ülke yenilemesi bir ASN
veri kümesine düşmez.

**çekim kaydı (fetch log)** (`ip_range_fetches`) — Her veri kümesi çekimi
bir satır: hangi kaynak, hangi adres ailesi, ne zaman başladı ve bitti,
kaç satır ayrıştırıldı, kaç bayt okundu, ve başarısızsa tam hata zinciri.
Yenileme başına değil **dosya başına**, çünkü IPv4 ile IPv6 ayrı düşüyor
ve "biri güncel, öteki bir aylık" durumunun tek bir sonuçta dürüst bir
karşılığı yok.

Çeken servis yazar, panel yalnız okur, kimse `UPDATE` edemez. Yoksa
"coğrafya verim güncel mi" sorusunun cevabı yalnız sunucunun kendi
günlüğünde kalır — kabuğu olmayan müşterinin bakamayacağı tek yerde.

**yenileme kuyruğu (refresh queue)** (`internal/rangerefresh`,
`ip_range_refresh_requests`) — *Sağlık* sayfasındaki "şimdi yenile"
düğmesinin arkasındaki tablo. Panel bir satır yazar; çeken servis otuz
saniyede bir bakar, alır, yenilemeyi yapar, sonucu geri yazar. Panel
dışarı bağlantı açmaz — açsaydı, müşterinin tarayıcısının ulaştığı
sürecin içinde bir SSRF yüzeyi olurdu.

`internal/upgrade` ile aynı desen, bir tablo ötede. Tek gerçek fark:
upgrader hep kuruludur, resolver ise yalnız `asn_lookup` açıksa vardır ve
o varsayılan kapalıdır — yani "kimse almadı" burada olağan hâl. Bu yüzden
alınmamış istekler birkaç dakika sonra düşer; yoksa ilk basış, o
kurulumun kabul ettiği son basış olurdu.

Geliştirici parolası **istemez**, ve bu L3'ten bilinçli bir fark: kural
"geliştiriciye iş çıkarabilen şeyler parolanın arkasında", bu ise
müşterinin kendi sunucusuna kendi hattından iki kamuya açık dosyayı
yeniden indiriyor.

**sessiz adres (silent address)** — Collector'ın gördüğü ama beacon'ın
hiç görmediği adres. Yani JavaScript çalıştırmamış: ya bot, ya JS kapalı
bir tarayıcı. **JavaScript tabanlı hiçbir analitik aracın göremediği
trafik** budur.

**JS-bot** — Tersi: JavaScript çalıştıran ama parmak izi otomasyona ait
olan. Headless tarayıcılar.

**kesişim (crossover)** — İki veri kaynağının aynı adres üzerinden
birleştirilmesi. "Bu adres hem bağlandı hem JavaScript çalıştırdı mı"
sorusunu cevaplar — ki tek başına ne collector ne beacon cevaplayabilir.
Bu ürünün iddiasını kanıtlayan görünüm bu, ve `ip_hash_key` iki tarafta
aynı değilse **sessizce hiçbir şey bulmaz**.

**kayan pencere (sliding window)** (`internal/ratestore`) — Hız ölçmenin
ucuz yolu: her isteği kaydetmek yerine, son N saniyedeki sayacı tutmak.
Bellek trafikten bağımsız sabit kalır.

---

## 6. Gizlilik

**ip_storage** (`internal/privacy`) — Adreslerin diske nasıl yazılacağı.
Üç değer:

- `full` — adres olduğu gibi.
- `masked` (varsayılan) — son sekizli maskelenir (`203.0.113.42` →
  `203.0.113.0`). Ülke ve ASN yine bulunabilir, ziyaretçi sayısı yine
  doğrudur.
- `hashed` — adres yerine anahtarlı bir takma ad yazılır.

**ip_hash_key** — `hashed` modda takma adı üreten anahtar. En az 32
bayt. **Collector ve beacon'da birebir aynı olmak zorunda**, çünkü
kesişim birleştirmesi bu takma ad üzerinden yapılır. Farklıysa hata
verilmez; birleştirme hiçbir şey bulmaz ve sessiz bir hafta gibi
görünür. Kurulum betiği bu yüzden ikisini karşılaştırır.

**takma ad (pseudonym)** — Kişiyi doğrudan tanımlamayan ama aynı kişiyi
tanımaya yeten değer. Anonimleştirme değil: anahtarı olan geri
çevirebilir. Bir /24 ağında 16,7 milyon olasılık var, hepsini denemek
bir saniyeden kısa sürer — o yüzden anahtar veritabanı parolasının
yanında durur.

**gizlilik kartı** — Ziyaretçiye hangi verinin toplandığını gösteren
sayfa. Taklit edilememesi ve başka veriye erişememesi kural.

**drop_params / extra_params** — URL'den atılan ve tutulan sorgu
parametreleri. Kampanya parametreleri (`utm_*`) tutulur; kişisel veri
taşıyabilecekler atılır.

---

## 7. Kimlik ve oturum

**argon2id** (`internal/argon2id`) — Parola hash algoritması. Bilerek
yavaş ve bellek-yoğun: çalınan bir hash tablosunu denemek pahalı olsun
diye. Panel parolaları ve geliştirici parolası bununla saklanır.

**SHA-256** — Hızlı özet fonksiyonu. Parola için **uygun değil** (hızlı
olması kötü), ama rastgele üretilmiş jetonlar için doğru: jetonun
kendisi zaten tahmin edilemez.

**bearer jeton** — `Authorization: Bearer <jeton>` başlığıyla taşınan
kimlik. Okuma API'si bunu ister. Jetonun kendisi **saklanmaz**, yalnız
SHA-256'sı — yani sızan bir yapılandırma dosyası çalışan bir kimlik
vermez.

**TOTP** — Telefondaki uygulamanın ürettiği altı haneli, otuz saniyede
bir değişen kod. İkinci faktör.

**kurtarma kodu (recovery code)** — Telefonunu kaybedeni kurtaran, tek
kullanımlık kod. E-posta gerektirmeden çalışması bilinçli.

**oturum çerezi (session cookie)** — Girişten sonra tarayıcının tuttuğu
kimlik. **Secure** işaretliyse tarayıcı onu düz HTTP üzerinden geri
göndermez — o yüzden TLS olmayan bir kurulumda giriş "hiçbir şey
yapmıyor" gibi görünür. `secure_cookies = false` yalnız geliştirme için.

**HSTS** — Tarayıcıya "bu alan adına bir yıl boyunca düz HTTP ile
bağlanma" demek. Varsayılan kapalı ve `secure_cookies`'e bilerek
bağlanmadı: sertifikası sonradan gelen bir kurulumda yanlış açılmış bir
HSTS sizi panelden kilitler.

**CSRF** — Başka bir sitenin, sizin oturumunuzu kullanarak sizin adınıza
istek yaptırması. Her formda gizli bir jetonla engellenir.

**şifreli saklama (sealed)** (`internal/sealed`) — Bu veritabanındaki
geri okunabilir tek sır giden posta parolası: her gönderimde karşı
sunucuya verilmek zorunda. Panelin `secret_key`'i onu şifreler. Neyi
engellediği net: **veritabanının yapılandırma dosyası olmadan ele
geçmesi** — gecelik yedek, hazırlık ortamına geri yükleme. Neyi
engellemediği de net: panel sürecine erişen biri.

---

## 8. Panel kavramları

**kurulum sihirbazı** (`/kurulum/`) — Geliştiricinin yürüdüğü teknik
adımlar: veritabanı, siteler, görünüm, toplama, saklama, kontrol, devir.
**Yapılandırdığından çok doğrular**: veritabanı rollerini, TLS
sertifikasını panel değiştiremez ve değiştirememelidir, o yüzden o
adımlar gerçek durumu okuyup söyler.

**preflight** (`internal/panel/preflight`) — Sihirbazın "gerçekten
çalışıyor mu" kontrolleri. Yarısı **başka bir rolün** ne yapabildiğini
sorar (`has_table_privilege`), çünkü asıl soru panelin analitik
okuyamadığı. "Bakılmadı" ile "baktık ve sorun yok" ayrı sonuçlar, ve
bakılamayan zorunlu bir kontrol devir teslimi engeller.

**geliştirici bağlantısı (dev-link)** — Sunucuda kabuğu olan birinin
ürettiği tek kullanımlık giriş bağlantısı (`panel -dev-link`). Hesap
yokken kendiliğinden onaylı; **ilk hesap oluştuğu anda o otomatik onay
biter** ve sonrası sahibin onayına bağlanır.

**erişim onayı** (`/erisim`) — Sahibin, geliştirici erişim isteklerini
onayladığı sayfa. Bir geliştirici burada **onay veremez** — verebilseydi
sahibe bir kez sorulur, sonsuza kadar yeterdi.

**devir teslim (handover)** — Kurulumun geliştiriciden müşteriye
geçmesi. Sihirbazın son adımı bir davet bağlantısı üretir; müşteri
`/sahiplen/` ile hesabını açar.

**teknik kapı** (`/teknik`) — Sahibin, geliştiricinin kullandığı teknik
sihirbaza ulaştığı onay sayfası. Ne gizli ne açık: sunucu müşterinin,
ama yanlışlıkla tıklayan biri çalışan bir kurulumu bozmasın diye tek
onay var.

**geliştirici kapısı (developer_gate)** (`internal/devgate`) — Hukuki
ağırlığı olan ayarların (IP saklama modu, saklama süresi, hangi kampanya
parametrelerinin diske ulaştığı) önündeki ikinci parola. **Her
değişiklikte sorulur.** Bölüm yapılandırmada hiç yoksa o ayarlar
panelden kimse tarafından değiştirilemez — güvenli yön budur.

**sağlık sayfası** (`/saglik`) — Sürümler, çalışma süreleri, sayaçlar,
disk boyutları, okuma API'sinin cevap verip vermediği. Tek kuralı: **her
bölüm kendi başına düşer.** Hepsi birlikte kararan bir sağlık sayfası,
tam da okunduğu anda hiçbir şey söylemez. Üstünde **tek bir ziyaretçi
sayısı yok** — olsaydı panelin roluna analitiğe giden bir yol açardı.

**kalp atışı (heartbeat)** (`internal/heartbeat`) — Her servisin
dakikada bir kendi satırını yazması. `/healthz`'den farklı bir soruyu
cevaplar: `/healthz` "bu süreç şu an ayakta" der, kalp atışı "son yazma
başarılı oldu, 14:02'de" der. Bir müşteriye bir hafta veri kaybettiren
arıza, **ayakta olan ve salıdan beri her yazması başarısız olan** bir
collector'dır.

**denetim kaydı (audit log)** — Kim ne yaptı. Panel yazabilir, **silemez
ve değiştiremez** — `verify.sql` bunu doğrular.

**canlı ayarlar** (`internal/settings`) — `panel_settings` tablosundan
gelen, servis yeniden başlatmadan etkili olan ayarlar. Yapılandırma
dosyası geri düşüş (fallback); panel değişikliğin yapıldığı yer.

---

## 9. Yapılandırma

**TOML** — Yapılandırma dosyalarının biçimi. Bir tuzağı bu projeye
pahalıya mal oldu: **bir `[başlık]`'tan sonraki her anahtar o tabloya
aittir.** Üst düzey bir ayarın yorumlu yer tutucusu bir başlığın altına
düşerse, yorumu kaldırmak onu yanlış tabloya koyar ve hiçbir şey hata
vermez.

**`[privacy]`, `[retention]`, `[logging]`, `[limits]`** — Yapılandırma
tabloları. Anahtarın hangisine ait olduğu önemli; yukarıdaki tuzak.

**listen_addr** — Servisin dinlediği adres:port.

**flush_interval_seconds** — Collector'ın biriktirdiği satırları
veritabanına yazma sıklığı.

**overload_policy** (`internal/limiter`) — Sınırlar aşıldığında ne
olacağı: `fail_open` (isteği geçir, ölçümü kaybet), `fail_closed`
(reddet), `throttle` (kuyruğa al). Beacon'da `fail_open`'ın anlamı biraz
farklı: arkada korunacak bir site yok, o yüzden isteği kabul edip olayı
düşürür.

**secret_key** — Panelin şifreleme anahtarı. Tek işi giden posta
parolasını veritabanında şifreli tutmak. **Değiştirilirse** kayıtlı
posta parolası açılmaz olur — kaybedilecek tek şey odur.

**password_hash** — Geliştirici parolasının argon2id hash'i. Düz metin
kabul edilmez; panel açılmaz.

---

## 10. Kurulum ve dağıtım

**release/build.sh** — Sürüm paketini üretir. **Tekrarlanabilir**: aynı
commit, her makinede aynı baytlar. `-trimpath` derleme makinesinin
yollarını, `CGO_ENABLED=0` host'un C araç zincirini, `-buildvcs=false`
git bloğunu binary'den çıkarır.

**release/install.sh** — Veritabanını, dört rolü, yetkileri, sırları ve
yapılandırma dosyalarını kuran betik. Amacı on dakika kazandırmak değil:
**yetki matrisinin tek dosyadan uygulanıp veritabanına doğrulatılması**,
ve doğrulamayı geçmeyen bir kurulumun bitmemesi. GNU sed ister — BusyBox
sed kullandığı biçimi sessizce yok sayıyor, o yüzden yokluğunu kontrol
eder.

**grants.sql / harden.sql / verify.sql** — Sırasıyla: kimin ne
yapabileceği, kimsenin yapmaması gerekenin kapatılması, ve sonucun
veritabanına sorularak doğrulanması. Üçüncüsü olmadan ilk ikisi
"çalıştı" der, "işe yaradı" demez.

**systemd** — Linux'ta servisleri başlatan ve ayakta tutan sistem.
`release/systemd/*.service` birimleri.

**Docker imajı** — Tek imaj, beş giriş noktası (`collector`, `beacon`,
`analytics-api`, `panel`, `devpass`, `upgrader`) artı `init`. Ayrı
imajlar değil:
dört servis aynı şemayı ve aynı `ip_hash_key`'i paylaşıyor, sürümleri
kayan beş etiketin arızası çökme değil — sessizce boş bir kesişim
görünümü.

**init konteyneri** — Bir kez koşup çıkan konteyner. Aynı
`release/install.sh`'ı çalıştırır. Konteyner için ikinci bir kurulum
betiği yok: kimsenin koşmadığı ikinci betik zamanla birincisinden
ayrılır.

**birim (volume)** — Konteyner silinse de kalan disk alanı. `conf`
birimi yapılandırmayı ve `ip_hash_key`'i tutar; **silinmemeli**, çünkü o
anahtar saklanmış her satırın takma adını üretir.

**entrypoint** — Konteyner başlarken çalışan komut. Buradaki, hangi
servisin isteneceğine bakıp doğru binary'yi doğru yapılandırmayla
çalıştırır.

**yayımlanan port (published port)** — Konteynerin bir portunun host'a
açılması. Bu projede yalnız collector ve beacon yayımlanır; panel ve
okuma API'si yalnız compose ağının içinden erişilebilir.

---

## 11. Test ve doğrulama

**birim testi (unit test)** — Bağımlılık gerektirmeyen, saniyeler süren
test. Birleştirme kapısında koşar.

**entegrasyon testi** — Gerçek veritabanına karşı koşan test. Rol seçimi
`internal/testdb`'de. `-tags integration`. Bu projede her paket
**üretimde koştuğu rolle** bağlanır — daha yetkili bir düzenek üretimi
test etmez.

**uçtan uca test (E2E)** — Ürünün tamamını bir kez çalıştırır. `-tags
e2e` tarball kurulumunu, `-tags docker` konteyner yığınını. Aynı soruyu
iki kuruluma sorarlar ve cevap hep aynı çıkmadı.

**derleme etiketi (build tag)** — `//go:build integration` gibi. Testin
hangi koşulda derleneceğini söyler; pahalı testleri kapının dışında
tutmanın yolu.

**yarış dedektörü (race detector)** — `go test -race`. İki goroutine'in
aynı belleğe kilitsiz eriştiğini yakalar.

**fuzz** — Ayrıştırıcıya rastgele ve bozuk girdi verip çökertmeye
çalışmak. Saldırganın baytını okuyan ayrıştırıcılar için
(`internal/ja4`, beacon JSON) gecelik koşuyor.

**mutasyon testi (mutation testing)** — Kodu bilerek bozup testin
kırmızıya dönüp dönmediğine bakmak. Bu projede her yeni doğrulama böyle
sınandı: **yeşil hâli hiçbir şey söylemeyen bir kontrol, olmaması
gereken bir kontroldür.**

**SAST / gosec** (`internal/sast`) — Kaynak kodu güvenlik açığı
kalıpları için tarayan araç. **Taban çizgisi (baseline)**: bilinen ve
gerekçelendirilmiş bulgular kayıtlı; kapı yalnız **yeni** bulguya
kırmızı yanar.

**govulncheck** — Kullanılan kütüphanelerde ve Go standart
kütüphanesinde bilinen zafiyetleri arar. Erişilebilirlik analizi yapar:
paket zafiyetli olsa da çağırmadığınız fonksiyondaysa saymaz.

**kontrol grubu (control)** — Bir testin "bizim kodumuz çalışıyor" ile
"kütüphane zaten öyle yapıyormuş" arasını ayıran ikinci ölçümü. Bu
projede birkaç kez, kodun hiçbir şey yapmadığı ortaya çıktı.

---

## 12. Bu projeye özgü deyimler

Bu depoda tekrar tekrar geçen ve NOTES.md'nin diliyle yazılmış cümleler.
Bir kural olarak okunmalılar.

**"Sessizce yanlış çalışmak"** — Bu projedeki arızaların çoğunun biçimi.
Çökmez, hata vermez, log basmaz; sadece yanlış sonuç üretir ya da hiç
üretmez. En pahalı örnek: yanlış TOML tablosuna yazılan bir anahtar.

**"Çalıştı ile işe yaradı ayrı şeylerdir"** — Hatasız koşan bir `REVOKE`
ile etkisini gösteren bir `REVOKE` aynı olgu değil. O yüzden
`verify.sql` var.

**"Yeşil hâli hiçbir şey söylemeyen kontrol"** — Boş iki değeri
karşılaştıran bir eşitlik testi geçer ve bir şey kanıtlamaz. Böyle bir
kontrol, olmamasından daha kötüdür: güven verir.

**"Var olmayan bir deliği uyaran yorum"** — Yanlış bir güvenlik uyarısı,
okuyucuya gerçek uyarıları da ciddiye almamayı öğretir. Bu oturumda üç
kez oldu ve üçü de düzeltildi.

**"Üretimden daha yetkili bir test düzeneği üretimi test etmez"** —
Geliştirme veritabanını test rolünün yaratmış olması, aylarca beş gerçek
kusuru gizledi.

**"Bakılmadı ile baktık ve sorun yok ayrı sonuçlar"** — Preflight'ın ve
sağlık sayfasının her yerinde geçerli. Bir kontrolün çalışamamış olması,
geçmiş olması değildir.

**"Mekanizmayı gönderiyoruz, veriyi değil"** — Bot verisi pakete
konmuyor, kurulumdan sonra indiriliyor.

**"Ölçüldü"** — Bu depoda bir iddia bu kelimeyle bitiyorsa, biri o
davranışı gerçekten çalıştırıp görmüştür. Bitmiyorsa, tahmindir.

---

*Bu dosya elle tutuluyor. Yeni bir terim eklerken: ne demek, bu projede
nerede, olmazsa ne kırılır — üçü birden.*

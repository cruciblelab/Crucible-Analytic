# Crucible Analytic — Veri Envanteri

**Hukuki değerlendirme için hazırlanmış teknik envanterdir. Hukuki metin
değildir.**

Bu belge, sistemin **fiilen hangi alanları veritabanına yazdığını**
listeler. Her satır, kaynak koddaki şema dosyalarından birebir
çıkarılmıştır (`internal/storage/schema.sql`,
`internal/beacon/schema.sql`, `internal/panel/schema.sql`), tarif veya
tahmin değildir.

Son güncelleme: 2026-09-08 — Bölüm 6 (saklama süresi) ve Bölüm 7
(ziyaretçiye dönük açıklama yüzeyi) "planlanan"dan çıkıp yazılana
taşındı. İkisi de eski hâliyle sistemi olduğundan **kötü** anlatıyordu.

---

## 0. Sistemin şekli — hukukçunun önce bilmesi gereken

**Kendi sunucunuzda çalışır.** Yazılım müşterinin kendi sunucusuna
kurulur; veriler o sunucudaki veritabanında kalır. Veri, yazılımı
geliştiren tarafa (Crucible) veya herhangi bir üçüncü tarafa
**aktarılmaz**. CDN yok, harici analitik servisi yok, reklam ağı yok.

**İki ayrı veri kaynağı vardır** ve topladıkları farklıdır:

| Kaynak | Ne görür | Ne göremez |
|---|---|---|
| **Collector** (ters vekil) | Siteye gelen **her bağlantıyı** — JavaScript çalıştırsın çalıştırmasın | Şifreli trafikte adres satırını (URL) göremez |
| **Beacon** (JS) | Sayfa yolu, başlık, yönlendiren, ekran, dil | Yalnız JavaScript çalıştıran istemcilerden veri alır |

**Üç ayrı ilgili kişi kategorisi vardır.** Karıştırılmamalı:

1. **Site ziyaretçileri** — Bölüm 1 ve 2.
2. **Müşterinin kendi personeli** (panele giren yöneticiler) — Bölüm 3.
3. Kimse — Bölüm 4 (referans verisi, kişisel veri içermez).

---

## 1. `traffic_snapshots` — Collector kayıtları

**Bu bir istek kaydı (request log) DEĞİLDİR.** Collector'ın bellekteki
sayaçlarının **periyodik özetidir** (varsayılan 10 saniyede bir).
Ziyaretçinin hangi sayfaya gittiği bu tabloda **yoktur ve yazılamaz**.

| Sütun | Tip | İçerik | Kişisel veri mi? |
|---|---|---|---|
| `time` | zaman damgası | Özetin alındığı an | Tek başına hayır |
| `site_id` | metin | Hangi site | Hayır |
| `ip` | IP adresi | **Ziyaretçinin IP adresi — varsayılan olarak maskeli** (bkz. Bölüm 1.5) | **Evet** |
| `ja4` | metin | TLS parmak izi (aşağıya bakınız) | Tartışmalı — aşağıda açıklandı |
| `prev_window_count` | tam sayı | Önceki penceredeki istek sayısı | Hayır |
| `curr_window_count` | tam sayı | Mevcut penceredeki istek sayısı | Hayır |
| `request_rate` | ondalık | Saniyedeki istek sayısı | Hayır |
| `bot_score` | 0–100 | Hesaplanan bot olasılık skoru | Türetilmiş |
| `is_known_bot_ja4` | evet/hayır | Parmak izi bilinen bot listesinde mi | Türetilmiş |
| `country` | metin | IP'den türetilen ülke kodu (örn. `TR`) | Türetilmiş |
| `asn` | tam sayı | IP'den türetilen operatör numarası | Türetilmiş |
| `asn_org` | metin | Operatör adı (örn. `Turk Telekom`) | Türetilmiş |
| `is_known_bot_asn` | evet/hayır | Operatör bilinen bot listesinde mi | Türetilmiş |

**Bu tabloda olmayan, sorulması muhtemel şeyler:** ziyaret edilen sayfa
adresi (URL), sayfa başlığı, yönlendiren site, çerez, oturum kimliği,
kullanıcı adı, e-posta, form içeriği. Şemada bunlar için **sütun
yoktur.**

### 1.5 IP maskeleme — varsayılan davranış (2026-08-16'dan itibaren)

Hukukçu görüşü doğrultusunda **varsayılan maskelidir** ve bu, yazılımın
kendiliğinden yaptığı şeydir; bir kurulum hiçbir ayar yapmazsa maskeli
saklar.

| Mod | `ip` sütunu | `ip_hash` sütunu | Anahtar gerekir mi |
|---|---|---|---|
| `masked` **(varsayılan)** | `185.23.45.0` (yalnız ağ) | boş | hayır |
| `full` | `185.23.45.0` (yalnız ağ) | tam adresten türetilmiş anahtarlı jeton | **evet** |

### 1.6 Ham IP adresi hiçbir modda saklanmaz

Bu, tasarımın etrafında kurulduğu kural. İki mod var ve **ikisi de**
adresi ağına indirger:

- **`masked`** — yalnız ağ (IPv4 /24, IPv6 /64) yazılır. Anahtar
  gerekmez, yapılandırma gerekmez; hiçbir şey ayarlanmamış bir kurulum
  bu moddadır.
- **`full`** — aynı maskeli ağ yazılır, **yanına** tam adresten
  türetilmiş anahtarlı bir jeton eklenir. Bu jeton aynı /24 içindeki iki
  ziyaretçiyi ayırmaya yarar; adresin kendisi yine hiçbir sütuna
  yazılmaz.

**`full` moda geçmek ciddi bir iştir ve iki koşulu birden ister:**
1. Geliştirici şifresi (Bölüm 8.5) — her değişiklikte sorulur.
2. Jeton anahtarının **önceden** yapılandırma dosyasında bulunması.
   Anahtar yoksa panel bu değeri **reddeder**; kabul edip sessizce
   maskeli modda çalışmaz. (Anahtarsız kabul edilseydi kurulum, ayarı
   "full" diyerek maskeli davranırdı — bir modun sessizce başka bir mod
   olması, bu ayarın yanlış olabileceği en kötü biçim.)

Maskeli moddayken anahtar **hiç kullanılmaz** ve gerekmez.

**Jetonun koruduğu ve korumadığı:** koruduğu, veritabanının tek başına
adres vermemesi — çalınan yedek, kopyalanan disk, SQL enjeksiyonu, ele
geçirilmiş salt okunur API. Korumadığı, anahtarı elinde tutan taraf:
satır zaten /24'ü taşıdığı için elinde anahtar olan biri 256 adayı
dener. Yani doğru cümle **"adres verinin kendisinden geri
türetilemez"** — *"hiç kimse hiçbir zaman"* değil.

**Kesişim birleşimi:** maskeli modda /24 çözünürlüğünde, full modda tam
adres çözünürlüğünde çalışır. Mod değişiminden önce ve sonra yazılmış
satırlar birbirine eşleşmez; bu doğrudur, çünkü ikisinin aynı ziyaretçi
olup olmadığını hiçbir şey bilemez.

**Maskeleme yazma anında olur, sonradan değil.** Maskelenmemiş adres
diske hiç değmez: bellekte kalır, işini yapar, satır yazılırken
maskelenir. Yani "önce tam kaydedip sonra temizleme" diye bir aşama
yoktur — temizlenecek bir şey hiç oluşmaz.

**Tam adresin bellekte yaptığı iki iş** (ikisi de kişisel veri
üretmez):
1. Ziyaretçi kimliğini türetir (Bölüm 2.3'teki `visitor_id`) — bu, aynı
   /24 arkasındaki iki kişinin ayrı sayılabilmesini sağlar.
2. Ülke/operatör çözümlemesi yapar — maskeli adresten çözümlenseydi
   sonuç ziyaretçinin değil, ağ bloğunun kaydına ait olurdu.

**Maskelemenin bedeli, açıkça:** iki veri kaynağını birleştiren analiz
(collector ↔ beacon) /24 çözünürlüğünde çalışır. Aynı /24 içindeki iki
farklı ziyaretçi bu birleşimde tek satır gibi görünebilir. Panel bunu
gizlemez; ilgili görünümler "bu kurulumda IP maskeleme açık" der.

**Geriye dönük değildir.** Mod değiştiğinde eski satırlar olduğu gibi
kalır. Değişimin ne zaman yapıldığı, kim tarafından yapıldığı ve önceki
değerin ne olduğu **silinemez denetim kaydına** yazılır
(`panel_audit_log`).

**Bu ayarı değiştirmek ayrı bir şifre ister** — Bölüm 8.5.

### JA4 (TLS parmak izi) hakkında — hukukçu için önemli ayrım

JA4, ziyaretçinin *cihazını değil*, **tarayıcı yazılımının TLS
ayarlarını** özetleyen bir dizedir. Aynı Chrome sürümünü aynı işletim
sisteminde kullanan **binlerce kişi aynı JA4 değerini üretir.**

Yani JA4 **tekil bir tanımlayıcı değildir**; ayırt edici gücü "Chrome
131 / Windows 11" demekle karşılaştırılabilir. Tek başına bir kişiyi
işaret etmez. Kullanım amacı, otomatik trafiği (bot) insan trafiğinden
ayırmaktır.

### Collector'ın "görmesi" ile "kaydetmesi" arasındaki fark

Collector bir ters vekildir, yani trafik onun üzerinden geçer.

- **Passthrough modunda** trafik şifreli olarak geçer; yazılım adres
  satırını **teknik olarak göremez**.
- **Full modunda** yazılım HTTP'yi sonlandırır, dolayısıyla adres
  satırını ve başlıkları *işleme sırasında görür* — ancak **kaydetmez.**
  Yukarıdaki tabloda bunlar için sütun bulunmamaktadır.

Bu ayrım hukuken sorulabilir: veri *aktarım sırasında işlenir*, fakat
*saklanmaz*.

---

## 2. `beacon_events` — Tarayıcı (JavaScript) kayıtları

Sayfa görüntüleme başına bir satır.

| Sütun | Tip | İçerik | Kişisel veri mi? |
|---|---|---|---|
| `time` | zaman damgası | Olay anı | Tek başına hayır |
| `site_id` | metin | Hangi site | Hayır |
| `visitor_id` | metin (32 hane) | **Takma ziyaretçi kimliği** — aşağıda ayrıntılı | Takma adlı veri |
| `event_type` | metin | `pageview` veya `event` | Hayır |
| `event_name` | metin | Sitenin kendi kodunun tanımladığı olay adı (örn. `signup`) | Siteye bağlı |
| `path` | metin | Sayfa yolu (`/urunler/ayakkabi`) — **sorgu dizesi hariç** | Tek başına hayır |
| `query` | metin | **Yalnızca izin listesindeki kampanya parametreleri** — aşağıda | Hayır |
| `title` | metin | Sayfa başlığı | Hayır |
| `utm_source` | metin | Kampanya kaynağı (`instagram`) | Hayır |
| `utm_medium` | metin | Kampanya kanalı (`email`) | Hayır |
| `utm_campaign` | metin | Kampanya adı (`bahar-indirimi`) | Hayır |
| `utm_term` | metin | Anahtar kelime — **aşağıdaki uyarıya bakınız** | Duruma bağlı |
| `utm_content` | metin | Kampanya varyantı (`mavi-buton`) | Hayır |
| `ref` | metin | UTM kullanmayan sitelerin kısaltması | Hayır |
| `click_source` | metin | Hangi reklam ağından tıklandı: `google` / `facebook` / `microsoft` / boş | Hayır |
| `click_id` | metin | Ham tıklama kimliği — **varsayılan olarak BOŞ**, aşağıya bakınız | **Evet, saklanırsa** |
| `referrer_host` | metin | Gelinen sitenin alan adı (`google.com`) | Hayır |
| `referrer_path` | metin | Gelinen sitedeki yol — **sorgu dizesi tamamen atılır** | Hayır |
| `ip` | IP adresi | **Ziyaretçinin IP adresi — varsayılan olarak maskeli** (iki tabloyu birleştiren anahtar; bkz. Bölüm 1.5) | **Evet** |
| `browser` | metin | Sunucuda User-Agent'tan çıkarılır (`Chrome`) | Hayır |
| `os` | metin | Sunucuda çıkarılır (`Windows`) | Hayır |
| `device` | metin | `desktop` / `mobile` / `tablet` | Hayır |
| `is_bot_ua` | evet/hayır | Tarayıcı kendini bot olarak tanıtıyor mu | Hayır |
| `screen_w`, `screen_h` | tam sayı | Ekran genişlik/yükseklik (piksel) | Hayır |
| `language` | metin | Tarayıcı dili (`tr-TR`) | Hayır |
| `country`, `asn`, `asn_org` | — | IP'den türetilir, Bölüm 1'deki ile aynı | Türetilmiş |

### Tarayıcının sunucuya gönderdiği şeyin tamamı

Kaynak koddan (`internal/beacon/beacon.js`) birebir — dokuz alan, başka
hiçbir şey:

```
site, type, name, url, referrer, title, screen_w, screen_h, language
```

Sunucunun ayrıca **HTTP başlıklarından okudukları**: `User-Agent`
(tarayıcı/OS sınıflandırması için), `X-Forwarded-For` / `X-Real-IP`
(yalnızca güvenilir vekilden geldiyse, gerçek IP için), `Origin`.

### `visitor_id` — takma kimliğin tam tanımı

Ziyaretçiyi saymak için **çerez kullanılmaz.** Bunun yerine her istek
için şu hesaplanır:

```
visitor_id = HMAC-SHA256( günlük_tuz , site_kimliği ‖ IP ‖ User-Agent )
```

Hukuki değerlendirmeyi doğrudan ilgilendiren dört özellik:

1. **Tuz rastgeledir, yalnızca bellekte tutulur ve hiçbir yere
   yazılmaz.** Veritabanı yedeğinde, diskte, log dosyasında bulunmaz.
2. **Tuz 24 saatte bir değişir ve eskisi silinir.** Bu nedenle bir
   günden eski kayıtlardaki `visitor_id` değerleri **hiç kimse
   tarafından — veritabanının tamamına sahip olan dâhil — yeniden bir
   IP'ye bağlanamaz.** Yeniden türetmek matematiksel olarak mümkün
   değildir, çünkü girdi artık mevcut değildir.
3. **`site_kimliği` hesaba dâhildir**, yani aynı kişi iki farklı sitede
   **birbiriyle ilişkilendirilemeyen** iki ayrı kimlik alır. Siteler
   arası takip yapısal olarak imkânsızdır — kapatılmış bir özellik
   değil, hiç var olmayan bir özellik.
4. **IPv6 adresleri /64 önekine kırpılarak** hesaba girer (adresin
   tamamı değil).

Sonuç: `visitor_id` **kalıcı bir tanımlayıcı değildir.** Ayrıca aynı
tabloda `ip` sütunu bulunduğu için, günün içinde ilişkilendirme IP
üzerinden zaten mümkündür — `visitor_id`'nin işlevi gizlemek değil,
**doğru saymaktır** (aynı IP arkasındaki farklı kişileri ayırmak,
değişen IP'deki aynı kişiyi birleştirmek).

### Sorgu dizesi (query string) — kasıtlı kısıtlama

Sayfa adresindeki sorgu dizesi **olduğu gibi saklanmaz.** Yalnızca şu
dokuz parametre saklanır, başka hiçbiri:

```
utm_source  utm_medium  utm_campaign  utm_term  utm_content
ref  gclid  fbclid  msclkid
```

Gerekçe, kaynak kodda yazılıdır: sorgu dizeleri sıklıkla **şifre
sıfırlama jetonu, davet kodu, oturum kimliği ve e-posta adresi** taşır.
İzin listesi olmadan bunlar analitik tablosunda süresiz kalırdı. Listede
olmayan her parametre **atılır** — yeni bir parametrenin saklanabilmesi
için kaynak koda elle eklenmesi gerekir.

**Yönlendiren sitenin (referrer) sorgu dizesi ise tamamen atılır** —
tek bir parametresi bile saklanmaz. Örnek olarak: bir kullanıcı
Google'da **organik** arama yapıp siteye geldiğinde, **arama terimi
saklanmaz**; yalnızca `google.com` saklanır.

Bu liste **kurulum başına ayarlanabilir** (`[campaign]` bölümü): standart
bir parametre çıkarılabilir, listede olmayan bir parametre eklenebilir.
Yani hukukçunun kararı kod değişikliği değil, yapılandırma satırı olur.

### ⚠️ `utm_term` — hukukçunun bilmesi gereken tek istisna

Yukarıda "arama terimi saklanmaz" dedik ve bu **organik arama için
kesinlikle doğrudur.** Ücretli reklamda bir istisna vardır:

`utm_term` normalde reklam verenin **satın aldığı anahtar kelimeyi**
taşır (kullanıcının yazdığını değil). Ancak reklam veren, Google Ads'te
`{keyword}` yerine `{searchterm}` kullanacak şekilde ayarlarsa,
**kullanıcının gerçekten yazdığı arama** bu alana düşebilir. Bu, reklam
verenin yapılandırmasına bağlıdır ve bizim kontrolümüzde değildir.

Bu nedenle `utm_term` tek satırlık bir ayarla tamamen kapatılabilir:

```toml
[campaign]
drop_params = ["utm_term"]
```

Kapatıldığında sütun boş kalır ve değer saklanan sorgu dizesine de
girmez.

### ⚠️ Reklam tıklama kimlikleri (`gclid` / `fbclid` / `msclkid`)

Bunlar diğer kampanya parametrelerinden **farklıdır** ve ayrı
değerlendirilmelidir. `utm_*` değerleri sabit etiketlerdir — `instagram`
yazar, herkeste aynıdır. Tıklama kimlikleri ise **her tıklamada
benzersizdir**; reklam platformu üretir.

Biz onları bir kişiye çeviremeyiz — o eşleştirme yalnızca
Google/Meta/Microsoft'un kendi sisteminde vardır. Ama benzersiz oldukları
için "kişisel veri değil" demek doğru olmaz.

**Varsayılan davranış: ham tıklama kimliği SAKLANMAZ.** Saklanan tek şey
`click_source` sütunudur — yani "bir Google reklamından tıklanmış"
bilgisi. Bu, analiz açısından değerli olan kısımdır ve hiç kimseyi
tanımlamaz.

Gerekçe: ham kimliğin tek meşru kullanımı, dönüşümleri reklam
platformuna geri yüklemektir — bu proje bunu yapmaz. Hiçbir şeyin
tüketmediği benzersiz bir tanımlayıcıyı saklamak, envanterde
açıklanması gereken ve gerekçelendirilemeyen bir şeydir.

Müşteri isterse `campaign.store_click_ids = true` ile açılabilir; o
durumda `click_id` sütunu dolar ve bu belgede ayrıca beyan edilmelidir.

---

## 3. `panel_*` — Müşterinin kendi personeline ait veriler

Bu tablolar **site ziyaretçilerine ait değildir.** Yönetim paneline
giren kişilere (müşterinin çalışanları) aittir. Ayrı bir ilgili kişi
kategorisidir.

| Tablo | Alanlar |
|---|---|
| `panel_users` | E-posta, görünen ad, **şifrenin argon2id özeti** (şifrenin kendisi hiçbir zaman saklanmaz), iki faktörlü kimlik doğrulama gizli anahtarı, rol bayrakları, oluşturma ve son giriş zamanı |
| `panel_sessions` | Oturum jetonu, süre bitişi |
| `panel_site_members` | Hangi kullanıcı hangi siteye hangi yetkiyle erişiyor, kimin verdiği, ve varsa **erişimin bitiş tarihi** |
| `panel_audit_log` | Kim, ne zaman, ne yaptı; **eylemi yapanın IP'si ve User-Agent'ı** |
| `panel_api_tokens` | Jeton adı, **yalnızca SHA-256 özeti**, yetki kapsamı, son kullanım |
| `panel_login_attempts` | Denenen e-posta, **IP adresi**, başarılı mı — kaba kuvvet saldırısını sınırlamak için |
| `panel_dev_access` | Geliştirici erişim talepleri ve onayları |
| `panel_member_invites` | Davet edilen **e-posta adresi**, hangi siteye hangi rolle, kimin davet ettiği, bağlantının geçerlilik süresi, kabul edilince erişimin kaç gün süreceği, ve kullanıldıysa kullanım anı ile **IP adresi**. Bağlantının kendisi değil, yalnızca SHA-256 özeti saklanır |

`panel_login_attempts` tablosu, **var olmayan hesaplar için de** kayıt
tutar. Gerekçe: saldırı desenini görebilmek. Bu, güvenlik amaçlı meşru
menfaat değerlendirmesi gerektirebilir.

`panel_member_invites` tablosu, **hesabı hiç açılmamış bir kişinin
e-posta adresini** taşıyabilir: davet edilen ama daveti hiç kabul etmemiş
biri. Süre dolduğunda satır kendiliğinden geçersizleşir (varsayılan yedi
gün) ve davet eden kişi istediği anda geri alabilir. Kabul edilmemiş bir
davet, o kişi hakkında adresinden başka hiçbir şey içermez.

`panel_site_members` satırı **boş bırakılabilir bir bitiş tarihi**
taşıyabilir. Boşken erişim süresizdir, yani bugüne kadarki davranış.
Doluysa erişim o tarihte kendiliğinden biter: bitiş, erişimi hesaplayan
her sorgunun kendi içinde süzülür, bir temizlik işi tarafından değil.
Satır silinmez — kimin ne zamana kadar erişimi olduğu, sonradan
sorulabilecek bir sorudur — ama erişim vermez. Sahiplik rolüne bitiş
tarihi konulamaz; veritabanı kısıtı buna izin vermez.

---

## 4. `ip_country_ranges`, `ip_asn_ranges` — Referans verisi

Kişisel veri **içermez.** Hangi IP bloğunun hangi ülkeye/operatöre ait
olduğunu gösteren, kamuya açık tahsis tablolarıdır. Ziyaretçiyle ilgisi
yoktur.

Bu tablolar internetten indirilebilir veya yerel bir dosyadan
yüklenebilir (`local_csv_path`). **İndirme yönü tek taraflıdır** —
sunucudan dışarıya ziyaretçi verisi gitmez, yalnızca genel tablo
indirilir. Yerel dosya kullanıldığında sistem **hiç ağ isteği yapmaz**
(bu, otomatik testle doğrulanmıştır).

---

## 5. Kesinlikle TOPLANMAYAN — hukukçunun sorabileceği başlıklar

Aşağıdakiler için şemada sütun **yoktur**; yani teknik olarak
saklanamazlar:

- **Çerez.** Sistem hiçbir çerez yazmaz ve hiçbir çerez okumaz.
  (`localStorage`'a yalnızca bir *devre dışı bırakma* bayrağı için
  **okuma** yapılır, yazma yapılmaz.)
- **Parmak izi çıkarma teknikleri:** canvas, WebGL, yazı tipi listesi,
  ses (audio) parmak izi, cihaz sensörleri — hiçbiri yok.
- **Davranış izleme:** fare hareketi, kaydırma, tuş vuruşu, tıklama
  ısı haritası, ekran kaydı (session replay) — hiçbiri yok.
- **Form içeriği.** Ziyaretçinin doldurduğu hiçbir alan okunmaz.
- **Ad, soyad, e-posta, telefon, adres, TCKN, ödeme bilgisi** — sistemin
  bu alanlar için hiçbir sütunu yoktur.
- **Ham sorgu dizesi** (Bölüm 2'ye bakınız).
- **Organik arama terimleri** (yönlendiren sorgu dizesi tamamen atılır).
  Ücretli reklamdaki `utm_term` istisnası için Bölüm 2'ye bakınız.
- **Ham reklam tıklama kimliği** — varsayılan olarak saklanmaz; yalnızca
  hangi reklam ağı olduğu (`click_source`) saklanır.
- **Siteler arası takip** — yapısal olarak imkânsız (Bölüm 2, madde 3).
- **Üçüncü taraflara aktarım.** Reklam ağı, veri simsarı, harici
  analitik servisi bulunmaz.
- **Kullanıcı hesabı eşleştirmesi.** Sistem, ziyaretçinin sitedeki
  hesabıyla hiçbir bağ kuramaz; siteden böyle bir bilgi almaz.

---

## 6. Saklama süresi

> **Düzeltme (2026-09-08).** Bu bölüm daha önce "sistemde saklama süresi
> politikası yoktur, her iki tablo da süresiz büyür" diyordu. **O cümle
> artık doğru değil.** Politika yazıldı, kuruldu ve kurulu sistemde
> ölçüldü; aşağısı bugünkü durumdur. Eski hâliyle okuyan bir
> değerlendirme, sistemi olduğundan kötü anlatır.

**Bugünkü durum.** Her iki analitik tablosunun da saklama süresi vardır,
**varsayılan 90 gün**, ve süresi dolan veri otomatik silinir. Sınırlar:
en az **1 gün**, en çok **730 gün** (iki yıl). Aralık dışında bir değer
yazılandırma dosyası okunurken reddedilir; servis o değerle başlamaz.

**Nerede ayarlanır — ve neden panelde değil.** Servisin kendi
yapılandırma dosyasında, `[retention] days`. Panelde *değildir*, ve bu
bilinçli: bu, projedeki tek **hukuki ağırlıklı** ayardır. Panelde
dursaydı, sızmış bir parola bir müşterinin saklama süresini HTTP
üzerinden değiştirebilirdi. Bugün değiştirmek için sunucuya erişmek
gerekir. Aynı dosyada **site başına** kısaltma da yazılabilir; "bu
müşteri otuz gün istedi" gerçek bir taleptir.

**Nasıl siliniyor.** İki yol, ne için iyi olduklarına göre ayrılmış:

- **Zaman dilimi düşürme (chunk drop).** TimescaleDB hipertabloyu
  zaman aralıklı dilimler hâlinde tutar; bir dilimin tamamını düşürmek
  bir dosyayı bağlantısından koparmaktır. Dağıtımdaki **en uzun**
  saklama süresi bu yolla uygulanır — ucuzdur ve hiçbir sitenin hâlâ
  istediği veriyi kaldıramaz.
- **Satır silme.** Yalnız dağıtımdan **daha kısa** süre isteyen siteler
  için, yalnız o sitenin satırlarında. Bir dilim bütün sitelerin o
  zaman aralığındaki satırlarını taşır, dolayısıyla "A sitesinin 30
  günden eski satırları" dilim düşürerek ifade edilemez.

Tersini yapmak — politikayı en kısa değere kurmak — daha uzun süre
isteyen her sitenin verisini sessizce yok ederdi, ve bu özelliğin
çalışıyor gibi görünmesiyle aynı şey olurdu.

**Kim uyguluyor.** Veriyi yazan servisin kendisi: collector
`traffic_snapshots` için, beacon `beacon_events` için. Açılışta bir kez,
sonra saatte bir. Uygulama dört adet `SECURITY DEFINER` veritabanı
işlevi üzerinden yapılır; kurulu bir sistemde hipertabloların sahibi
süper kullanıcıdır ve TimescaleDB yetkiye değil **sahipliğe** bakar —
bu sarmalayıcılar olmadan özellik yalnız geliştirme veritabanında
çalışıyordu ve bunu uçtan uca kurulum ölçtü.

**Yedekler de kapsamda.** Saklama süresi analitik satırlarını yaşına
göre siler; ondan *önce* alınmış bir yedek o satırları hâlâ taşır. Yedek
dizini bu yüzden ayrı bir yaş sınırına bağlandı
(`upgrader.toml` → `[backup] keep_days`) ve panel bunu yazıyor. Sınır
konmazsa yedekler süresiz saklanır — o zaman **panel bunu da yazar**, ki
"süresiz saklanıyor" en çok orada söylenmelidir.

Hukukçuya sorulacak asıl soru şu: *ilgili mevzuat ve müşterinin
faaliyeti açısından uygun saklama süresi nedir?* 90 gün teknik bir
başlangıç önerisidir, hukuki bir tespit değildir. Tavanın iki yıl
olmasının gerekçesi de teknik değil: kişisel veri amacın gerektirdiği
süre kadar tutulmalıdır, ve tavanı on yıl olan bir ürün kimsenin
savunamayacağı bir kurulumu davet eder.

**Kendiliğinden gerçekleşen ek bir sınır:** `visitor_id`, tuz döndüğü
için 24 saat sonra zaten yeniden bağlanamaz hâle gelir. Bu, saklama
süresinden bağımsız olarak işleyen teknik bir kısıttır.

---

## 7. Ziyaretçiye dönük açıklama yüzeyi — yazıldı

> **Not (2026-09-08):** Bu bölüm daha önce "planlanan — henüz
> uygulanmamış" başlığını taşıyordu. **Yüzey yazıldı, kuruldu ve gerçek
> tarayıcıda ölçüldü.** Aynı bölümde "planlanan" diye duran **IP saklama
> modu** da 2026-08-16'da yazılmıştı; ayrıntısı Bölüm 1.5'te. Hâlâ
> yazılmamış olanlar bu bölümün sonunda, ayrı başlık altında.

**Ne var.** Beacon'ın kendi yol öneki altında iki uç — varsayılan önek
`/_ca`:

| Uç | Ne döner |
|---|---|
| `GET /_ca/privacy` | JSON: hangi alanların saklandığı, IP modu, tuzun ne sıklıkta döndüğü, varsa politika sayfası ve iletişim adresi |
| `GET /_ca/privacy.html` | Aynı bilginin Türkçe sayfası, bağlantı verilebilir |

Üçüncü bir kullanım da var, ayrı bir uç değil: sitenin kendi gizlilik
sayfasına `<div data-crucible-privacy></div>` konursa, snippet oraya
kum havuzuna alınmış bir çerçeve içinde aynı sayfayı çizer. Çizmeden
önce JSON ucuna sorar; 200 gelmezse **hiçbir şey çizmez** — kapalı bir
kurulumda müşterinin sayfasının ortasında tarayıcının 404'ü görünmesin
diye.

**Açıklama metni ayardan türetilir, kopyalanmaz.** Ziyaretçiye
gösterilen metin, `privacy.ip_storage`'ı **verinin yazıldığı yerin
okuduğu aynı canlı kaynaktan** okur. Yani ayar değiştirildiğinde
açıklama bir sonraki istekte kendiliğinden değişir; güncellenmesi
unutulabilecek ikinci bir metin kopyası yoktur. Bu, belgeye yazılmış bir
niyet değil, teste bağlanmış bir değişmez: metnin tek bir yerde
durduğunu tutan bir test var.

**Operatörün ekleyebileceği iki şey.** Panelden (Ayarlar → Gizlilik):
kendi gizlilik politikası sayfasının adresi, ve talepler için bir
iletişim adresi. İkisi de **iki uçta birden** denetlenir — panel
yazarken reddeder (müşteriye gösterecek bir cümlesi vardır), beacon
basarken reddeder (kimseye gösterecek cümlesi yoktur, sessizce düşürür).
İkincisi gereksiz değil: satır elle düzenlenebilir, eski bir sürümden
gelebilir, kontrol yokken alınmış bir yedekten dönebilir. Reddedilenler
arasında `javascript:` ve `data:` şemaları, kullanıcı adı/parola taşıyan
adresler, satır sonu ve denetim karakterleri var.

**Müşteri bu yüzeyi kapatabilir** (`privacy.visitor_surface`, varsayılan
**açık**). Kapattığında iki uç da 404 verir ve gömülü blok hiçbir şey
çizmez. Yükümlülük ortadan kalkmaz, **kendisine ve elle** geçer: panel
bunu kapatma anahtarının yanında açıkça yazar — "burada ne toplanıyor"
sorusuna kendi sayfanızda kendiniz cevap verirsiniz, ve o metin ayar
değiştiğinde kendiliğinden güncellenmez.

**Devre dışı bırakma ziyaretçinin elinde, ve anahtardan bağımsız.**
`crucible.optOut()` çağrısı beacon'ı o tarayıcıda susturur,
`crucible.optIn()` geri açar, `crucible.status()` hangi durumda
olunduğunu söyler. Altta yatan bayrak `localStorage`'daki
`crucible.disabled`; elle ayarlamak da hep çalıştığı gibi çalışıyor. Bu
üçü **yüzey kapalıyken de çalışır** — müşterinin bir açıklama sayfası
sunmaması, ziyaretçinin çıkabilmesini kaldırmaz.

Bunlar bizim çizdiğimiz bir bant değil, **çağrı**: sitede zaten bir çerez
bandı ya da onay platformu var, ve yanına ikinci bir kutu koymak kimseye
yaramaz. Müşteri kendi anahtarını bu üçüne bağlar.

`optOut()` ve `optIn()` seçimin **saklanıp saklanamadığını** döndürüyor —
kum havuzundaki bir çerçevede ya da site verisini engelleyen bir
tarayıcıda `false`. O durumda seçim yine de o sayfanın ömrü boyunca
uygulanıyor: ziyaretçi şimdi istedi.

*(Bayrak okuma özelliği baştan beri vardı. Yaptığı şey, `window.crucible`
tanımlanmadan bütün betikten çıkmaktı — yani devre dışı bırakmış bir
ziyaretçinin çağırabileceği bir `optIn()` yoktu ve durumu soran bir onay
bandı hata alıyordu. Girilebilen ve çıkılamayan bir özellik.)*

### 7.1 Silme talebi kanalı — **yazılmadı**, ve gerekçesi ölçüldü

> **Düzeltme (2026-09-02) — hukukçunun bilmesi gereken.** Bu maddede
> daha önce bir **silme talebi** kanalı tarif ediliyordu: talebin kimlik
> iddiasıyla değil, isteğin kendisinden türetilen `visitor_id` ile
> işleneceği, dolayısıyla kimsenin başkasının verisinin silinmesini
> isteyemeyeceği yazıyordu. **Bu tasarım yazılmadan önce ölçüldü ve
> kaldırıldı.**
>
> Gerekçesi: `visitor_id`, Bölüm 2'de tarif edildiği gibi
> HMAC(günün tuzu; site + ağ + user agent). Türkiye'deki mobil
> operatörlerin yaygın olarak kullandığı CGNAT altında bu üç girdinin
> üçü de iki farklı kişi için **aynı olabiliyor** — ağ tanımı gereği
> ortak, user agent aynı cihaz modeli ve tarayıcı sürümünde tek bir
> katara çıkıyor. Yani böyle bir silme kanalı, engellemeyi vaat ettiği
> şeyi üretirdi: bir ziyaretçi, hiçbir kimlik taklit etmeden, başka
> kişilerin kayıtlarını sildirebilirdi.
>
> Alternatifi — ziyaretçinin tarayıcısına, taleplerini
> ilişkilendirebileceğimiz **kalıcı bir tanımlayıcı** yazmak — sistemin
> bugün hiç oluşturmadığı kişisel veriyi oluşturmak, ve bugün gerekmeyen
> bir çerez onayını gerektirmek olurdu.
>
> **Sonuç, dürüstçe:** bu sistemde silinecek bir "kişiye ait kayıt
> kümesi" teknik olarak oluşmuyor. Silme kanalı yerine **açıklama ve
> kapatma** yazıldı. Kayıtlar saklama süresi sonunda kendiliğinden
> siliniyor (Bölüm 6), ve zaten hiçbiri gün sınırının ötesinde bir
> kişiyle ilişkilendirilemiyor (Bölüm 2, "`visitor_id` — takma kimliğin
> tam tanımı").

### 7.2 Hâlâ yazılmamış olanlar

**IP modu değiştirildiğinde geçmiş satırlar olduğu gibi kalır.**
`privacy.ip_storage` maskeliye çevrildiğinde, o andan sonra yazılan
satırlar maskeli olur; **daha önce yazılmış satırlar değişmez.** Bugün
sistem bunu ne yapıyor ne de uyarıyor. Planlanan: değişimin anında
panelin bunu yazması, ve isteyen kurulumun geçmişi onaylı bir işlemle
temizleyebilmesi. Hukuki değerlendirme açısından önemli olduğu için
burada, "yazıldı" listesinde değil.

**Açıklama sayfası site başına özelleştirilemez.** Politika adresi ve
iletişim adresi kurulum geneline yazılır, site başına değil — bu bilinçli
bir karar: metni basan servis küresel ayarları okur, site başına bir
adres onu gösteren servisin göremeyeceği bir ayar olurdu. Tek kurulumda
birden çok müşterinin sitesi varsa, tek bir politika adresi gösterilir.

---

## 8. Geliştirici/tedarikçi erişimi — işleyen sıfatı doğabilir

Hukukçunun mutlaka bilmesi gereken başlık.

**Kurulum aşaması.** Yazılımı kuran taraf (Crucible), müşteri henüz
hesap oluşturmamışken panele girebilir. Kurulum bittikten ve müşteri
kendi hesabını oluşturduktan sonra **bu erişim kendiliğinden düşer.**
Sonrasında her erişim talebi, müşterinin panelden **açıkça
onaylamasını** gerektirir; onay tek seferliktir ve süreye bağlıdır.

**Destek erişimi jetonu.** Müşteri isterse, tedarikçinin analitiği
uzaktan **okumasına** izin veren bir jeton oluşturulabilir. Özellikleri:

- Yalnızca **okuma**. Veritabanı rolü seviyesinde `SELECT` dışında
  hiçbir yetkisi yoktur — bu, uygulama kodundaki bir kontrol değil,
  veritabanının zorladığı bir kısıttır.
- Müşterinin panelinde **açıkça listelenir**, gizlenmez.
- Müşteri tarafından **tek tıkla iptal edilebilir.**
- **Son kullanım zamanı gösterilir.**

Bu, tedarikçiye müşterinin analitik verisine erişim sağladığı için,
muhtemelen bir **veri işleyen sözleşmesi** gerektirir. Jeton
oluşturulmazsa tedarikçinin hiçbir erişimi olmaz.

### 8.5 Hukuki ağırlıklı ayarların ayrı şifreye bağlanması

Yukarıdaki erişim kuralları "panele kim girebilir" sorusunu
cevaplıyor. Ondan ayrı bir soru daha var: **panele girmiş biri, saklanan
kişisel verinin kapsamını tek başına değiştirebilmeli mi?**

Cevabımız hayır. Aşağıdaki yedi ayarın her değişikliği, panel
şifresinden **başka** bir şifre ister:

| Ayar | Neye karar verir |
|---|---|
| `privacy.ip_storage` | IP tam mı maskeli mi saklanır (Bölüm 1.5) |
| `analytics.retention_days` | Ziyaret kayıtlarının saklama süresi |
| `logs.retention_days` | Erişim kayıtlarının (IP içerir) saklama süresi |
| `logs.important_retention_days` | Güvenlik/kimlik doğrulama kayıtlarının süresi |
| `campaign.drop_params` | Hangi kampanya parametresi hiç yazılmaz (`utm_term` dâhil) |
| `campaign.extra_params` | İçeriğini denetlemediğimiz ek alanların saklanması |
| `campaign.store_click_ids` | Ham reklam tıklama kimliğinin saklanması |

Bu şifrenin özellikleri, hukuki değerlendirmeyi ilgilendirdiği ölçüde:

- **Veritabanında değil, sunucudaki yapılandırma dosyasında durur** ve
  orada da yalnızca **hash'lenmiş** hâliyle bulunur. Düz metin hiçbir
  yerde saklanmaz; hash'ten şifre geri türetilemez.
- **Her değişiklikte yeniden sorulur.** Oturum açık kalmaz, "bir kez
  girdim, on dakika geçerli" yoktur. Bu bir tercih değil, kodun yapısal
  özelliği: doğrulamanın ürettiği yetki saniyeler içinde geçersizleşir
  ve yalnızca o tek ayar için geçerlidir.
- **Her deneme — başarılı da başarısız da — silinemez denetim kaydına
  yazılır** (`panel_audit_log`), kim denedi, hangi ayar için, hangi
  adresten.
- **Şifre tanımlı değilse bu ayarlar hiç değiştirilemez** ve
  varsayılanlarında (en korumacı değerlerde) kalır.

Pratik sonucu: müşterinin personeli, panele tam yetkiyle girmiş olsa
bile, saklanan kişisel verinin kapsamını kendi başına genişletemez.
Bunun için sunucuya erişimi olan tarafın (Crucible) katılımı gerekir —
ki bu da yukarıdaki işleyen/sorumlu tartışmasının kayda geçen bir
parçasıdır.

**Müşteri de bu ayarlara erişir — ama değiştiremez.** Bu ayrım
kasıtlıdır ve gerekçesi ikilidir: (1) bunlar normalde doğrudan
sunucudaki yapılandırmadan verilen ayarlardır, panelden değiştirilmesi
sistemin işleyişini bozabilir; (2) bir kısmı — Bölüm 8.5'teki yedi
ayar — hangi kişisel verinin ne kadar süre saklandığına karar verir.
Müşteri değeri ve gerekçesini görür, kilidi ve nedenini okur;
değişiklik gerekiyorsa geliştiriciye iletir ve geliştirici sunucuya
bağlanıp yapar. Bu, veri sorumlusu/işleyen ayrımı açısından kayda
değer olabilir: **kapsamı genişletme kararı fiilen geliştiricinin
katılımını gerektirir.**

**Ama gizlenmiyor.** Bu, hukuki değerlendirme açısından önemli olabilir:
müşteri bu yedi ayarın **hepsini, güncel değerleriyle ve gerekçeleriyle
birlikte** kendi panelinde görür. Göremediği tek şey değiştirme
kontrolüdür — kilit işareti ve nedeni yazılıdır. Yani müşteri, kendi
sisteminde hangi kişisel verinin ne kadar süre saklandığını her an
doğrulayabilir; yalnızca tek başına değiştiremez. Ayarı gizlemek,
müşteriyi kendi kurulumunun davranışını açıklayamaz duruma
düşürürdü — aydınlatma yükümlülüğü açısından da istemediğimiz bir
sonuç.

---

## 9. Güvenlik önlemleri — teknik ve idari tedbirler başlığı için

- **Şifreler** argon2id ile özetlenir (OWASP parametreleri). Şifrenin
  kendisi hiçbir zaman saklanmaz, günlüklenmez.
- **İki faktörlü kimlik doğrulama** (TOTP) desteklenir; kullanılmış bir
  kodun tekrar kullanılması engellenir.
- **Yetki ayrımı:** yazılımın dört bileşeni **dört ayrı veritabanı
  kullanıcısıyla** çalışır. Okuma API'si hiçbir şey yazamaz; beacon
  yalnızca tek bir tabloya ekleme yapabilir; panel analitik tablolara
  **hiç erişemez.** Bir bileşenin ele geçirilmesi diğerlerine erişim
  vermez.
- **SQL enjeksiyonu:** tüm sorgular bağlı parametre kullanır; kullanıcı
  girdisi hiçbir zaman SQL metnine dönüşmez.
- **Denetim kaydı** yalnızca eklenebilir; yazılım kendi geçmişini
  değiştiremez (veritabanı yetkisiyle zorlanır).
- **Giriş denemeleri sınırlanır** ve kaydedilir.
- **Oturum yönetimi** kanıtlanmış bir kütüphaneyle yapılır; oturumlar
  sunucu tarafında saklanır, dolayısıyla anında iptal edilebilir.

---

## 10. Hukukçuya sorulmasını önerdiğimiz sorular

1. ~~IP adresi **tam mı, maskeli mi** saklanmalı?~~ **Cevaplandı:
   maskeli.** Yazıldı ve varsayılan yapıldı (Bölüm 1.5). Kalan soru
   dar: belirli bir müşteri için tam adres gerekçelendirilebilir mi, ve
   gerekçelendirilirse bu neye dayanır?
2. **Saklama süresi** ne olmalı? Varsayılan öneri 90 gün.
3. **Açık rıza gerekli mi?** Sistem çerez kullanmadığı, kalıcı
   tanımlayıcı üretmediği ve siteler arası takip yapamadığı için meşru
   menfaat değerlendirmesi mümkün olabilir — bu bir hukuki tespittir.
4. **Aydınlatma metninde** hangi ifadeler yer almalı? Bu belgedeki alan
   listeleri doğrudan kullanılabilir.
5. **Veri işleyen sözleşmesi** gerekli mi? (Bölüm 8 — destek erişimi.)
6. **Yurt dışına aktarım** söz konusu mu? Yazılım veriyi aktarmaz, ancak
   müşterinin sunucusu yurt dışındaysa bu ayrıca değerlendirilmelidir.
7. `panel_login_attempts` tablosunun **var olmayan hesaplar için de**
   kayıt tutması (güvenlik amaçlı) nasıl değerlendirilmeli?
8. Ziyaretçinin **silme talebi**, teknik olarak yalnız günün kayıtlarını
   kapsayabiliyor (eskisi zaten ilişkilendirilemiyor). Bu, mevzuat
   açısından yeterli bir karşılık mı?
9. **`utm_term` saklanmalı mı?** Ücretli reklamda kullanıcının gerçek
   arama terimini taşıyabiliyor (Bölüm 2). Tek satırlık ayarla
   kapatılabilir.
10. **Ham reklam tıklama kimliği** varsayılan olarak saklanmıyor. Bu
    tercih doğru mu, yoksa müşterinin reklam ölçümü için gerekli mi?

---

## Ek: Doğrulama

Bu belgedeki her alan listesi şu dosyalardan çıkarılmıştır ve oradan
doğrulanabilir:

| Bölüm | Kaynak dosya |
|---|---|
| 1 | `internal/storage/schema.sql` |
| 1.5 | `internal/privacy/ip.go`, `internal/storage/row.go`, `internal/beacon/server.go` |
| 2 | `internal/beacon/schema.sql`, `internal/beacon/event.go`, `internal/beacon/visitor.go`, `internal/beacon/beacon.js` |
| 3 | `internal/panel/schema.sql`, `internal/panel/memberinvite.go` |
| 4 | `internal/asnlookup/schema.sql` |
| 6 | `internal/retention/retention.go`, `release/sql/grants.sql` |
| 7 | `internal/beacon/privacy.go`, `internal/privacy/notice.go`, `internal/privacy/policy.go`, `internal/beacon/beacon.js` |
| 8.5 | `internal/devgate/devgate.go`, `internal/panel/settings.go` (`GuardedKeys`) |

Bölüm 1.5, 6, 7 ve 8.5'teki iddialar ayrıca **çalışan sistemde**
doğrulandı, belgeden değil: gerçek bir tarayıcı gerçek beacon sürecine
olay gönderdi ve veritabanına düşen değer okundu; kurulu paket gerçek
bir TimescaleDB'ye kuruldu ve saklama işinin gerçekten tanımlandığı
veritabanından okundu; açıklama sayfası gerçek Chromium'da çizildi ve
anahtar kapatılınca çizilmediği ölçüldü; şifre kapısı gerçek bir form
üzerinden, gerçek bir tarayıcıyla denendi. Testleri:
`internal/privacy/ip_test.go`, `internal/beacon/server_test.go`,
`internal/beacon/privacy_browser_test.go`,
`internal/retention/integration_test.go`, `e2e/e2e_test.go`,
`internal/panel/settings_integration_test.go`,
`internal/panel/devpassword_browser_test.go`.

**Bu tablo denetleniyor.** `internal/docs` içindeki bir test, buradaki
her dosya yolunun depoda gerçekten var olduğunu ve belgede geçen her
ayar adının panelin kayıt defterinde tanımlı olduğunu tutuyor. Yeniden
adlandırılmış bir dosya ya da kaldırılmış bir ayar, bu belgeyi sessizce
yanlış bırakmak yerine kapıyı kırmızıya çevirir.

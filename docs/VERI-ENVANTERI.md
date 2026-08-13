# Crucible Analytic — Veri Envanteri

**Hukuki değerlendirme için hazırlanmış teknik envanterdir. Hukuki metin
değildir.**

Bu belge, sistemin **fiilen hangi alanları veritabanına yazdığını**
listeler. Her satır, kaynak koddaki şema dosyalarından birebir
çıkarılmıştır (`internal/storage/schema.sql`,
`internal/beacon/schema.sql`, `internal/panel/schema.sql`), tarif veya
tahmin değildir.

Son güncelleme: 2026-08-13

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
| `ip` | IP adresi | **Ziyaretçinin IP adresi** | **Evet** |
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
| `referrer_host` | metin | Gelinen sitenin alan adı (`google.com`) | Hayır |
| `referrer_path` | metin | Gelinen sitedeki yol — **sorgu dizesi tamamen atılır** | Hayır |
| `ip` | IP adresi | **Ziyaretçinin IP adresi** (iki tabloyu birleştiren anahtar) | **Evet** |
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
Google'da arama yapıp siteye geldiğinde, **arama terimi saklanmaz**;
yalnızca `google.com` saklanır.

---

## 3. `panel_*` — Müşterinin kendi personeline ait veriler

Bu tablolar **site ziyaretçilerine ait değildir.** Yönetim paneline
giren kişilere (müşterinin çalışanları) aittir. Ayrı bir ilgili kişi
kategorisidir.

| Tablo | Alanlar |
|---|---|
| `panel_users` | E-posta, görünen ad, **şifrenin argon2id özeti** (şifrenin kendisi hiçbir zaman saklanmaz), iki faktörlü kimlik doğrulama gizli anahtarı, rol bayrakları, oluşturma ve son giriş zamanı |
| `panel_sessions` | Oturum jetonu, süre bitişi |
| `panel_site_members` | Hangi kullanıcı hangi siteye hangi yetkiyle erişiyor |
| `panel_audit_log` | Kim, ne zaman, ne yaptı; **eylemi yapanın IP'si ve User-Agent'ı** |
| `panel_api_tokens` | Jeton adı, **yalnızca SHA-256 özeti**, yetki kapsamı, son kullanım |
| `panel_login_attempts` | Denenen e-posta, **IP adresi**, başarılı mı — kaba kuvvet saldırısını sınırlamak için |
| `panel_dev_access` | Geliştirici erişim talepleri ve onayları |

`panel_login_attempts` tablosu, **var olmayan hesaplar için de** kayıt
tutar. Gerekçe: saldırı desenini görebilmek. Bu, güvenlik amaçlı meşru
menfaat değerlendirmesi gerektirebilir.

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
- **Arama terimleri** (yönlendiren sorgu dizesi tamamen atılır).
- **Siteler arası takip** — yapısal olarak imkânsız (Bölüm 2, madde 3).
- **Üçüncü taraflara aktarım.** Reklam ağı, veri simsarı, harici
  analitik servisi bulunmaz.
- **Kullanıcı hesabı eşleştirmesi.** Sistem, ziyaretçinin sitedeki
  hesabıyla hiçbir bağ kuramaz; siteden böyle bir bilgi almaz.

---

## 6. Saklama süresi — **dikkat: mevcut durum ile planlanan farklı**

**Bugünkü durum, dürüstçe:** sistemde **saklama süresi politikası
yoktur.** Her iki analitik tablosu da süresiz büyür. Bu bilinen bir
eksikliktir ve giderilmek üzere planlanmıştır.

**Planlanan durum:** her tablo için ayarlanabilir saklama süresi,
**varsayılan 90 gün**, süresi dolan veriler otomatik silinir.

Hukukçuya sorulacak asıl soru bu olabilir: *ilgili mevzuat ve müşterinin
faaliyeti açısından uygun saklama süresi nedir?* 90 gün teknik bir
başlangıç önerisidir, hukuki bir tespit değildir.

**Kendiliğinden gerçekleşen ek bir sınır:** `visitor_id`, tuz döndüğü
için 24 saat sonra zaten yeniden bağlanamaz hâle gelir. Bu, saklama
süresinden bağımsız olarak işleyen teknik bir kısıttır.

---

## 7. Planlanan — henüz uygulanmamış, ancak hukukçunun bilmesi yararlı

Bunlar **yazılmadı**, kararlaştırıldı:

**IP saklama modu seçeneği.** İki değerli bir ayar:
- `tam` — bugünkü hâl, IP adresi tam saklanır.
- `maskeli` — IPv4'ün son okteti sıfırlanır, IPv6 /64'e kırpılır.

Müşteri "KVKK ile uğraşmak istemiyorum" derse maskeli mod seçilir.
Teknik bedeli: iki veri kaynağını birleştiren analiz zayıflar.

**Ziyaretçiye dönük gizlilik kartı.** Sitenin gizlilik/çerez sayfasına
gömülebilen küçük bir bileşen. Ziyaretçi kendisi hakkında ne
tutulduğunu görür ve **silinmesini talep edebilir.** Talep, kimlik
iddiasıyla değil, isteğin kendisinden türetilen kimlikle işlenir —
dolayısıyla kimse başkasının verisinin silinmesini isteyemez.

Bu bileşenin kapsamı, teknik gerçeği yansıtacak biçimde açıkça
yazılacaktır: **bugünkü kayıtlar silinir**; daha eski kayıtlar zaten
kişiyle ilişkilendirilemediği için bulunamaz ve saklama süresi sonunda
kendiliğinden silinir.

**Devre dışı bırakma zaten mevcuttur.** Ziyaretçi kendi tarayıcısında
`localStorage.setItem('crucible.disabled', '1')` ayarladığında beacon
hiçbir veri göndermez. Bu özellik **bugün kodda vardır** ve
çalışmaktadır; eksik olan, bunu ziyaretçiye sunan arayüzdür.

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

1. IP adresi **tam mı, maskeli mi** saklanmalı? (Maskeli mod ürünün bir
   analiz yeteneğini zayıflatır — bu bir maliyet/uyum dengesidir.)
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

---

## Ek: Doğrulama

Bu belgedeki her alan listesi şu dosyalardan çıkarılmıştır ve oradan
doğrulanabilir:

| Bölüm | Kaynak dosya |
|---|---|
| 1 | `internal/storage/schema.sql` |
| 2 | `internal/beacon/schema.sql`, `internal/beacon/event.go`, `internal/beacon/visitor.go`, `internal/beacon/beacon.js` |
| 3 | `internal/panel/schema.sql` |
| 4 | `internal/asnlookup/schema.sql` |

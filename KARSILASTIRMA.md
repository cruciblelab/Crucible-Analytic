# Crucible Analytic ile Umami karşılaştırması

Bu belge, sahibin isteğiyle hazırlandı: *"Umami'yle bir karşılaştırma
tablosu yapsak tablo nasıl olurdu, gerçekçi tarafsız hazırlanacak."*

Karşılaştırılan sürümler: **Crucible Analytic v0.23.0** (bu depo) ve
**Umami v3.3.1** (MIT, `github.com/umami-software/umami`, commit
`ca661c7`). Ölçümler 2026-09-10'da, 4 CPU / 16 GB bir konteynerde.

---

## 0. Bu tabloyu okumadan önce

Bir karşılaştırma tablosu, hazırlayanın kendi ürünü hakkında olduğunda
varsayılan olarak taraflıdır. Bu belgenin taraflılığını azaltmak için üç
kural var ve hepsi belgenin içinde uygulanmış:

1. **Ölçülen, kaynağından doğrulanan ve bakılmayan ayrı ayrı
   işaretlenmiştir.** "Ölçüldü" yazan her satırın nasıl ölçüldüğü
   `NOTES.md`'de yazılı. Bakmadığım hiçbir şey hakkında iddia yok.
2. **Umami'nin önde olduğu bölüm önce gelir** (§1 ve §2). Bir tablo
   yalnız hazırlayanın kazandığı satırları içeriyorsa, o tablo bir
   ölçüm değil bir broşürdür.
3. **Her hız iddiasının yanında bedeli yazılıdır.** Hız bedava gelmiyor;
   neyin karşılığında geldiği söylenmezse sayı yanıltıcı olur.

Ve kapsam sınırı: ölçülen tek yol **olay alma** yoludur. Umami'nin okuma
tarafı, panosunun hızı, raporlarının maliyeti **ölçülmedi.**

---

## 1. Umami'nin açık ara önde olduğu yerler

| Konu | Umami v3.3.1 | Crucible Analytic v0.23.0 | Kaynak |
|---|---|---|---|
| **Olgunluk** | Arkasında bir şirket (Umami Software, Inc., telif 2022), büyük bir topluluk, barındırılmış bulut seçeneği | **1.0 öncesi** (v0.23.0). 255 commit, tek geliştirici, tek sahip. Canlı müşteri sayısı hakkında iddia yok | depo geçmişi |
| **Arayüz dili** | **52 dil** (`public/intl/messages`) | **2 dil** (Türkçe, İngilizce) | kaynaktan sayıldı |
| **Analiz yüzeyinin genişliği** | Huni, hedef, kohort, segment, karşılaştırma, yolculuk, atıf, elde tutma, gelir, performans, UTM raporu; özel panolar (boards) | Kartlar + kırılımlar + ayarlar. Huni, kohort, segment, gelir, atıf **yok** | Umami rota ağacı |
| **Oturum kaydı ve ısı haritası** | Var (`replays`, `heatmaps`, `record` ucu) | **Yok** | Umami rota ağacı |
| **Kısa bağlantı ve pixel takibi** | Var (`links`, `pixels`) | **Yok** | Umami API ağacı |
| **Gerçek zamanlı görünüm** | Var (`realtime` sayfası + `api/realtime`) | **Yok** — pano aralık seçiyor (7/30/90 gün), canlı akış göstermiyor | iki tarafın kaynağı |
| **İki adımlı doğrulama (2FA)** | Var (`api/2fa`, dört tablo) | **Yok** (kurtarma kodları var, 2FA yok) | Umami şeması |
| **Paylaşım bağlantısı** | Var (`share`) — panoyu dışarıya açma | **Yok** | Umami API ağacı |
| **Toplu olay alma ucu** | Var (`api/batch`) | **Yok** | Umami API ağacı |
| **Veritabanı seçeneği** | PostgreSQL **veya ClickHouse** | Yalnız PostgreSQL + TimescaleDB | `db/` dizini |
| **Belgelendirme ve topluluk desteği** | umami.is, geniş topluluk, hazır entegrasyonlar | Yalnız bu depodaki belgeler | — |

**Bunun anlamı:** bugün "bana huni, kohort, oturum kaydı ve ekibime 52
dil lazım" diyen biri için doğru cevap Umami'dir ve bu tablo onu
değiştirmiyor.

---

## 2. Ölçülen: bellek ve disk

Bu bölüm de büyük ölçüde tek yönlü, ama yönü ters. İkisi de kendi
varsayılan yapılandırmasında, aynı makinede.

| Ölçüm | Crucible (beacon) | Umami | Nasıl |
|---|---:|---:|---|
| Boşta bellek (RSS) | **15,7 MB** | 129 – 203 MB | iki ayrı başlatma |
| 400 olay sonrası | **18,9 MB** | 347 – 405 MB | aynı yük, aynı sıra |
| 45 sn dinlendikten sonra | — | 350 MB | Node belleği geri vermiyor |
| Dağıtım boyutu | **72,5 MB** (dört ikili: collector, beacon, API, panel) | ~296 MB (standalone 103 + statik 5,6 + public 5,2 + GeoLite2 63 + Node çalışma zamanı 119) | `du`, `ls` |
| Çalışma zamanı gereksinimi | **Yok** (statik ikili) | Node.js 22 | — |

**Uyarı, kendi sayımıza karşı:** Umami'nin bellek sayısı iki başlatmada
129 ile 203 MB arasında değişti; V8'in yığını çöp toplamaya bağlı, yani
bu bir aralık, tek bir değer değil. Ve 350 MB "gereken bellek" değil,
"400 olaydan sonra bırakmadığı bellek". Yine de mertebe farkı gerçek:
**512 MB'lık bir sunucuda ikisi aynı rahatlıkta değil.**

---

## 3. Ölçülen: olay alma hızı

Aynı makine, **aynı PostgreSQL**, uygulama ve veritabanı aynı
çekirdeklere `taskset` ile çivilenmiş, yük kalan çekirdeklerden, aynı
yük üreteci, aynı protokol, iki tarafta da **hiçbir ayar yapılmadan**.
Umami üretim kipinde (`NODE_ENV=production`, test bunu kontrol ediyor).

Ölçülen sayı **kabul edilen istek değil, veritabanına düşen satır** —
çünkü beacon tamponlayıp `COPY` ile toplu yazıyor, Umami isteğin içinde
yazıyor, ve "kabul ettim" iki tarafta aynı iş değil. Her hücre **iki tam
koşunun aralığı** ve hepsi **alt sınır**.

| | 1 çekirdek | 2 çekirdek | tek bağlantı p50 |
|---|---:|---:|---:|
| **Crucible beacon** | **22.445 – 23.982 satır/s** | **30.157 – 30.402 satır/s** | **182 – 215 µs** |
| Umami `/api/send` | 183 – 192 satır/s | 270 – 277 satır/s | 5,71 – 7,78 ms |
| Umami `/api/send` (jetonlu) | 283 – 306 satır/s | 335 – 390 satır/s | 3,59 – 6,03 ms |

Umami'nin *en iyi* yoluna karşı **73 – 91 kat** olay, **19 – 31 kat** az
gecikme.

**Eşzamanlılıkla ne oluyor** (1 çekirdek, Umami):

| bağlantı | satır/s | p50 |
|---:|---:|---:|
| 1 | 129 | 6,50 ms |
| 8 | 129 | 54,96 ms |
| 32 | 159 | 188,79 ms |
| 128 | 162 | 679,59 ms |

Verim eşzamanlılıkla artmıyor, gecikme doğrusal büyüyor: tek bir kaynağın
seri kuyruğu. Crucible aynı testte tepesini 128 bağlantıda %91-100
koruyor.

### Bedeli — bu tablonun en önemli satırı

**Umami cevap verdiğinde olay diskte. Beacon cevap verdiğinde olay
bellekte**, en çok iki saniye sonra diskte. Süreç çökerse tampondaki
olaylar kaybolur (`internal/beacon.Writer` bunu açıkça yazıyor).

Yani hızın bir kısmı satın alınmadı, **ödünç alındı**: dayanıklılıktan.
Bilinçli bir takas — düşen bir sayfa görüntülemesi yuvarlama hatası,
yavaş açılan bir sayfa gerçek bir problem — ama takas olduğu için
tabloda duruyor. Kayıp riskini kabul etmeyen biri için Umami'nin
davranışı daha doğrudur.

**İkinci uyarı:** fark büyük olduğu için mekanizması yazılmalı. İstek
başına çekirdek zamanı Crucible'da ~43 µs, Umami'de ~3.400 µs. Aradaki
iş: 500'lük gruplarla `COPY` karşısında olay başına bir `INSERT`;
önceden derlenmiş Go işleyicisi karşısında Next.js rota işleyicisi +
Prisma. Bu bir "iyi kod / kötü kod" farkı değil, iki farklı mimari
tercihin sonucu.

---

## 4. Crucible'ın yaptığı, Umami'nin yapmadığı

| Konu | Crucible Analytic | Umami |
|---|---|---|
| **JavaScript'siz sayım** | Collector vekili her isteği görüyor: bot, tarayıcı, JS kapalı ziyaretçi | **Göremiyor** — JS koşmazsa olay hiç doğmuyor |
| **TLS parmak izi (JA4)** | Var, kırılım olarak da | Yok |
| **Bot skoru** | 0-100, hız + parmak izi + UA + ASN'den; bot **sayılıyor** ve ayrılıyor | UA'ya göre `isbot`, ve bot olayı **reddediliyor** (sayılmıyor) |
| **ASN kırılımı** | Var (yerel aralık tabloları) | Yok (ülke/şehir var) |
| **Ülke/ASN engelleme** | Var, aşırı yük politikasından bağımsız | Yok |
| **Kendini koruma** | `[limits]`: eşzamanlı bağlantı ve saniyedeki istek tavanları, fail-open / fail-closed / throttle politikaları, panelden değiştirilebilir | Yok |
| **Veritabanı yetki ayrımı** | Beş Postgres rolü, sütun bazlı GRANT, RLS + FORCE; beacon yalnız INSERT edebiliyor, okuma API'si yalnız SELECT | Tek uygulama rolü |
| **Geliştirici/sahip ayrımı** | Geliştiriciye iş çıkaran her şey hash'li geliştirici parolasının arkasında; müşteri kendine yetki vererek geçemiyor | Rol tabanlı (admin/user), böyle bir ayrım yok |
| **Yedek al / doğrula / geri yükle** | Panelden; sırlar ayrı yedek türü, dosya bütünlüğü doğrulanıyor, şema yükseltmesinden önce otomatik yedek | Yok (veritabanı yedeği operatörün işi) |
| **Şema yükseltmesi** | Panelde düğme; istek satırı + yetkili uygulayıcı, sıkıştırılmış kurulumda da çalıştığı teste bağlı | `prisma migrate deploy`, elle |
| **Sağlık sayfası** | Her bölüm kendi başına düşebiliyor; kalp atışı kanalı | `api/heartbeat` (tek uç) |
| **Ziyaretçiye dönük gizlilik yüzeyi** | `<önek>/privacy` (JSON) + `privacy.html`, `optOut()/optIn()/status()` API'si, panelden açılıp kapanıyor | Yok (çerezsiz olduğu belgelerde yazılı) |
| **Saklama ve sıkıştırma politikası** | Ayardan; siteye göre bölümlenmiş sıkıştırma, ölçülmüş 4,1 kat disk kazancı | Yok |
| **Süreli üyelik** | Var (bitiş tarihi, sahip rolüne konulamıyor) | Yok |
| **Denetim kaydı** | Var (başarısız girişler dahil) | Bakılmadı — iddia yok |

---

## 5. İkisinde de olanlar

Kendi sunucunuzda çalışma · açık kaynak (Apache-2.0 / MIT) · PostgreSQL
· çerezsiz ziyaretçi kimliği · gömülü JS parçacığı · sayfa, yönlendiren,
kampanya/UTM, tarayıcı, işletim sistemi, cihaz, ülke kırılımları · çok
kullanıcı ve takım/rol yönetimi · özel olaylar · saklanan verinin sizde
kalması.

*(Bu listeye "gerçek zamanlı görünüm"ü de yazmıştım; iki tarafın
kaynağına bakınca bizde olmadığı çıktı ve satır §1'e taşındı. Tarafsızlık
bunu yapmayı gerektiriyor: doğrulanmamış bir "ikisinde de var" satırı,
tabloyu sessizce lehe çeviren şeydir.)*

---

## 6. Ölçülmeyenler — ikisi için de

- **Umami'nin okuma tarafı ve panosu.** Bu belge yalnız olay alma
  yolunu ölçüyor. Umami'nin raporları ClickHouse ile çok farklı
  davranabilir; ölçülmedi.
- **Ayarlanmış hâlleri.** İkisi de kurulduğu gibi koştu. Prisma'nın
  bağlantı havuzu, PostgreSQL'in `shared_buffers`'ı, Node'un yığın
  sınırı — hiçbiri elle ayarlanmadı.
- **Crucible'ın ülke/ASN çözümü açıkken** olay alma hızı. Varsayılan
  profil (`hafif`) aralık tablosu yüklemiyor; Umami'nin derlemesi
  MaxMind GeoLite2-City indiriyor ve olay başına şehir çözüyor. O adımın
  maliyeti ayrıca ölçüldü (0,064 ms) ve farkı açıklamıyor, ama iki
  tarafın yapılandırması bu noktada **eşit değil.**
- **HTTP/2**, gerçek internet gecikmesi, 4 ve üstü çekirdek, uzun
  süreli dayanıklılık, eşzamanlı okuma+yazma yükü.
- **Umami'nin kendi yayımladığı bir verim sayısı yok**, yani buradaki
  sayılar onların iddiasıyla değil yalnız bu makineyle
  karşılaştırılabilir.

---

## 7. Kısa cevap

**Umami'yi seçin** eğer: geniş analiz yüzeyi (huni, kohort, segment,
oturum kaydı, gelir) istiyorsanız; ekibiniz Türkçe/İngilizce dışında bir
dil kullanıyorsa; olgunluk ve topluluk desteği bir gereksinimse; ya da
olay kaybı riskini hiç kabul etmiyorsanız.

**Crucible Analytic'i seçin** eğer: küçük bir sunucuda yüksek olay
hacmi taşıyorsanız; JavaScript koşmayan trafiği (bot, tarayıcı) saymak
istiyorsanız; veritabanı yetkilerinin sıkı ayrılması, panelden yedek ve
şema yükseltmesi, geliştirici/sahip ayrımı sizin için gereksinimse; ya da
Türkçe bir operasyon yürütüyorsanız.

**Bir cümlede:** Umami daha çok şey yapıyor, bu daha hızlı ve daha az
kaynak tüketiyor. İkisi aynı sorunun iki farklı yerine bakıyor.

---

Ölçümlerin tamamı, düzeneği ve ölçerken yaptığım beş hata `NOTES.md`
içinde *"Umami kuruldu ve aynı düzenekte ölçüldü"* başlığı altında.
Ölçümü tekrarlamak için: `internal/loadtest/eventthroughput_test.go`
dosyasının başındaki komut.

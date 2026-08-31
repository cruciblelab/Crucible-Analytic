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

## 0.5 DURUM — tek bakışta nerede olduğumuz

*Son güncelleme: 2026-08-26 (D2, C6, A5.2 ve üç ürün kararı sonrası —
§0.6). Bu bölüm her faz sonunda güncellenir ve belgenin geri kalanını
okumadan "nerede kaldık" sorusunu cevaplar.*

**Rakamlar** *(ölçüldü, 2026-08-26)*: 23 iç paket, **5 binary**
(`collector`, `beacon`, `analytics-api`, `devpass`, `panel`), 103 test
dosyası, **836 test fonksiyonu**, ~59.900 satır Go — **29.128'i test
dışı, 30.755'i test**. Yani test kodu üretim kodundan fazla, ve bu
oran bilinçli: bu projede bir şeyin çalıştığı iddiası ancak gerçek
bağımlılığa karşı ölçüldüyse kabul ediliyor.

Belgeler ~10.000 satır (`README`, `PLAN`, `NOTES`, `KURULUM`,
`SECURITY`, `THIRD-PARTY`).

**Zincirin tamamı — boş veritabanından oturum açmış sahibe — uçtan uca
yürüyor.** İlk çalıştırma tespiti, geliştirici sihirbazı (8 adım), devir
teslim, giriş, iki faktör, hesap ve üye yönetimi, pano ve kırılım
sayfaları çalışıyor. Doğrulama gerçek PostgreSQL 16 + TimescaleDB
2.17.2'ye, gerçek Chromium'a (8 tarayıcı testi) ve gerçek eşzamanlı TCP
yüküne karşı yapılıyor.

**Modülerlik ölçümü (2026-08-17, AI.2 sonrası).** 22 paketin **12'si
yaprak** — hiçbir iç bağımlılığı yok. Bağımlılık grafiği sığ ve tek
yönlü, **döngü yok**; en yüksek fan-out `cmd/collector` (11), ki bir
binary'nin her şeyi bağlaması beklenen şey. `internal/panel`'i yalnız
iki yer import ediyor: `cmd/panel` ve `internal/panel/web`.

AI.2 öncesi `internal/panel` 4.424 satırdı; `preflight` çıkınca **3.397**
oldu ve deponun en büyük dosyası (`preflight.go`, 985 satır) kendi
paketine geçti. Sınır artık yorumda değil testte: `preflight`'ın
`internal/panel`'i import etmediğini `TestPreflightDoesNotImportThePanel`
tutuyor.

**Bölme borcu büyüdü ve ölçüsü burada (2026-08-26).** C3/C4 sonrası
`internal/panel` 3.955 test dışı satırdı, `internal/panel/web` 3.650.
Şimdi **4.837** ve **5.470** — yani D1, D2 ve C6 en büyük iki paketi
sırasıyla %22 ve %50 büyüttü ve `internal/panel/web` deponun en büyük
paketi oldu.

| Paket | Test dışı satır |
|---|---|
| `internal/panel/web` | 5.470 |
| `internal/panel` | 4.837 |
| `internal/api` | 3.340 |
| `internal/beacon` | 2.561 |
| `internal/panel/ui` | 2.098 |

AI.2'nin "kimlik ailesi C4'ten sonra bölünür" notu artık geciken bir
borç. **Bilinçli olarak ertelendi:** D grubu aynı iki pakete yazmaya
devam edecek, ve akan bir yüzeyi bölmek bittikten sonra bölmekten pahalı.
Ama D grubu bitmeden büyümeye devam ederse bu satır bir sonraki fazda
gerekçe değil bahane olur.

### Gruplar

| Grup | Durum | Kalan |
|---|---|---|
| **AI** ara işler | ✅ **4/4** | — |
| **A** Ayarlar ve saklama | 🟡 **10/14** | A2, A3, A8, A9 |
| **B** Gözlemlenebilirlik | 🟡 **5/7** | B3, B5 |
| **C** Panel HTTP yüzeyi | 🟡 **11/12** | C8 geliştirici erişim politikası |
| **D** Dashboard | 🟡 **4/8** | D4b, D5–D8 (D4a yapıldı; D3'ten yalnız ham dışa aktarma kaldı) |
| **E** Birleştirme | ⬜ **0/3** | hepsi |
| **G** Yayın hattı | ✅ **2/2** | — (F2 kurulum betiği F'de) |
| **H** Güvenlik taraması | 🟡 **3/5** | H1 (ja4 yapıldı, üç ayrıştırıcı kaldı), H3 |
| **F** Ertelenen | 🟡 **1/3** | F1 yedekleme, F3 filo — bilerek sonraya |
| **K** Kanıt ve dağıtım | ✅ **3/3** | — *(planda yoktu; §K grubu neden araya girdiğini yazıyor)* |
| **L** Yükseltme yolu | ✅ **3/3** | — *(altıncı binary + systemd timer; "hiçbir servis durmuyor" ölçüldü)* |
| **M** Veri kaynakları | ⬜ **0/3** | M1 kütüphane, M2 çekim kaydı, M3 yenile düğmesi |

### Kalan fazların sırası — biri diğerine yük bindirmesin diye

*(2026-08-30. Grup harfleri sıra değil kategori; asıl sıra bağımlılıklardan
çıkıyor ve harflerle uyuşmuyor. Aşağıdaki bağların hepsi koda bakılarak
doğrulandı, hatırlanarak değil.)*

#### Ölçülen üç yapısal gerçek

**1. Ayar sayfası hiç yok.** Panelin rotaları sayıldı: hesap, sağlık,
üyeler, pano, kırılım, adres listesi, geliştirici erişimi, posta, teknik
kapı, karşılama, sahiplenme, kurulum. **`/ayarlar` yok.** Arka ucu hazır
(`Store.SettingsView`, `settings_access.go:248`), gösterecek sayfa
yapılmamış.

Bu tek eksik **A2, B3 ve L3'ü birden bekletiyor** — üçü de kullanıcının
bir ayar sayfasında yapacağı iş.

**2. Sağlık sayfası zaten sürüm gösteriyor** (`buildinfo.Version`, artı
kalp atışı satırlarından servis başına). Yani **L1'in evi var** ve ayar
sayfasını beklemiyor.

**3. E1 planın en büyük yeniden-yazım üreticisi.** Dört binary → bir,
üç TOML → bir demek; dokunduğu yer: `install.sh`, Docker `entrypoint.sh`
(beş giriş noktası), dört systemd birimi, örnek yapılandırmalar, üç
`config.go`, `build.sh`, KURULUM.md, `examples_test.go`,
`ports_test.go`. **Ne zaman yapılırsa, ondan öncesi bir kez daha
yazılıyor.**

#### Bağımlılık haritası

```
D4a ayar sayfası kabuğu  ←— hiçbir şeye bağlı değil
     ├─→ A2 profil seçici
     ├─→ B3 39 operasyon
     └─→ L3 yükseltme düğmesi

B1 panel_logs ──→ B2 panel_operations
     ▲                   ├─→ B3
     │                   ├─→ D4b akan operasyon penceresi
  korelasyon             └─→ L3 (yükseltme bir operasyondur)
  kimliği satır
  şeklini belirliyor

A3 yalnız-ülke ──→ A2 profiller ──→ D5 profil/görünüm ayrımı

L1 şema sürümü ──→ L2 açılış kontrolü ──→ L3 düğme

H1 · H3 · E2  ←— bağımsız, araya girebilir
E1            ←— ayrı karar, herkesi yeniden yazdırır
```

#### İki gerçek yük bindirme riski

**B1 ile B2 ayrılamaz.** *(2026-08-30: tek faz olarak yapıldı ve tahmin
doğruydu — `panel_logs.operation_id` içindeki değer
`panel_operations.id`'dir.)* B2'nin korelasyon kimliği B1'in **satır
şeklinde** taşınmak zorunda. B1 önce ve o sütun olmadan yapılırsa, B2
geldiğinde log tablosu ve yazıcısı yeniden yazılır.

**L3'ten önce B2 gelmeli.** Yükseltme, tanımı gereği bir operasyondur —
"ne oldu, hangi adımda düştü, geri alındı mı". L3 B2'siz yapılırsa
yükseltme operasyon günlüğünün dışında kalır, ve B2 geldiğinde L3 elden
geçer.

#### Sıra

| # | iş | neden burada |
|---|---|---|
| 1 | ~~**L1** şema sürümü~~ ✅ | bağımsız; evi hazır; L2 ve L3'ün ön şartı; az önce ölçtüğümüz riski kapatıyor |
| 2 | ~~**L2** açılışta sütun kontrolü~~ ✅ | L1'in kaynağını kullanır; **müşteri düğmeye hiç basmasa bile** veri kaybını kapatır |
| 3 | ~~**D4a** ayar sayfası kabuğu~~ ✅ | üç fazı birden açan tek iş; arka ucu zaten hazır |
| 4 | ~~**B1+B2** birlikte~~ ✅ | tek faz, ayrılamaz; B3/D4b/L3'ün altı |
| 5 | ~~**L3** düğme~~ ✅ | 3 ve 4'ün üstüne oturdu; beşinci rol ve altıncı binary burada geldi |
| 5.5 | **D4c** ayarlar kabuğu ← **sıradaki** | M1, C8 ve L3'ün yüzeyleri buraya biniyor; kabuk sonra gelirse üçü de düz listeye eklenip taşınır |
| 5.6 | **M1 → M2 → M3**, **C8** | M3 sonucu göstermeden yarım kalır, sonucu M2 getirir; C8 bağımsız, kabuğu bekler |
| 6 | **A3 → A2 → D5** | kendi içinde kapalı zincir; A3'süz A2 yalan söyler |
| 7 | **B3** 39 operasyon | B2 ve D4a üstünde |
| 8 | **H1 · H3 · E2** | bağımsız; herhangi bir yere sıkışır |
| 9 | **D4b · D6 · D7 · D8** | yüzey işleri, altları hazır olunca |
| 10 | **E1** | *bilinçli karar ister* — ne zaman yapılırsa öncesi yeniden yazılır |

**A3'süz A2 yalan söyler:** A2'nin "Dengeli" profili "yalnız ülke" diyecek,
ama `asnlookup` A3 olmadan dört tabloyu da yüklemeye devam edecek. Profil
tablosunun vaat ettiği ~65-70 MB gerçekleşmez ve panel doğru olmayan bir
şey gösterir. Bu yüzden A3 önce.

**E1 hakkında:** ertelemenin bedeli her fazda biraz büyüyor, ama K2
(Docker) E1'in yarısını zaten ödedi — `entrypoint.sh` bugün
`crucible <servis>` biçiminde dağıtım yapıyor. Kalan yarı üç TOML'un
birleşmesi, ve orası `install.sh` ile örnek yapılandırmaların tam
göbeği. Karar bilinçli verilmeli, sürüklenerek değil.

### Bitmiş maddeler

*(İki numaralandırma ailesi var ve karışıyor: tireli **AI-1/AI-2** §2.5'in
ara işleri, noktalı **AI.2/AI.3** ise faz araları. İsimler commit
mesajlarında geçtiği için değiştirilmedi; ayrımı burada yazmak
düzeltmekten ucuz.)*

- **AI-1** Sunucu otoritesi — denetlendi, ihlal yok; güven kararları
  iddia+karar birlikte kaydediliyor
- **AI-2** Log ağacı — `<dir>/<servis>/<gün>/<kategori>.log`, 9 kategori,
  JSON satırları, log enjeksiyonuna karşı temizleme, sır maskeleme
- **AI.2** Paket bölme — `preflight` kendi paketine (deponun en büyük
  dosyasıydı), `internal/config` → `internal/collector`. Sınır yorumda
  değil testte: `TestPreflightDoesNotImportThePanel`
- **AI.3** Güvenlik denetimi — OWASP Top 10 (2021), CWE Top 25, ASVS.
  **Sekiz bulgu düzeltildi**, ikisi bağımlılıkta ve `govulncheck`
  *erişilebilir* dedi (pgx'te yer tutucu karışması = enjeksiyon, x/text'te
  sonsuz döngü); on üç başlık bakıldı ve doğru bulundu; üçü açık
  bırakıldı ve **etkiledikleri sayfada yazıldı**. `SECURITY.md`
- **A1** `panel_settings` — kapalı anahtar kaydı, yazarken **ve okurken**
  sınır doğrulaması
- **A1.5** Log yaşam döngüsü — düz metin → sıkıştırılmış → silinmiş,
  kategori bazlı saklama (sıradan 14 gün, önemli 365 gün), ölçülen %6
- **A6** *(mekanizma)* Canlı ayar okuma — son bilinen değeri koruyan
  önbellek; beacon'da 5 ayar bağlı
- **C2.5** Kurulum sihirbazının son adımı — 11 manuel adım, 14 otomatik
  kontrol, ikisi negatif (rol yalıtımı)
- **A7** IP saklama modu — **varsayılan maskeli** (hukukçu kararı),
  yazma anında ve son adımda; ülke/ASN ve ziyaretçi kimliği tam
  adresten türetiliyor
- **A7.5** Geliştirici şifresi kapısı — 7 hukuki ağırlıklı ayar, her
  seferinde soruluyor, hash'li, yapılandırma dosyasından
- **A4** Saklama politikaları — iki hypertable'da gerçek
  `add_retention_policy`; chunk düşürme, satır silme değil; en uzun
  saklama isteyen siteyi koruyan politika + yalnız daha azını isteyen
  site için hedefli temizlik
- **A7.6** Görünürlük ≠ yazılabilirlik — müşteri **her ayarı görür**
  (değeri, kaynağı, gerekçesi), geliştirici ayarlarında kontrol yok,
  hukuki olanlarda kilit; şifre alanı yalnız işletmeciye gösterilir.
  Kilit metni üç şeyi söylüyor: ne olduğu, **ne bozulacağı**, ve ne
  yapılacağı ("bize iletin, sunucuya bağlanıp biz yaparız")
- **A7.8** **Ham IP hiçbir modda saklanmıyor.** İki mod: `masked`
  (yalnız ağ, anahtar gerekmez, varsayılan) ve `full` (aynı maskeli ağ
  **artı** tam adresten türetilmiş anahtarlı jeton). `full`'e geçmek iki
  koşul ister: geliştirici şifresi **ve** anahtarın önceden config'de
  olması — yoksa panel değeri reddeder, sessizce maskeli çalışmaz.
  Kesişim birleşimi tek paylaşılan ifadeye taşındı
  (`COALESCE(ip_hash, inet_send(ip))`) ve full modda /24 yerine tam
  adres çözünürlüğünde çalışıyor; yapısal test çıplak `ip` birleşimini
  yasaklıyor. `cmd/devpass -ipkey` anahtarı üretiyor, sihirbaza adım ve
  preflight kontrolü eklendi
- **A7.9** Geliştirici modu uyarısı — kapıda uyarı, kilit değil:
  "teknik bilginiz yoksa değiştirmeyin, görüntülemenin riski yok"
- **C1** Panelin render katmanı — `internal/panel/ui` (katalog,
  biçimlendirme, gömülü varlıklar, şablonlar, güvenlik başlıkları),
  `internal/panel/web` (yapılandırma, yönlendirme, erişim günlüğü) ve
  **`cmd/panel`**. Katalog iki yönlü denetleniyor (eksik anahtar panelin
  açılmasını engelliyor, kullanılmayan anahtar testte hata veriyor);
  CSP'de `unsafe-inline`/`unsafe-eval` yok ve yapısal test kaynağı
  koruyor; varlıklar içerik hash'li URL'den bir yıl önbellekleniyor,
  sayfalar hiç önbelleklenmiyor. Gerçek tarayıcı, hiçbir Go testinin
  bulamayacağı iki şeyi buldu (htmx'in satır içi `<style>`'ı ve favicon
  404'ü) — ikisi de düzeltildi
- **C2** İlk çalıştırma tespiti ve geliştirici sihirbazı — hesabı olmayan
  kurulumun ön sayfası durumu söyleyip komutu yazıyor; `panel -dev-link`
  tek kullanımlık bağlantı üretiyor; altı adımlı sihirbaz, ikisi yazan
  dördü doğrulayan; saklama adımı geliştirici şifresini her seferinde
  soruyor ve analitik saklamayı **site başına** yazıyor; son adım gerçek
  sorguları düğmeye basınca çalıştırıyor. Geliştirici bağlantısının
  kullanımı artık denetim kaydında
- **C1.5** Çok dillilik *(kullanıcı isteği)* — dil paketleri dizinden
  **bulunuyor**, listelenmiyor: yeni dil = bir dosya + yeniden derleme.
  `tr` temel, `en` eklendi. Temel paket anahtar kümesinin sahibi (eksik
  anahtar açılışı engelliyor); çeviri eksik olabilir (temel dile düşer,
  raporlanır, testte hata verir). Sayı/tarih/çoğul biçimleri dile bağlı,
  çoğullar gerçek CLDR kurallarından. Testler **repoda olmayan bir
  Rusça paket** yüklüyor — "yeni dil kod değişikliği istemiyor" iddiası
  gösteriliyor, öne sürülmüyor
- **A7.7** *(düzeltme)* **Geliştirici modu bir sayfadır, yetki değil.**
  Erişimi üç soru belirliyor ve yalnız ikisi kontrolü kapatabiliyor:
  (1) config dosyasında mı — öyleyse panelden **kimse** değiştiremez,
  geliştirici dâhil; (2) hukuki/etik ağırlığı var mı — öyleyse yalnız
  işletmeci, her seferinde şifreyle; (3) değilse sıradan ayar, müşteri
  değiştirir. Beş ayar müşteriye geri döndü (log arşivleme, log
  seviyesi, verbose penceresi, sıkıştırma, beacon site listesi). Ayrıca
  **config dosyası ayarları salt-okunur listeleniyor** — parola taşıyan
  alanların yalnız varlığı yazılı, değeri hiçbir yerde gösterilmiyor
- **C4** Giriş, iki faktör, hesap ve üye yönetimi — bu fazın eklediği
  güvenlik özelliği **yok**; hepsi daha önce yazılmıştı. Eklediği şey
  onlara *ulaşılıp ulaşılmadığına* karar veren kısım. Throttle paroladan
  önce sorulıyor; her başarısızlık aynı cümle, aynı statü, aynı süre
  (hesabı olmayan adres için de argon2id çalıştırılıyor, yoksa yanıt
  süresi üyelik oracle'ı olur); `?next=` onarılmıyor **reddediliyor**
  (`//host` ve `/\host` dâhil); bekleyen ikinci faktör bir oturum değil.
  Bağlantıyı gizlemek yetki değil: gezinme filtreleniyor **ve** her
  handler yeteneği ayrıca soruyor
- **C6** Müşteri ne görecek — **bu projenin tek teknik olmayan ayarı.**
  Siteyi yaptıran kişi parmak izinin ne olduğunu bilmiyor ve bilmek
  zorunda da değil; D1+D2 sonrası sayfa on iki bloka çıkmıştı ve hiçbiri
  seçilebilir değildi. Artık site başına iki ayar, kurulum sihirbazında
  bir adım (yedinciden sekizinciye çıktı), sonradan da değiştirilebilir.
  **Gizlemek yetmez, sorgu hiç atılmamalı:** istek kayıttan değil seçili
  kümeden kuruluyor, ve testi süre değil **API'ye giden yol sayısı**
  ölçüyor. Yalnız collector kartı seçen bir sayfa beacon özetini bile
  çekmiyor; `KnownSites` de artık yalnız okunacak listeyi soruyor.
  **Canlı koşuda kendi kuralımı düzelttim:** "boş = varsayılan" tek
  başına yetmiyordu — yalnız collector çalıştıran bir kurulum beacon
  kırılımlarını kapatamıyor, müşterisi de "snippet kurulmamış" diyen
  altı tablo görüyordu. `ViewNone` o yüzden var: boş liste ile "hiçbiri"
  diskte farklı olmak zorunda
- **KURULUM.md** Sabit kurulum kılavuzu (Türkçe) — indirmeden devir
  teslime kadar, her komut yazılmadan önce çalıştırıldı. Yazma işi dört
  gerçek hata buldu, dördü de belgede değil **koddaki/örnekteki**
  karşılığındaydı: `panel.example.toml` ayrı bir veritabanı gösteriyordu
  (yalıtım kontrolü hata verir → `CheckSkip` → devir teslim **kalıcı
  olarak** bloke), ilk taslaktaki `ALL SEQUENCES` grant'ı analitik
  rollerine panelin dizilerini açıyordu (bu veritabanındaki altı dizinin
  altısı da panelin; analitik tablolarında `BIGSERIAL` yok),
  `preflight.checkService` binary'de ölü (aşağıdaki risk tablosu),
  README iki yerde eskimişti (Go 1.23+ → `go.mod` 1.25.0; "pano yok" →
  D1'den bir commit sonra). §4'ün tamamı gerçek TimescaleDB'ye
  uygulanarak doğrulandı: dört rol × beş tablo yetki matrisi basıldı,
  roller düşürüldü
- **D2** Detaya inişler — D1 altı sayı gösteriyordu, bu faz **neden** o
  olduğunu gösteriyor: sayfa, kaynak, kampanya, cihaz, ülke, olay.
  Kırılım kimliği başka bir servise giden **yol parçası** olduğu için
  kayıt kapalı küme ve arama, site aranmadan önce yapılıyor. Dört satır
  tipi tek tabloya iniyor ama sütun başlığı kırılımın kendisinden
  geliyor — sayım pageview de olabilir olay adedi de, ve payda da öyle.
  Boş grup adı olan bir satır: API onu düşürmüyor ki toplamlar tutsun,
  panel de düşürmüyor. **Yazarken kendi kodumda gerçek bir hata buldu:**
  kampanya ucu etiketsiz trafiği SQL'de dışlıyor, yani hem türetilen
  "boş" bayrağı hem de iki kataloğa yazdığım "Kampanyasız" satırı asla
  render edilemezdi; ikisi de silindi, ayrım kayda geçti, iki yönlü test
  eklendi. **Ölçüldü:** 2 çağrı 4,1 ms, 8 çağrı 10,4 ms — altı
  kırılımın dördü özet çağrılarının içinde bittiği için ölçülebilir bir
  maliyet getirmiyor
- **D1** Site panosu — **ürünün var olma sebebi olan sayfa.** Panel bu
  faza kadar analitik API'sine tek bir çağrı yapmıyordu. Sayılar HTTP
  üzerinden geliyor (panelin rolü analitik tablolarını okuyamaz ve
  okumamalı; yapısal test bunu tutuyor). Kart kümesi **kapalı kayıt**,
  C6 sayfayı sökmek yerine seçim bağlayacak. Üç ayrı boşluk üç ayrı
  cümle: snippet hiç kurulmamış / bu dönem boş / API'ye ulaşılamıyor —
  ve **başarısızlık asla sıfır olarak okunmuyor**. Aralık sınırları
  panelin saat diliminde **tam gün**
- **A5.1** Ayar göçünün mekanizması ve en pahalı iki yanlış
  yapılandırma — dosya **her zaman geri düşüş katmanı** kalıyor (satır →
  dosya → gömülü varsayılan), göç satırı bir kez yazıyor ve o andan
  sonra satır kazanıyor. Göç bir kabuk komutu, çünkü servisin rolü
  `panel_settings`'e yazamaz ve **yazamamalı**. Var olan satırın üstüne
  asla yazmıyor. `beacon.trusted_proxies` (kataloğun en üst maddesi) ve
  limitler **servis başına** canlı; collector'ın canlı ayar okuyucusu
  burada yazıldı
- **C5** Geliştirici erişimi onay ekranı — C2 bir kural yazmış ve o
  kurala uymanın yolunu bırakmamıştı: istek, sayfası olmayan bir tabloda
  duruyordu. Kapı `ownsAnySite` **değil** — kullanılmış bir bağlantı
  `Superadmin` taşıyor, yani onaylanmış bir geliştirici bir sonraki
  isteği onaylayabilirdi; önce `Kind`, sonra sahiplik. Sayfa "kim"i
  göstermiyor çünkü panel bilmiyor: gerekçenin, kabuk erişimi olan
  birinin **iddiası** olduğu ilk isteğin üstünde yazılı. Tanımlanıp hiç
  yazılmayan dört denetim eylemi artık yazılıyor; reddedilen kullanım
  yalnız jeton gerçek bir satıra uyduğunda kaydediliyor
- **C3** Devir teslim, sahip sihirbazı ve teknik kapı — zincirin eksik
  halkası: ilk sahip hesabını açmanın hiçbir yolu yoktu. Tek kullanımlık
  sahiplenme bağlantısı (saklanan yalnız SHA-256), **tek işlemde** hesap
  + her siteye sahiplik + davetin tüketilmesi; yarışı veritabanı
  çözüyor (sekiz eşzamanlı sahiplenme → bir hesap, yedi ret). Sahiplenme
  asla superadmin üretmiyor. Teknik kapı: onay **oturumda**, yetki
  değil — her istek yine "bu kişi bir şeye sahip mi" diye soruyor

### Sıradaki işler, önem sırasıyla

*(Sıra üç kez ölçümle değişti. D1'de: altyapı ürün seviyesindeydi,
arayüz yarımdı. 2026-08-26'da: faz sonu doğrulaması elle yapılıyordu ve
kırk dakika sürüyordu, o yüzden G1 öne alındı — ve ilk koşuşunda
kimsenin dokunmadığı kodda 34 erişilebilir zafiyet buldu. Aynı gün
ikinci kez: aşağıdaki H2 maddesi, taramanın hangi fazda işe yaradığı
**denenerek** öne alındı.)*

1. ~~**H2 — statik taramanın taban çizgisi.**~~ ✅ **yapıldı** — kapıda,
   sürüm sabitli, 21 giriş gerekçeli. Aşağıdaki gerekçe olduğu gibi
   duruyor çünkü **sırayı belirleyen buydu** ve sonraki üç faz için
   hâlâ geçerli: artık D3, G2, F2 ve C7.3 taranarak yazılacak.

   Sıradaki üç fazın üçü de
   `gosec`'in yakaladığı sınıflarda **yeni yüzey** ekliyor, ve bu
   tahmin değil: o kod sınıfları kasten yazılıp tarandı. G2'nin paketi
   ve F2'nin betiği arşiv yolu geçişi ile değişkenli alt süreç
   (`G204`, MEDIUM/HIGH) demek; C7.3'ün SMTP'si sertifika doğrulamasını
   kapatma satırı demek (`G402`, **HIGH/HIGH**) — e-posta kuran her
   projede en az bir kez yazılan satır.

   Yani tarayıcı en çok **bundan sonra** işe yarayacak, ve şu an
   anlamlı bir yeşili olmadığı için hiçbir şey tespit etmiyor: taban
   çizgisi yokken her gece aynı 18 bulguyu yeniden buluyor ve yarın
   girecek 19'uncuyu onların içinde kaybediyor. Küçük faz, ama
   kendisinden sonraki üçünün taranıp taranmayacağına karar veren faz.
2. ~~**D3 — aynı sayfalar üzerinde geliştirici katmanı.**~~ ✅ **yapıldı**
   *(Ham dışa aktarma hariç: o bir çizim işi değil, senin verdiğin karara
   bağlı bir ayar — geliştirici şifresinin arkasında.)* D2 o sayfaları
   yazdı; bu faz sütun ekliyor. Kalan yirmi küsur API ucunun (parmak
   izi, ASN, skor, kesişim) panelde karşılığı hâlâ yok. C6 kayıtları
   kapalı küme tuttuğu için yeni bloklar doğrudan seçilebilir olur.
3. ~~**G2 — sürüm paketi.**~~ ✅ **yapıldı** *(Gerekçesi düzeltildi.)* Bu madde "C7'den
   sonra, çünkü davet e-postası gönderemeyen bir ürünü paketlemek
   bitmemiş bir şeyi paketlemektir" diye sıralanmıştı. **O öncül
   yanlıştı ve ölçümle çürüdü:** davet bağlantısı e-postasız çalışıyor,
   ve iki yıldır ürünün en hassas iki akışı zaten böyle çözülmüş. Doğru
   gerekçe şu: C7.2'den sonra paketlenecek şey **eksiksiz**, ve erken
   paketlemek gerçek kurulum demek — gerçek kurulum da gerçek geri
   bildirim.

**Sonrası:** ~~F2 (kurulum betiği)~~ ✅ → ~~C7.3 (e-posta
sihirbazı)~~ ✅ → ~~B4/B7 (sağlık sayfası ve sürüm kavramı)~~ ✅ →
~~K1 (uçtan uca kanıt)~~ ✅ → ~~K2 (Docker)~~ ✅ → ~~K3 (sözlük)~~ ✅ →
**A2/A3 → D4–D8 → B'nin kalanı → E.**

**K grubu bu sıraya sonradan girdi ve girmesi doğruydu.** B4/B7'den
sonra A2'ye geçilecekti; onun yerine "yazdıklarımız gerçekten çalışıyor
mu" sorusu soruldu ve **on iki gerçek kusur** çıktı — biri kurulumun
müşteriye hiç verilememesi. A2'ye geçilseydi, hepsinin üstüne bir kat
daha atılmış olacaktı. Özellik eklemeyen bir faz, sırayı hak edebilir.

**H'nin kalanı sıraya girmiyor, sıraya paralel gidiyor.** H1 (kalan üç
ayrıştırıcı), H3 (sır taraması) ve H4 (yapısal değişmezler) hiçbir
fazı beklemiyor ve hiçbir faz onları beklemiyor. Sıraya sokmak, onları
"sırası gelmedi" diye erteleme hakkı doğururdu; oysa üçü de bir
öğleden sonralık iş. H2 istisna, çünkü **kendisinden sonraki fazların
taranıp taranmayacağını** belirliyor — o yüzden o sırada.

**Bilerek en sona bırakılan:** E grubu (tek binary, tek yapılandırma).
Beş süreci birleştirmek, hepsi hâlâ değişirken yapılırsa iki kez
yapılır.

**Ürün olmak için üç eksik — üçünün de artık sahibi var**
*(2026-08-26)*:

| Eksik | Sahibi |
|---|---|
| ~~CI yok~~ | ✅ **G1'de kapandı.** İlk koşuşunda 1.25.0 araç zincirinde **34 erişilebilir stdlib zafiyeti** buldu — kod değişmemişti, CVE'ler sonradan çıkmıştı. `go.mod` 1.25.13'e alındı, sonrası sıfır |
| Dağıtım aracı yok: sürüm paketi, systemd unit'leri, kurulum betiği | **G2**, sonra **F2** |
| E-posta yolu yok: davet ve parola sıfırlama | **C7** |

Üçü de 2026-08-18'de "açık" olarak yazılmıştı ve sahipsizdi. Sahipsiz
bir eksik, unutulmuş bir eksiktir.

### Açık riskler ve sahipleri

| Risk | Sahibi |
|---|---|
| **Kontrol sonuçları ve elle-yapılacaklar listesi yalnız Türkçe** | açık (`CheckResult.ID` anahtara çevrilebilir; `Detail` dinamik) |
| **`preflight.checkService` binary'de ölü** — yazılmış, testleri var, `cmd/panel` ona hiç adres vermiyor ve `panel.toml`'da o alan yok. Sihirbaz 14 kontrol gösteriyor ve hiçbiri "collector/beacon/API ayakta mı" sorusunu sormuyor | **açık** (KURULUM.md yazılırken bulundu, 2026-08-18; `panel.toml`'a `[service_urls]` eklemek + `PreflightConfig`'e geçirmek — küçük, ama kurulum kontrol yüzeyini genişlettiği için kendi fazını hak ediyor) |
| Doğrulanamayan 5 kurulum adımı ayrı gösterilmeli | C2.5 (`UncheckedSteps()` hazır) |
| **Kesişim görünümleri "maskeli" uyarısını göstermeli** | **D5** (yeni — maskeli varsayılan olduğu için artık her kurulumda geçerli) |
| ~~Collector'da saklama süresi yalnız dosyadan okunuyor~~ | ✅ **kapandı — ama diğer uçtan.** İki tablo artık aynı yerden yapılandırılıyor: ikisi de dosyadan. Panelin saklama ayarı eklenmedi, **kaldırıldı** — ziyaret kayıtlarının ne kadar tutulacağı hukuki ağırlıklı tek ayar ve sunucuya erişmeyi gerektirmeli. Tavan 3650 → 730 |
| **Saklama değişikliği en geç bir saat içinde etkili** | bilinçli — uygulamak idempotent ama kısa saklamalı site için her turda satır silme demek; dakikada bir çalıştırmak boşuna tarama olurdu |
| **Mod değişiminin tarihi denetim kaydından okunmalı** | **D5** (mekanizma hazır: `ActionSettingChanged` eski değeri taşıyor) |
| ~~`logs.level` yalnız beacon'da bağlı~~ | ✅ **A5.2'de kapandı** — `logs.level` ve `logs.verbose_until` iki serviste de canlı; ikisinin de `Live` bayrağı düzeltildi |
| ~~Collector tarafı canlı ayarlar (geo, skor)~~ | ✅ **A5.2'de kapandı** (limitler A5.1'de). **`storage.flush_interval_seconds` bilerek dışarıda** — sebep kapsam değil değer: yazma aralığı bir destek çağrısının ihtiyacı değil, bir performans ayarı |
| Sıkıştırılmış log görüntüleyicide açılmalı | B1 |
| Bakım yalnız açılışta çalışıyor | B3 |
| Log hacmi ve debug penceresinin maliyeti ölçülmedi | B4 |
| `GRANT SELECT` elle veriliyor | F2 |
| **IP jeton anahtarı iki serviste aynı mı** — preflight varlığı görür, aynılığı göremez | **F2** (kullanıcı: "kontrolü ekleriz") |
| Verbose penceresi kullanıcıya yerel saatte gösterilmeli | **D4** (C4'e yazılmıştı; C4 giriş kapısıydı, **müşteriye dönük ayar sayfası hâlâ yok** — tek ayar yüzeyi kurulum sihirbazı) |
| **Geliştirici şifresi kısıtlaması yalnız süreç içinde** | **E** (tek panel süreci varsayımı; birden çok süreç olursa sayaç paylaşılmalı) |
| **Kilitli satırın gösterimi şablon işi** — `SettingsView` hazır, kilit metni ve gerekçe dönüyor | **D4** (aynı sebep: gösterecek sayfa henüz yok) |
| **Panel çok süreçli çalışırsa varlık hash'i ve katalog süreç içinde** | bilinçli — gömülü oldukları için tüm süreçlerde aynı |
| **Sağdan-sola dil denenmedi** — `dir` ve mantıksal CSS hazır, ama hiçbir RTL paket render edilmedi | **açık** (bir RTL paket yazıldığında düzen gözden geçirilmeli) |
| **Hesap bazlı dil tercihi yok** — bugün yalnız kurulum ayarı ve tarayıcı | **açık** (`/hesap` sayfası C4'te açıldı; eksik olan `panel_users`'ta bir sütun ve çözümlemeye kullanıcı tercihinin eklenmesi — çözümleme parametresi zaten variadic) |
| **Şifre değişikliği diğer cihazlardaki oturumları kapatmıyor** — oturum tablosunda kullanıcı sütunu yok, bulmak bugün tablo taraması | **açık** (AI.3'te bulundu, hesap sayfasında yazılı; kapatmak `scs` şemasına sütun eklemek demek) |
| **İki faktör kurtarma kodu yok** — kaybeden kişiyi sahip ya da işletmeci kurtarıyor; tek sahip kaybederse kabuk gerekiyor | **açık** (AI.3; kayıt ve kod formunda yazılı) |
| **Panelde global eşzamanlılık sınırı yok** — her giriş denemesi bir argon2id doğrulaması, sınır kuyruk değil throttle sayaçları | **açık** (AI.3; panel varsayılan `127.0.0.1` dinliyor ve TLS'i sonlandıran bir proxy arkasında çalışması bekleniyor) |
| ~~`govulncheck` düzenli çalıştırılmalı~~ | ✅ **G1'de kapandı** — hem kapıda hem gecelik. Gerekçe teorik değildi: ilk koşu 34 bulgu verdi |
| **Beş binary'nin dördü sürümünü söyleyemiyor** — `main.version` sembolü yalnız `panel`'de var, ama KURULUM.md beşini de `-X main.version` ile derletiyor ve linker olmayan sembol için uyarmıyor *(ölçüldü 2026-08-26, `go tool nm`)* | **G2** sembolü ekler, **B7** gösterir |
| ~~Entegrasyon paketinin tekrar koşulabilirliği çürüyebilir~~ | ✅ **G1'de kapandı** — kapı aynı işte, aynı veritabanına karşı iki kez koşuyor |
| **Kurulum dili config dosyasında, panelde değil** | **A5** (kullanıcı: "ilerleyen zamanlarda"; kayıttaki ilk **dinamik** enum olacak — diller derlemeye bağlı) |
| `utm_term` varsayılanı | **kapatıldı** — mekanizma artık şifreyle korunuyor, karar hukukçuda |
| Kısmi indeks kampanyasız sorguları hızlandırmıyor | **ölçüm bekliyor** |
| Kampanyası olmayanı filtreleyememe | bilinçli sınır, kapatılmayacak |

### Beklenen kararlar (senden)

1. ~~**IP tam mı maskeli mi**~~ — ✅ **cevaplandı: maskeli.** Yazıldı,
   varsayılan yapıldı, doğrulandı (A7)
2. **Analitik saklama süresi** (öneri 90 gün) — A4'e girecek
3. **"Site" tanımı** — alt alan adları tek site mi (öneri: evet, A5'e
   girer, maliyeti ~sıfır)

---

## 0.6 Verilen kararlar — üç açık soru kapandı

Uzun süredir açık duran üç soruyu müşteri karara bağladı. Üçü de ürün
kararı, mühendislik kararı değil; buraya yazılıyorlar çünkü kodda
karşılıkları var ve gerekçeleri altı ay sonra sorulacak.

### 1. Analitik saklama süresi: dosyadan, varsayılan 90, tavan 730

**Karar:** panelden tamamen kaldırıldı, yalnız servislerin
yapılandırma dosyasından değişiyor. Varsayılan 90 gün, tavan 730 gün
(eskiden 3650).

**Neden panelden çıktı:** kayıttaki her başka ayar işletimsel — yanlış
değer başarıma, doğruluğa veya diske mal olur. Bu ayar bir insanın
gezinme geçmişinin ne kadar tutulacağını belirliyor ve KVKK'nın
ölçülülük ilkesinin doğrudan konusu. Geliştirici parolası arkasındaydı;
o güçlü bir kilit ama müşterinin hâlâ içinde durduğu bir odanın
kapısındaydı — değer HTTP üzerinden görünüyor ve değişiyordu.

**Tavan neden 730:** 3650, "sakla" ile "sonsuza kadar sakla" arasındaki
farkın kaybolduğu nokta diye seçilmişti; bu aritmetik hakkında doğru bir
cümle ve sayı için yanlış bir temel. Bir yıl değil iki yıl, çünkü eski
analitiğin dürüst kullanımı "geçen yılın aynı ayı".

**Beraberinde giden:** `analytics.compress_after_days` tamamen
kaldırıldı — hiçbir servis okumuyordu. Etiketi, yardım metni, parola
kapısı ve denetim kaydı vardı; yaptığı hiçbir şey yoktu.

### 2. Alt alan adları: ayrı sayılır, seçim kurulumda söylenir

**Karar:** `site.com` ve `blog.site.com`, snippet'lere aynı site kimliği
yazılırsa tek site, farklı yazılırsa iki site. Ürün karar vermiyor;
**sihirbaz bunu söylüyor.**

**Neden kod değil belge işi:** hangi alan adlarının tek sayılacağı zaten
snippet'e yazılan metinle belirleniyordu. Eksik olan mekanizma değil,
bir karar verildiğini söyleyen cümleydi.

**Neden geri alınamaz:** `visitor_id = HMAC(günlük_tuz, site_id ‖ ip ‖
user_agent)`. Site kimliği hash'in içinde, tuz dönüyor, hash tersine
çevrilmiyor. İki kimlikle toplanan veride ikisini de gezen kişi kalıcı
olarak iki ziyaretçi. Olay sayıları sonradan toplanabilir, kişi sayıları
toplanamaz — bu yüzden panelde "birleştir" düğmesi **yapılmadı**:
görüntüleme toplamlarını doğru, ziyaretçi sayısını yanlış gösteren bir
düğme, dipnotla bile müşteriyi yanıltır.

### 3. Lisans: Apache-2.0

**Karar:** MIT'ten Apache-2.0'a geçildi. SaaS'a çevirmek serbest,
rakip dâhil. Atıf zorunluluğu MIT'ten katı, sorumluluk kabul edilmiyor,
dağıtılan şey yalnız kod.

| İstenen | Apache-2.0'da karşılığı |
|---|---|
| SaaS serbest olsun | Madde 2–3, kısıt yok |
| Atıf daha katı olsun | Madde 4: lisans korunur, değişiklik beyan edilir, `NOTICE` taşınır |
| Sorumluluk kabul edilmesin | Madde 7–8, MIT'in tek cümlesinden çok daha ayrıntılı |
| Veri/log/build dağıtılmasın | Lisans metninin işi değil: `.gitignore` + `NOTICE` + `THIRD-PARTY.md` |

**Neden özel metin değil:** "MIT + kendi ek şartlarımız" istenen kuralı
birebir verirdi ama artık standart bir lisans olmazdı; her kurumsal
kullanıcının hukukçusu metni tek tek okumak zorunda kalır ve bu
benimsemeyi düşürür. Apache-2.0 istenenlerin dördünü de tanınmış bir
metinle veriyor.

**Bu bir hukuki görüş değil.** Metin birebir apache.org'un kanonik
sürümüyle karşılaştırıldı ve aynı; yayına çıkmadan bir hukukçuya
okutulmalı.

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
| `internal/collector` | TOML yapılandırma, çift mod | Birim testli |

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

#### A4 — Saklama süresi politikaları ✅ **yapıldı**

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

**Sonradan çıkan asıl mesele — chunk ile site çakışması:** saklama
süresi *site başına* bir ayar, ama bir chunk o zaman aralığındaki **her
sitenin** satırlarını tutuyor. "A sitesinin 30 günden eski satırları"
chunk düşürerek ifade edilemiyor. Çözüm ikisini işine göre ayırmak:

- **Hypertable politikası**, herhangi bir sitenin istediği **en uzun**
  süreyi kullanır. Ucuz, günlük çalışır, ve hâlâ istenen veriyi asla
  silemez.
- **Daha azını isteyen site**, aradaki farkı hedefli bir silme ile
  kaybeder. Yalnız o site için, yalnız gerçekten kısa bir değer
  tanımlıysa, ve her sitenin aynı süreyi kullandığı sıradan durumda
  **hiç çalışmaz**.

Tersi — politikayı en kısa değere kurmak — daha çok saklamak isteyen her
sitenin verisini sessizce yok ederdi ve özellik çalışıyormuş gibi
görünürdü.

**Bitti ölçütü:** ✅ politika `timescaledb_information.jobs`'tan okunarak
doğrulandı (iki tabloda da); ✅ üç kez uygulamak tek iş bırakıyor, aynı
değeri tekrar uygulamak no-op; ✅ `DryRun` kaç satır sileceğini önden
söylüyor ve hiçbir şey silmiyor; ✅ gerçek satırlarla, yalnız kısa
isteyen sitenin satırı silindi; ✅ gerçek binary'ler gerçek politikayı
kurdu (beacon 45→panelden 120, collector 200); ✅ preflight uyarısı
kapandı.

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

##### A5 ikiye bölünüyor, ve nedeni

A5'in iki yarısı gerçekten farklı işler. **Mekanizma** (bir ayarın
dosyadan veritabanına nasıl taşındığı, mevcut bir kurulumun ayarını
sessizce kaybetmeden) bir *tasarım* işi ve 4 anahtar için de 40 anahtar
için de aynı iş. **Anahtarların kendisi** ise sıralı kablolama: her biri
kendi "canlı olabilir mi" kararını ve kendi testini istiyor, ama
birbirlerinden bağımsızlar.

Mekanizma yanlış yapılırsa anahtarların hiçbiri işe yaramaz. Anahtarların
**bir kısmı** yapılırsa hiçbir şey bozulmaz. Bölme buradan geçiyor.

**Hangi anahtarlar önce:** tahminle değil, §4'ün kendi kanıtıyla. Onarım
kataloğu 7 numara için şunu yazıyor: *"Katalogda en üstte olmayı hak
ediyor: Cloudflare arkasında boş liste, her ziyaretçiyi aynı IP gösterir
ve sistemdeki diğer her sayıyı aynı anda yanlışlar."* Sonra 8/9/10
(limitler). A5.1 bunları alıyor.

---

#### A5.1 — Mekanizma, ve en pahalı iki yanlış yapılandırma ✅ **yapıldı**

**Kapsam:**

1. **Göç komutu ve denetim izi.** A5'in kendi bitti-ölçütü #3.
2. **`beacon.trusted_proxies`** — kataloğun kendi ifadesiyle en üstte
   olmayı hak eden madde. Canlı; `Check` her girdiyi `netip.Prefix`
   olarak ayrıştırıyor.
3. **`limits.*`** (dört anahtar) — katalog 8, 9, 10. Beacon **ve**
   collector'da canlı.
4. **Collector'ın canlı ayar okuyucusu** — bugün *hiç yok*, ve
   limitler onsuz canlı olamaz. Açık risk listesindeki "iki tablo iki
   ayrı yerden yapılandırılıyor" maddesi burada kapanıyor.

**Göç tasarımı: A6'nın kuralıyla A5'in ifadesini bağdaştırmak.**

A5 "dosyadaki değer bir kez göç ettirilir ve **dosyadan yok sayılır**"
diyor. A6 ise "okuma başarısız olursa **son bilinen değerler korunur**,
varsayılana dönülmez — müşterinin ayarlarını sessizce sıfırlamak bayat
bir ayardan kötüdür" diyor. İkisi ilk bakışta çelişiyor: dosya yok
sayılırsa, açılışta veritabanı erişilemezse elde yalnız gömülü
varsayılanlar kalır — tam da A6'nın yasakladığı sessiz sıfırlama.

Çözüm, ikisinin farklı şeylerden bahsettiğini görmek:

- **Kodda dosya her zaman geri düşüş katmanıdır.** Üç katman: satır
  varsa veritabanı, yoksa dosya, o da yoksa gömülü varsayılan. Her
  katman bir öncekinden dar. Bu zaten beacon'da çalışan desen
  (`cfg.Campaign.Live(live)`).
- **Göç, satırı bir kez yazar.** O andan sonra satır kazanır, yani
  dosyayı düzenlemek hiçbir şey değiştirmez — A5'in "yok sayılır"ı
  *etkide* doğru olur.
- **Panel bunu söyler**, ve servis açılışta hangi değerin nereden
  geldiğini loglar. Yoksa biri dosyayı düzenler, yeniden başlatır, ve
  hiçbir şey olmaz — A7.6'nın "görünürlük ≠ yazılabilirlik" dersinin
  aynısı, ters yönden.

**Göçü kim çalıştırır.** Collector'ın rolü `panel_settings` üzerinde
yalnız `SELECT` yapabiliyor (A6 kararı) — yani servis kendi ayarını göç
ettiremez, ve **ettirememeli**: yazma yetkisi vermek, ele geçirilmiş bir
collector'a saklama süresini ve IP saklama modunu değiştirme gücü
verirdi. Bunlar geliştirici şifresinin arkasındaki ayarlar.

O yüzden göç bir kabuk komutu: `panel -migrate-settings <servis>
<dosya>`. Kurucu bir kez çalıştırır. Bu, projenin zaten kullandığı şekil
— şemayı uygulamak, `-dev-link`, `-owner-link` da öyle: **servisin sahip
olmadığı yetkiyi isteyen iş, kabuk komutudur.**

Komut TOML'u **genel olarak** okur (`map[string]any`, bilinen yol → bilinen
anahtar) — servisin config paketini import etmez. İki sebep: panelin
binary'sine beacon sunucusunun tamamını bağlamamak, ve komutun ne
yaptığını dürüst tutmak — *bildiği anahtarları* okur, dosyanın tamamını
doğrulamaz. Doğrulamayı `panel.Validate` yapıyor zaten.

**Üç kural, sırayla:**

- **Var olan satırın üstüne asla yazmaz.** Panelden değiştirilmiş bir
  değeri, unutulmuş bir dosya satırıyla geri almak, göçün yapabileceği
  en kötü şey.
- **Reddedilen değer atlanır ve söylenir**, yazılmaz.
- **Her yazılan anahtar bir denetim kaydı üretir**, dosyanın yolu ve
  değeriyle.

**Bitti ölçütü:** taşınan her anahtar için "dosyada yoksa veritabanından
okunur" testi; göçün var olan satırı ezmediği testi; canlı değişimin bir
aralık içinde etki ettiği testi; veritabanı kesilirken son değerin
korunduğu testi; `-race` temiz.

##### Ne oldu

**Bir ayar iki şey ifade edemez.** İlk taslak `limits.*`'ı iki servisin
okuduğu tek aile yaptı. Bu, söylediğini ifade edemeyen bir sayı olurdu:
collector siteye gelen her bağlantıyı görüyor, beacon yalnız tarayıcısı
snippet'i çalıştıran ziyaretçileri. Biri için doğru olan tavan diğeri
için bir büyüklük mertebesi yanlış, ve ikisini birden kapsayan tek sayı
ne yapılırsa yapılsın bir yerde yanlış. Artık `collector.limits.*` ve
`beacon.limits.*` — `beacon.sites`'ın zaten kurduğu, **adın hangi süreç
okuyor onu söylediği** kural. Sekiz kayıt girdisi tek fonksiyondan
üretiliyor; iki aile arasındaki fark yalnız "hangi süreç okuyor"
olmalı, başka bir şey değil.

**Zaten bozuk olan üç şey.** Hiçbiri bu fazda oluşmadı:

1. **`Definition.Check` beş Kind'in dördünde ölüydü.** Kendi
   dokümantasyonu "Kind'in kendi kurallarından sonra, kanonik biçim
   üzerinde çalışır" diyor. Aslında `switch`'in yalnız `KindString`
   dalında çağrılıyordu; listeye, tam sayıya, bool'a ya da enum'a
   bağlanan bir doğrulayıcı **hiç çağrılmıyordu**. En kötü şekilde
   ortaya çıktı: bozuk bir ağın reddedilmesini bekleyen bir test onun
   saklandığını gördü — ölü bir doğrulayıcı her zaman böyle görünür,
   hata olarak değil **kabul** olarak. Artık `switch`'ten sonra tek
   yerde, her Kind için çalışıyor; test bugün kullanılanları değil beş
   Kind'in hepsini dolaşıyor.
2. **Sessizce hiçbir şey yapmayan bir test temizliği.** `defer
   pool.Close()` fonksiyon dönerken, `t.Cleanup` ise ondan *sonra*
   çalışır — yani havuz önce kapanıyor, satır silmeleri kapalı havuza
   gidiyor ve hata `_` ile atılıyordu. Dört satır süiti aştı ve bir
   sonraki test onlardan birini okuyup "iki servis aynı ayarı
   paylaşıyor" diye patladı.
3. **Süitin geri kalanı hakkında iddiada bulunan bir test.** Göçün
   denetim kontrolü *bütün* `setting.migrated` kayıtlarını dolaşıp her
   birinin bu testin dosyasını adlandırmasını istiyordu.

**Limiter:** `Config` artık atomik işaretçinin arkasında ve **`Admit`
başına bir kez** okunuyor. Aşağıda tekrar okumak, iki kontrol arasına
düşen bir değişikliğin yarısı eski yarısı yeni sınırlarla verilmiş bir
karar üretmesine izin verirdi — kimsenin yapılandırmadığı ve sonradan
tekrar üretilemeyecek bir durum. Kuyruğa girmiş bir çağıran başladığı
yapılandırmayla bitiriyor.

**Collector'ın canlı ayar okuyucusu burada yazıldı** — daha önce hiç
yoktu. Açık risk listesindeki "iki tablo iki ayrı yerden
yapılandırılıyor" maddesi kapandı, ve A5.2'nin kalan anahtarları artık
mimari değil kablolama.

#### A5.2 — Saldırıyı durduran ayarlar ✅ **yapıldı**

**Bu maddenin ilk hâli "kalan her şey, mekanik kablolama" diyordu ve
yanlıştı.** Kalan alanları tek tek okuyunca ikiye ayrıldılar: çalışan
bir süreç bazılarını istekler arasında değiştirebilir, bazılarını
kuruluşta sabitlemiştir. İkisini aynı listeye koymak, müşteriye
değiştiremeyeceği bir ayarı değiştirebilirmiş gibi göstermek olurdu —
A6'nın `Live` bayrağının var olma sebebi tam da bu.

##### Canlı olacaklar, ve neden bu dördü

| Anahtar | Neden |
|---|---|
| `asn_lookup.blocked_countries` | Her bağlantıda bakılan saf veri |
| `asn_lookup.blocked_asns` | Aynı |
| `asn_lookup.known_bot_asns` | Skorlama anında bakılıyor |
| `asn_lookup.apply_to_scoring` | Yukarıdakini açan/kapatan bayrak |

**Seçim ölçütü "kolay olan" değil, "destek çağrısının gerçekten
istediği".** Bugün "şu ülkeden saldırı geliyor, kapat" demek SSH +
dosya düzenleme + yeniden başlatma demek — yani saldırı sürerken en
uzun yol. Diğer aday alanların hiçbiri bu ağırlıkta değil.

**Yapılacak asıl iş bir optimizasyonu kaybetmemek.** `NewGeoBlocklist`
iki liste de boşsa **nil** dönüyor ve sunucu bunu görünce bağlantı
başına ülke/ASN çözümlemesini hiç yapmıyor. Liste canlı olunca o "boş
mu" sorusu açılışta bir kez değil, her bağlantıda sorulmak zorunda —
ama yine tek bir atomik okuma kadar ucuz kalmalı, yoksa hiçbir şey
engellemeyen bir kurulum, hiç kullanmadığı bir özellik için her
bağlantıda LRU'ya gitmeye başlar.

##### Canlı olmayacaklar, ve bunu panelin söylemesi

`[cache]`'in tamamı (pencere/TTL/temizlik: `ratestore` kurulurken
sabitleniyor), `asn_lookup.enabled` (açmak ~135 MB tablo yüklemek),
`cache_max_entries` (LRU boyutu), beacon tampon/parti boyutları (kanal
kapasitesi). Bunlar `Live: false` kalacak; panel "yeniden başlatma
gerekiyor" diyecek. **Yalan söylemekten iyisi budur.**

`storage.flush_interval_seconds` sınırda: ticker `Run` içinde bir kez
kuruluyor, `Reset` ile canlı yapılabilir. Bu fazda **yapılmıyor** —
sebebi kapsam değil, değer: yazma aralığını değiştirmek bir destek
çağrısının ihtiyacı değil, bir performans ayarı, ve yarım bir canlılık
eklemek yerine bir sonraki fazda düzgün ölçülerek yapılması daha
doğru.

##### Önce düzeltilecek üç tutarsızlık

Fazı yazarken kayıt ile gerçeği karşılaştırınca çıktılar:

1. **`logs.level` canlı okunuyor ama `Live: false`.** Beacon her turda
   `slog.LevelVar` üzerinden uyguluyor. Panel müşteriye "yeniden
   başlatın" diyor, gerekmiyorken.
2. **`logs.verbose_until` de canlı okunuyor** ve durum daha kötü: testin
   listesinde hiç yok, yani kontrol ona hiç bakmıyor.
3. **Testte gerekçesiz bir muafiyet var:** `if !def.Live && key !=
   "logs.level"`. Kuralı yakalaması gereken testin, tam da yakalayacağı
   satırı elle atlaması. Muafiyet kalkıyor.

Ayrıca **log seviyesi collector'da hiç canlı değil** — beacon'da var,
collector'da yok. İki süreç de log yazıyor; asimetri kaza.

##### Bitti ölçütü

Kayıt girdileri; collector'ın `applySettings`'i dördünü de uyguluyor;
göç komutu dosyadan taşıyor; **canlı değişimin gerçek eşzamanlı yük
altında çalıştığı ölçülüyor** — bağlantılar akarken listeye ülke
ekleniyor ve engellenmeye başladığı görülüyor, `-race` altında.

##### Ne yapıldı, ve yazarken çıkan dört hata

Dördü de kayıt girdisiyle, canlı uygulamayla ve göç girdisiyle bağlandı;
üç `Live` tutarsızlığı ile testteki gerekçesiz muafiyet kalktı;
`GeoBlocklist` `atomic.Pointer` ile değiştirilebilir hâle geldi ve `nil`
optimizasyonunun yerini `Active()` aldı; collector'ın log seviyesi de
canlı oldu (`_ = logControls` satırı, collector'ın kendi `Controls`'unu
kurup çöpe attığı yerdi).

**Ölçüm:** yük testi 8 işçiyle kesintisiz bağlantı akıtırken liste
değişiyor — engelden sonra açılan **~11.000 bağlantının 0'ı** sunuldu,
engel kalkınca yeniden sunulmaya başladı, `-race` altında.

Yazarken çıkan ve kayda değer dört hata (dördüncüsü üç ayrı testte):

1. **Yük testinin ilk hâli hiçbir şey kanıtlamıyordu.** Üç ayrı atış
   yapıyordu ve değişimle yarışan atış "0 sunuldu" diyordu — bu, "engel
   anında etki etti" ile "goroutine `Set` döndükten sonra çalıştı, o
   sırada uçan hiç bağlantı yoktu" ile aynı sonucu veriyor. İlginç şey
   olmuş olsa da olmasa da geçen test, kanıt değildir. Kesintisiz akışa
   çevrildi.
2. **Sonra o test gerçek bir yanlış atfı yakaladı:** "engelliyken
   sunuldu" denen 3 bağlantı, engel *kalktıktan* sonra çevrilmiş
   olanlardı. Deneme artık iki ucundan da damgalanıyor; değişimi
   ortasında yakalayanlar hiçbir tarafa yazılmıyor (her seferinde tam
   `işçi × değişim` = 16 tane, ki bu da damganın çalıştığının kendi
   kontrolü).
3. **`known_bot_asns` canlıydı ama uygulanmıyordu.** `applySettings`
   listeyi *uzunluğuna* bakarak karşılaştırıyordu; bir ASN'yi başkasıyla
   değiştirmek uzunluğu korur. Yeni liste veritabanında durur, panelde
   güncel görünür, skorlamaya hiç ulaşmazdı. Artık **her turda
   uygulanıyor, karşılaştırma yalnızca "log yazayım mı" sorusuna
   bakıyor** — bir güvenlik ayarının doğruluğu, kendi değişim
   dedektörünün isabetine bağlı kalmamalı.
4. **Entegrasyon testleri ikinci koşuda kırmızıydı.** İki test yalnızca
   daha önce çalışmadıkları bir veritabanında geçiyordu; kendi
   yazdıklarını temizlemedikleri için ikinci koşuda kendi izlerine
   takılıyorlardı. Testin en kötü yanlış olma biçimi bu: yazarken yeşil,
   sonraki kişide kırmızı. `clearSiteSettings` ile önce ve sonra
   temizleniyor.

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

**Kalan kablolama** (mekanik, A5.2): geo blok listeleri, bot skor eşiği,
flush aralığı. Collector'ın canlı okuyucusu ve limitler A5.1'de yazıldı.

##### Eski not



**Ne:** collector ve beacon, ayar satırını kısa aralıkla (bir dakika
uygun) yeniden okur ve canlı değerleri atomik olarak değiştirir.

**Dosyalar:** `internal/collector/live.go` (yeni), `cmd/*/main.go`

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

#### A7 — IP saklama modu: tam veya maskeli ✅ **yapıldı**

**Ne:** IP adresi KVKK/GDPR anlamında kişisel veridir ve bu faza kadar
iki tabloda da tam olarak saklanıyordu. Artık ayar:

| Mod | Ne saklanır | Ne kaybedilir |
|---|---|---|
| `full` | Adresin tamamı | — |
| `masked` **(varsayılan)** | IPv4 son oktet sıfırlanır (/24), IPv6 /64'e kırpılır | Kesişim birleşimi zayıflar, "şu IP ne yaptı" görünümü bir aralığa bakar |

**Karar — hukukçudan geldi:** varsayılan **maskeli**. Bu, planın açık
kararlarından biriydi ve artık kapalı. Sunucular bize ait olacağı için
varsayılanın güvenli tarafta durması gerekiyor: bir kurulumu yapan
kişinin hiçbir şey seçmemesi, en çok veri saklayan moda düşmek anlamına
gelmemeli.

**Sıralama, bu maddenin can alıcı yeri:** maskeleme **yazma anında**
uygulanır, ama **son adımda**. Tam adres önce ziyaretçi kimliğini
türetir ve ülke/ASN'yi çözer, sonra maskelenip satıra yazılır. Ters
sırada yapılsaydı maskeleme sessizce coğrafyayı ve ziyaretçi sayımını da
bozardı — kimse fark etmeden. Tam adres belleğin dışına hiç çıkmaz:
diske değen değer zaten maskelidir.

**Dikkat — bunun bir maliyeti var ve gizlenmemeli:** maskeli modda
`beacon_events.ip` ile `traffic_snapshots.ip` birleşimi zayıflar. O
birleşim projenin ayırt edici tezi (§0). İki taraf da aynı biçimde
maskelendiği için birleşim **çalışmaya devam eder**, ama /24 çözünürlükte
— aynı aralıktaki iki farklı ziyaretçi tek satır gibi görünebilir.
Kesişim görünümleri bunu söyleyecek; sıfır göstermeyecek, gizlemeyecek
(§D5 kuralı, sahibi **D5**).

**Geçmişe dönük değil, ve bu kayıt altında:** mod değiştiğinde eski
satırlar olduğu gibi kalır. Panelin "hepsi maskeli" diyebilmesi için
değişimin *ne zaman* olduğunu bilmesi gerekir — bu ayrı bir sütun
gerektirmez, çünkü ayar değişimi zaten denetim kaydına (`panel_audit_log`)
yazılıyor ve o kayıt silinemez.

**Bitti ölçütü:** ✅ maskeleme yazma anında uygulanıyor; ✅ ülke/ASN ve
ziyaretçi kimliği tam adresten türetiliyor; ✅ ayar canlı (yeniden
başlatma gerekmiyor); ✅ gerçek tarayıcıyla, gerçek veritabanında iki mod
da doğrulandı.

---

#### A7.5 — Geliştirici şifresi kapısı ✅ **yapıldı**

**Ne:** Geliştirici ayarlarının **hepsi** değil, yalnız **hukuki sorun
çıkarabilecek olanları** her değiştirilişinde ayrı bir şifre ister.
Şifre yapılandırma dosyasından gelir, **hash'li** saklanır ve **her
seferinde** sorulur — oturum yok, "5 dakika açık kalsın" yok.

**Neden ayrı bir şifre:** panel şifresi "sen kimsin" sorusunu
cevaplıyor. Bu şifre başka bir soruyu cevaplıyor: "bu değişikliği
yapmaya yetkin var mı". İkisi aynı anahtar olursa, panele girmiş
herkes — müşteri, müşterinin stajyeri, oturumu çalınmış bir yönetici —
saklanan kişisel verinin kapsamını sessizce değiştirebilir. Uyarı metni
bunu açıkça söyler: *geliştirici, hukuki sorun çıkarabilecek alanlar
için şifre kuralı getirdi.*

**Mekanizma, `internal/devgate` — bilerek ayrı paket.** Panelin veri
katmanını sürüklemeden istenen yere import edilebilir. Veritabanı
bilmez, HTTP bilmez; sadece doğrular.

| Kural | Nasıl zorlanıyor |
|---|---|
| Şifre asla düz metin durmaz | Yapılandırmada yalnız `password_hash` var. Düz metin alanı **açılışta reddedilir**, yok sayılmaz |
| Her seferinde sorulur | `Verify` bir oturum değil, **saniyeler ömürlü** bir yetki döndürür. Saklanan bir yetki yaşlanır ve reddedilir |
| Yetki taklit edilemez | `Authorization` yapısının geçerlilik alanı dışa kapalı. Başka bir paket geçerli bir tane **üretemez** — derleyici zorluyor, kural değil |
| Yetki başka işe kaydırılamaz | Yetki hangi ayar için alındıysa ona bağlıdır. Bir ayar için alınıp başkasına kullanılamaz |
| Şifre yoksa kapı kapalıdır | Hash tanımlı değilse korumalı ayar **değiştirilemez**. Varsayılan zaten güvenli değer olduğu için bu, kaybedilen bir yetenek değil |
| Denemenin maliyeti sınırlı | argon2 çağrıları seri, kuyruk sınırlı, ardışık hatalar pencereli sayaçla durduruluyor. Yoksa kapının kendisi 19 MiB'lık bir DoS aracı olurdu |
| Her deneme kayda geçer | Başarılı da başarısız da: `auth` kategorisine `logging.Attempt`, denetim kaydına ayrı eylem |

**Korumalı ayarlar** (bu listeyi genişletmek kod değişikliğidir):

| Ayar | Neden hukuki |
|---|---|
| `privacy.ip_storage` | Kişisel verinin ta kendisi |
| `analytics.retention_days` | Ne kadar süre saklandığı |
| `logs.retention_days`, `logs.important_retention_days` | Loglar IP içerir; kısaltmak da uzatmak da hukuki karar |
| `campaign.drop_params` | `utm_term` gerçek arama metni taşıyabilir |
| `campaign.extra_params` | İçeriğini bizim denetlemediğimiz alanları saklamak |
| `campaign.store_click_ids` | Tıklama başına benzersiz, kalıcı tanımlayıcı |

`logs.level` bilerek **korumasız**: bir destek çağrısının ilk hamlesi o,
ve debug'a çıkmak saklanan veri kümesini değiştirmez.

**Zorlama noktası — kritik:** kapı HTTP katmanında değil, **tek yazma
yolunda**. `SetSetting` korumalı bir anahtarı reddeder; onları yalnız
`SetGuardedSetting` yazabilir ve o da geçerli bir `Authorization`
olmadan çalışmaz. Yarın başka bir çağrı yeri eklenirse kapıyı atlaması
mümkün değil — unutmakla değil, derlememekle sonuçlanır.

**Bitti ölçütü:** ✅ hash'li; ✅ her seferinde soruluyor (saklanan yetki
reddediliyor, testle kanıtlı); ✅ yanlış şifre yazamıyor; ✅ şifre yoksa
korumalı ayar yazılamıyor; ✅ gerçek tarayıcıdan gerçek form gönderimiyle
doğrulandı.

---

#### A7.6 — Görünürlük ile yazılabilirliği ayırmak ✅ **yapıldı**

**Ne:** Bir ayarı *görmek* ile *değiştirmek* aynı soru değil. A7.5'te
kapıyı kurarken bu ikisi karışmıştı: `Developer: true` olan ayarlar
müşteriye hiç görünmüyordu ve korumalı olanlar "hata" veriyordu. Doğrusu
şu: **hepsi görünür, bir kısmı tıklanamaz.**

**Neden gizlemiyoruz:** göremediği bir ayarı müşteri soramaz. Kendi
kurulumunun neden öyle davrandığını açıklayamaz duruma düşer ve her
seferinde bize sormak zorunda kalır. Değeri göstermek, kontrolü
göstermemek doğru olanı söylüyor: *bu karar verilmiş, bizim tarafımızdan
verilmiş, ve değeri şu.*

| Durum | Kim görür | Ne görür |
|---|---|---|
| `writable` | herkes (yetkisi varsa) | normal kontrol |
| `gated` | **yalnız işletmeci** | kontrol + her seferinde şifre |
| `locked` | müşteri | değer + 🔒 + hukuki gerekçe, kontrol yok |
| `read_only` | müşteri | değer + açıklama, kontrol yok |

**Şifre alanı müşteriye hiç gösterilmez** ve bu estetik bir tercih
değil: sahip olamayacağı bir şifrenin kutusunu görmek onu şifreyi
aramaya davet eder, ve her denemesi **işletmecinin** hata bütçesinden
harcar. Beş yanlış deneme, sorumluluğu bize ait bir kurulumda bizi
dışarıda bırakırdı — güvenlik kontrolü kılığında bir hizmet reddi.
Bu yüzden müşterinin denemesi **kimliğe bakılarak**, argon2'ye hiç
gitmeden reddediliyor.

**Zorlama noktası:** `ApplySetting` ve `ClearSetting` yetkiyi
*yetkilendirmeden önce* kontrol ediyor. Müşteri geçerli bir yetki
nesnesi ele geçirse bile yazamaz — reddin gerekçesi ne yazdığı değil,
kim olduğu.

**Bitti ölçütü:** ✅ müşteri hepsini görüyor; ✅ kontrol yok; ✅ şifre
alanı yok; ✅ kilit ve gerekçe yazılı; ✅ elle kurulmuş POST doğru
şifreyle bile yazamıyor; ✅ gerçek tarayıcıda iki taraf da sürüldü.

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

#### A10 — Veriyi dağıtmak yerine getirmek ✅ **yapıldı** *(lisans)*

**Karar (kullanıcı):** *"Biz dağıtmayacağız, veri toplama sistemlerini
de vereceğiz. Kendileri crona mı bağlar, panelden manuel mi yapar, bizi
ilgilendirmez."*

**Sorun.** Depo public ve izin verici lisanslı *(o gün MIT'ti; müşteri
kararıyla Apache-2.0'a geçti — gerekçesi §0.6'da)*. İçinde başkasının verisinin bir anlık
görüntüsü duruyor: `internal/scoring/known_bots.json` — The Bot
Aquarium'un topluluk arşivinden 2026-07-21'de alınmış 51 kayıt.
README kaynağı söylüyor ama **hangi şartlarla yeniden dağıtabildiğimizi
söylemiyor.** Kendi lisansımız kendi kodumuzu kapsar; başkasının
verisini kapsamaz. Bir depo hem "MIT, herkes kullanabilir" deyip hem
içinde şartları yazılmamış üçüncü taraf verisi taşıyamaz — bunu
klonlayan herkes aynı belirsizliği devralır.

**Çözüm dağıtmamak, ama yeteneği de kaybetmemek.** Anlık görüntü
depodan çıkıyor; yerine **getirme sistemi** giriyor. Veri, kurulumun
kendi makinesinde, kurulumun kendi kararıyla, kaynağın kendi şartları
altında iniyor. Biz hiçbir veri kümesini yeniden dağıtmıyoruz.

**Nasıl çalıştırılacağı bizi ilgilendirmiyor** ve bu bir tasarım
kısıtı: mekanizma komut satırından çalışacak (cron'a bağlanabilsin),
ve sonradan panelden tetiklenebilecek biçimde bir pakette duracak.

**Yapılacaklar:**

1. **`internal/botdata`** — indir, ayrıştır, `browser` sınıflı kayıtları
   ele (onları katmak gerçek tarayıcıları bot işaretlemek olurdu),
   **kendini anlatan** bir dosyaya atomik yaz (kaynak, alınma zamanı,
   sayı), geri oku. **Dosyanın olmaması hata değil, boş küme.**
2. **`scoring.KnownBotJA4` global'ini sökmek.** Bugün paket düzeyinde
   bir `var` ve onu `internal/api` üç yerde okuyor. Enjekte edilir hâle
   gelmesi lazım — çünkü artık "hiç alınmamış" geçerli bir durum ve o
   durumda küme boş olmalı, gömülü bir dosyadan gelmemeli.
3. **`collector -update-bot-data`** — cron'a bağlanacak komut. Ne
   yaptığını yazar.
4. **`[bot_data]` yapılandırması** collector ve okuma API'sinde.
5. **Görünürlük:** preflight kontrolü ve sihirbaz satırı, **"hiç
   alınmadı" ile "alındı ve boş"u ayırarak** — bu projenin her yerde
   koruduğu "baktık/bakmadık" ayrımı.
6. **`THIRD-PARTY.md`** — her bağımlılığın lisansı, ve açık kural:
   *bu depo hiçbir üçüncü taraf veri kümesini yeniden dağıtmaz.*

**Kabul edilen kayıp, açıkça:** hiçbir şey yapmayan bir kurulumda
bilinen-bot listesi **boş** olur. Skorlama diğer sinyallerle çalışmaya
devam eder ama bu sinyal gelmez. Bu, dağıtmamanın dürüst bedeli ve
kurulumun bunu görmesi gerekiyor — 5. madde bunun için var.

**Panelden tetikleme bu fazda yok, ve sebebi teknik:** veriyi yazan
collector'ın rolü, panelin rolü değil. Panelin collector'a iş
söyleyebilmesi B3'ün operasyon kanalını gerektiriyor. Bugün cron ve
komut satırı var; kullanıcı zaten bunu yeterli buldu.

**Yapıldı.** `internal/botdata` (getir/yaz/oku), `scoring.KnownBots`
(global söküldü, nil küme anlamlı), `collector -update-bot-data`,
`[bot_data]` yapılandırması, `data.bot_fingerprints` preflight kontrolü
ve elle-yapılacaklar satırı, `THIRD-PARTY.md`. Anlık görüntü depodan
silindi.

**Canlı kaynağa karşı doğrulandı** ve ilk faydalı şeyi hemen buldu:
kaynakta artık **52** kullanılabilir kayıt var, depodaki anlık
görüntüde 51 vardı; 59 kayıt da tarayıcı olduğu için eleniyor. Yani
dağıttığımız kopya zaten geride kalmıştı.

---

### AI.2 — Paket bölme (C4 öncesi ara iş) ✅ **bitti**

**Neden C4'ten önce:** ölçüm `internal/panel`'i 4.424 satır / 16 dosya
gösterdi ve içinde en az dört ayrı sorumluluk var — kimlik doğrulama,
yetki ve ayarlar, denetim, kurulum kontrolleri. Sonra bölmek, önce
bölmekten pahalı: her faz o pakete bir şey daha ekliyor ve her ekleme
taşınacak yüzeyi büyütüyor.

**Bu fazda iki kesim** — en yüksek değerli ve en az dolaşık olanlar:

1. **`preflight` kendi paketine.** 985 satırla deponun en büyük dosyası,
   ve aslında bir *tanı aracı*, alan kuralı değil: veritabanına gerçek
   sorgular atıp ne bulduğunu söylüyor. `Store`'a değil doğrudan
   havuza bağlanması yeterli, ki bu da kesimi temiz yapan şey.
   `internal/panel` 4.424 → ~3.400 satıra iner.
2. **`internal/config` → `internal/collector`.** İsim yanıltıcı: global
   duruyor, aslında yalnız collector'ın. Beacon, API ve panelin her
   birinin config'i kendi paketinde. Yeni gelen yanlış tahmin eder.
   Ucuz: yalnız `cmd/collector` import ediyor.

**Bu fazda yapılmayan, ve neden:** ayarlar ailesi (1.531 satır) ve kimlik
ailesi `Access`, `Principal`, `Role` ve `Store` tiplerini paylaşıyor.
Onları bölmek ya bu tipleri ortak bir yere taşımayı ya da her alt paketin
kendi store'unu tutmasını gerektiriyor — ikisi de her çağrı yerini ve
tüm entegrasyon testlerini değiştiriyor. Tek seferde yapılacak iş değil,
ve yarım yapılırsa şu ankinden kötü. **Sırası:** C4'ten sonra, kimlik
ailesi C4'ün eklediği yüzeyle birlikte yerine oturduğunda.

**Ayrıca ölçülüp yapılmayan bir şey:** `internal/panel`'in `net/http`
import etmemesi gerektiği `panel/web`'in paket yorumunda yazıyor ama
**doğru değil** — `session.go` CSRF için, `devpassword.go` form alanı
için istek alıyor. `preflight` çıkınca üçten ikiye iner. Tamamen
kaldırmak CSRF'in şeklini değiştirmek demek; yazılı kuralı gerçeğe
uydurmak için o yorum düzeltildi, kural yapısal teste bağlanmadı.

**Bitti ölçütü:** `go test -race ./...` temiz, entegrasyon paketi gerçek
veritabanına karşı temiz, dört binary derleniyor, ve `internal/panel`
ile `internal/preflight` arasındaki sınır paket yorumlarında yazılı.

#### Ne oldu

`internal/panel` **4.424 → 3.397 satır**. `internal/panel/preflight`
1.119 satır, ve `internal/panel`'i **import etmiyor** — bu ölçütün son
maddesi yorumda kalmadı, `TestPreflightDoesNotImportThePanel` ile
`go/build`'den okunuyor. Yorum yazılır ve yanlışlanır; test yanlışlanmaz.

Kesimi temiz yapan iki karar:

- **`Checker` havuz alıyor, `Store` değil.** İki alanı var: `*pgxpool.Pool`
  ve `ipTokenKeyConfigured`. Kontroller panelin sormadığı şeyleri
  soruyor — *başka* bir rolün ne yapabildiğini, panelin okuyamadığı bir
  şemada hangi tabloların olduğunu — ve bunları `Store` metodu yapmak
  panelin veri API'sini yalnız tek bir sayfaya hizmet eden bir düzine
  fonksiyonla büyütürdü.
- **`GuardedKeys` çağrı yerinden geliyor.** Kontrolün ihtiyacı olan tek
  panel bilgisi buydu; `Config`'e bir `[]string` alanı olarak taşındı.
  Alternatifi `preflight`'ın `internal/panel`'i import etmesiydi, ki o da
  kontrol çalıştıran her binary'ye panelin store'unu, oturumlarını ve
  kimlik doğrulamasını sürüklerdi. Kural şu: bir kontrolün panelden bir
  şeye ihtiyacı varsa `Config`'ten geçer.

**Bölmenin açtığı yeni hata yolu, ve kapatılması:** kontroller `Store`
metoduyken havuzsuz bir `Store` yoktu. Bağımsız bir `Checker` boş
kurulabiliyor, ve bunu keşfedecek yer sihirbazın son adımı — teslimden
bir düğme öte panik. Şimdi havuzsuz bir `Checker` her veritabanı
kontrolünü "bakılmadı" diye bildiriyor ve teslim yine kapalı kalıyor
(atlanan zorunlu kontrol `Complete`'i bloke ediyor). `nil` bir `*Checker`
de aynı yoldan geçiyor. `TestRunSurvivesWithoutADatabase` ikisini de
tutuyor.

**Yol üstünde bulunan, ilgisiz bir kırık:** `internal/api`'nin entegrasyon
testi A10'da silinen `scoring.KnownBotJA4` globalini kullanmaya devam
ediyordu — `-tags integration` olmadan derlenmediği için fark
edilmemişti. Test artık kendi `KnownBots` kümesini veriyor, ki zaten
doğrusu oydu: getirilmiş bir dosyanın içeriğine bağlı bir test, dosya
boşsa kendini atlar.

**Bölmenin sarsıp ortaya çıkardığı iki yarış:** ikisi de bölmenin
sebep olduğu şey değil, ikisi de gizliydi. `internal/panel`'den 1.000
satır çıkınca paketin süresi değişti, paralel paketlerin sırası değişti,
ve ikisi de patlamaya başladı. Tek veritabanı, `go test ./...`'in
paketleri paralel çalıştırması, ve testlerin küresel olmayan varsaydığı
iki küresel şey:

1. **İki takım da aynı ayar satırlarını yazıp tabloyu tümden
   siliyordu.** `internal/settings` canlı testi ile `internal/panel`
   ayar testi aynı gerçek anahtarları (`logs.retention_days`) yazıyor,
   ikisi de `DELETE FROM panel_settings` ile bitiyordu. Artık her
   satırın sahibi var: canlı takım `test.settings.` önekini kullanıyor
   ve yalnız onu siliyor, panel `test.` dışındaki her şeyi siliyor.
2. **"Bu kurulumda hiç hesap yok" kontrol edilip güvenilebilecek bir şey
   değil.** `TestSetupFlow` ilk çalıştırma akışını yürüyor; `if
   CountUsers() == 0` koruması yazıldığı haliyle onarılamaz, çünkü
   sayımla sayfanın üretilmesi arasındaki boşluğa panel takımı bir hesap
   yazabiliyor. Üç koşudan birinde patlıyordu. Bir testin *süresi
   boyunca* geçerli kalması gereken koşula kontrol değil kilit gerekir:
   iki takım artık bir Postgres advisory lock alıp sırayla giriyor.
   Bağlantı `Acquire` ile sabitleniyor (advisory lock oturuma ait; havuz
   başka bağlantı verirse kilit sızar), ve kilit bağlantı havuza
   dönmeden bırakılıyor.

Yardımcı iki pakette de kopya duruyor — aralarında test-only bir paket
yok ve yalnız bir test yardımcısı için paket açmak altmış satır
kopyadan kötü. Anlaşması gereken tek şey sabit, ve iki tarafta da bir
test onu literal'e karşı doğruluyor: sessizce ayrışan iki kopya, iki
takımı da yeşil bırakıp yarışı geri getirirdi.

**Ölçüm, sonra:** 22 iç paket, 12'si yaprak, **döngü yok**, en yüksek
fan-out `cmd/collector` (11) — bir binary'nin her şeyi bağlaması
beklenen şey. `internal/panel`'i yalnız iki yer import ediyor:
`cmd/panel` ve `internal/panel/web`.

---

### AI.3 — Güvenlik denetimi (C3 sonrası ara iş) ✅ **bitti**

**Neden burada:** *(kullanıcı isteği: "açık iç olmamalı ... en prestijli
listelere göre bak hangi açıklar var bizde onları fixle")* — ama zamanı
tesadüf değil. C2, C3 ve C4 bu panele **kimlik doğrulaması olmadan**
internetten erişilebilen ilk yüzeyi verdi: giriş formu, davet bağlantısı,
geliştirici bağlantısı, ilk çalıştırma sayfası. Bir faz önce yapılacak
denetim daha küçük bir sistemi ve riskin çok daha küçük bir kısmını
bulurdu.

**Ölçüt:** OWASP Top 10 (2021), CWE Top 25, ve kendi barındırılan bir
yönetim paneline uygulanan ASVS maddeleri. Sonuç `SECURITY.md`'de:
düzeltilenler sınıflarıyla, **bakılıp doğru bulunanlar**, ve açık
bırakılanlar.

**Sıra:** önce `govulncheck`, sonra okuma. Aracın bir insandan daha iyi
yaptığı tek kısım o ve denetimin en yüksek önemli iki bulgusunu bir
dakikanın altında üretti.

#### Düzeltilen sekiz bulgu

| # | Bulgu | Sınıf |
|---|---|---|
| 1 | `pgx/v5` 5.7.6 — dolar-tırnaklı literal içinde `$1` yer tutucu sanılıyor; **parametreli sorguyu enjekte edilebilir hâle getiriyor**. `govulncheck` *erişilebilir* dedi: `api.Store.BeaconSites` | A06, CWE-89 |
| 2 | `x/text` 0.24.0 — geçersiz girdide sonsuz döngü; `ui.Formatter.Title` üzerinden erişilebilir, ki panel bunu **kullanıcının yazdığı isme** uyguluyor | A06, CWE-835 |
| 3 | Panel: her `POST` **kimlik doğrulamadan önce** sınırsız gövde okuyor. Ölçüldü: 64 MiB gövde ≈ 128 MiB heap — ve *sonra* CSRF jetonu yok diye reddediliyor | A05, CWE-770 |
| 4 | Panel: ayar yazımı başarısız olunca pgx hata metni (kısıt adı, SQLSTATE, bazen sorgunun kendisi) müşterinin sayfasına basılıyor | A04, CWE-209 |
| 5 | Panel: kimliksiz erişilen yollar `Store`'u kontrol etmeden kullanıyor — yanlış yapılandırılmış sunucu **uzaktan çöküyor** | CWE-476 |
| 6 | API: tek bir SQL tanımlayıcısı interpolasyonla giriyor (Postgres'te sütun adı için yer tutucu yok) ve koruma "yalnız sabit geçirin" diyen bir **yorumdu** | CWE-89, gizil |
| 7 | Panel: aşırı büyük gövde "CSRF jetonunuz eskimiş" diye görünüyor — hata mesajı yanlış yeri gösteriyor | A09 |
| 8 | Panel: yukarıdaki 413 **500 sayfasını** çiziyordu ("hata sunucu tarafında kaydedildi") — çünkü render katmanı sözü olmayan statü için sessizce geri düşüyor. Bu denetimin *kendi düzeltmesindeki* kusur; belgelenirken bulundu | A09 |

1 ve 2 yükseltmeyle kapandı (modül Go 1.25'e taşındı). Bunu açıkça
yazmak gerek: **denetimin en kötü iki maddesi bağımlılıklardaydı, burada
kimsenin yazdığı kodda değil** — ve buna karşı savunma dikkat değil,
aracı düzenli çalıştırmak.

#### Alınan kararlar

- **Gövde sınırı middleware, handler başına değil.** "Her handler
  hatırlar" tam da burada bozulan özellik: pakette sekiz `ParseForm`
  çağrısı var ve üç ayrı fazda, her seferinde başka bir şey düşünen biri
  tarafından yazıldılar.
- **`acceptPost`: önce ayrıştır, sonra jetonu kontrol et.** `CheckCSRF`
  bir form alanı okuyor, yani gövdeye ilk dokunan oydu. Sıra düzeltilince
  boyut hatası boyut olarak bildiriliyor (413), CSRF olarak değil (419).
- **Sentinel, konvansiyon değil.** `ErrInvalidSetting` ile `errors.Is`
  soruyor; konvansiyon her gelecek çağrı yerinin "bu hata insan için mi
  yazıldı" sorusunu doğru tahmin etmesi demekti.
- **`RequireStore` blanket middleware'i yanlış şekildi.** On bir render
  testini bozdu ve bozma nedeni tam da yanlışlığın nedeni: **kendi
  stil dosyasını sunamayan panel 503 sayfasını biçimsiz metin olarak
  çiziyor.** Koruma `haveStore()` olarak her handler'ın satıra gerçekten
  ihtiyaç duyduğu yere taşındı.
- **Yorum yerine kapalı tip.** İnterpolasyonla giren sütun adı artık
  dışa kapalı değerleri olan bir tip; istekten türeyen bir dize oraya
  ulaşamıyor ve kontrolü atlayan yeni bir sütun **derlenmiyor**.

#### Yanlış nedenle geçen iki test

- **DoS testi kendi `strings.Repeat`'ini ölçtü** — 34 MB "büyüme"
  gövdeyi ayıran testti. Akış yapan bir okuyucuya çevrildi, ve iddia da
  değişti: sabit bir tavan değil (ilk şablon büyümesinde çürür),
  **"maliyet gövde boyutuyla ölçeklenmeyi bırakıyor"**.
- **CSRF testi 303'ü "oldu" saydı** — oysa giriş formuna yönlendirme bir
  *reddir*. Artık 303 yalnız `Location` giriş yoluyla başlıyorsa kabul
  ediliyor, ve test entegrasyon paketine taşındı: store'suz sunucu her
  şeye 503 döndüğü için aynı test birim paketinde **tek bir handler'a
  ulaşmadan** geçerdi.

#### Açık bırakılanlar (ve etkiledikleri sayfada yazılı)

- Şifre değişikliği diğer cihazlardaki oturumları kapatmıyor — oturum
  tablosunda kullanıcı sütunu yok, bulmak bugün tablo taraması demek.
- 2FA kurtarma kodu yok; kurtarma sahip ya da işletmeci eliyle.
- Global eşzamanlılık sınırı yok — her giriş denemesi bir argon2id
  doğrulaması, sınır kuyruk değil throttle sayaçları.

**Bitti ölçütü:** `govulncheck` temiz, sekiz bulgunun her biri kendi
testini taşıyor, `go test -race ./...` ve entegrasyon paketi temiz,
`SECURITY.md` yazıldı.

---

### B. Gözlemlenebilirlik — SSH'i gereksiz kılan şey

#### B1 — `panel_logs` ✅ *(2026-08-30, B2 ile birlikte)*

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

#### B2 — `panel_operations` — operasyon günlüğü ✅ *(2026-08-30, B1 ile birlikte)*

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

**Neden B1 ile tek faz:** `panel_logs.operation_id` içindeki değer
`panel_operations.id`'dir. Ayrı yapılsalardı ya sütun kimsenin
dolduramadığı boş bir söz olurdu, ya da kimliğin bağlanacağı satır
olmazdı — ve ikinci hâlde kimliğin işe yarayıp yaramadığı B1 gelene
kadar anlaşılmazdı. Bir sütun iki tablonun arasındaysa, o iki tablo tek
bir fazdır.

**Ne yapıldı:**

- `internal/logsink` — tamponlu kanal, tek yazıcı goroutine, tampon
  dolunca düşürme ve sayma. Veritabanı asla istek yolunda değil.
  Ölçüldü: tampon 1, 5000 satır, 2.7 ms; aynı kod bloklamaya
  çevrildiğinde 1.88 saniye.
- `internal/panel/operations.go` — `BeginOperation` / `Step` / `Values`
  / `LinkAudit` / `Finish`. Üç değerli sonuç (`succeeded` / `failed` /
  `refused`) ve üç durumlu `rolled_back` (`NULL` = hiçbir şey
  uygulanmamıştı). Kimlik Go'da üretiliyor ve rastgele.
- Ayar değişimi uçtan uca bağlandı: işlem kaydı işten *önce* açılıyor,
  çünkü kimliğin işin üreteceği satırlara iliştirilecek zamanı olmalı.
- Şema bölündü: `panel_logs` → `internal/logsink/schema.sql`,
  `panel_operations` → `internal/panel/schema.sql`. Bir tablonun şeması
  onu yazanların yanında durur (`internal/heartbeat` emsali).

**Bitti ölçütü karşılandı:** düşmanca dize dört şekilde denendi (NUL,
geçersiz UTF-8, 1 MB tek satır, satır bölme) — hepsi yazıldı, hiçbiri
gönderildiği gibi saklanmadı; seviye tabanı ölçüldü; satır düzeyi
politikaları dört rolle uçtan uca ölçüldü ve teste bağlandı.

**Ayrıntılı anahtarın kendi kendine sönmesi** B1'in bitti ölçütünde
yazıyordu ve `internal/logging.Controls.Apply` ile zaten çalışıyor;
`logsink` seviyesini onunla *paylaşıyor* (aynı `*slog.LevelVar`), yani
süre dolduğunda ağaçla birlikte bu tablo da kısılıyor. Anahtarı panelden
açan yüzey D4b'de.

**Yeni kapı *(2026-08-30)*: ulaşılmayan kod.** `deadcode` bir kez elle
koşturulunca üç gerçek kusur çıktı — iki süpürme ve `LinkAudit` — ve
üçü de hiçbir testin göremeyeceği cinstendi, çünkü ulaşılmayan kod
başarısız olamaz. `internal/sast/cmd/deadcodediff` gosec'in
`sastdiff`'iyle aynı şekilde çalışıyor ve CI'da: gerekçeli muafiyet
listesi, iki yönlü kontrol, gerekçesiz girdi reddediliyor.

**Bu fazda bulunan iki kusur:** testlerin `postgres` olarak bağlanması
(hiçbir politikayı sınamıyorlardı) ve `errorChain`'in bütün-zincir
iddiasının korumasız olması. İkisi de NOTES.md'de.

**Kapanış düzeltmesi *(2026-08-30, aynı gün)*:** faz erken ✅
işaretlenmişti. Yazıcı hiçbir binary'ye bağlı değildi — dört `main.go`
da ağaç logger'ını kuruyor, `logsink` `cmd/` içinde hiç geçmiyordu, ve
`panel_logs` gerçek bir kurulumda kalıcı olarak boştu. Bitmiş bir faz
çalışan bir sistem değildir.

Kapatılırken çıkan ikinci şey daha büyüktü: **panelin hiç periyodik işi
yoktu**, ve bu yüzden çok önce yazılmış iki süpürme
(`PurgeOldLoginAttempts`, `PurgeOldDevAccess`) hiçbir yerden
çağrılmıyordu. Sınırladıkları tablolar yazıldıkları günden beri sınırsız
büyüyormuş.

Şimdi: `logsink.Attach` dört serviste; `internal/panel/housekeeping.go`
dört süpürmeyi saatlik tek giriş noktasında topluyor; `panel_logs` 14
gün, `panel_operations` 30 gün. Süpürme `started_at`'e bakıyor —
`finished_at` olsaydı bitmemiş operasyonlar hiç silinmezdi. Yapısal
testler hem "her süpürme çağrılıyor mu" hem "döngü main'den
başlatılıyor mu" sorularını AST'den soruyor.

---

#### B3 — Onarım operasyonları (39 adet)

**Ne:** Adlandırılmış, tipli, sınırları doğrulanmış Go fonksiyonları.
Tam katalog aşağıda §4'te. Her biri günlüklenir.

**Kalıcı olarak yasak:** "SQL çalıştır" kutusu; yapılandırma metin
alanı; kabuk komutu; komut dizesi alan bir "yeniden başlat"; parametresi
kod, sorgu, dosya yolu veya panelin bağlanacağı bir alan adı olan
herhangi bir operasyon.

---

#### B4 — Sistem sağlığı sayfası ✅ *(2026-08-27)*

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

> **Bu öncül yarı yanlış çıktı.** Beacon için doğruydu
> (`accepted`/`rejected`/`dropped`/`written` hepsi vardı); collector'da
> **hiç sayaç yoktu** ve `storage.Writer`'a eklendi. Daha önemlisi:
> **collector'dan panele hiçbir kanal yoktu.** Collector'ın HTTP sunucusu
> yok — olmamalı da, saldırgan baytına dokunan süreç o — ve yazdığı
> tabloyu panelin rolü okuyamıyor.

##### Yapılan: kanal bir satır (`internal/heartbeat`)

`/healthz` "bu süreç şu an ayakta" der. B4'ün sorduğu ise *"son yazma
başarılı oldu mu, ne zaman"*. Farklı sorular, ve canlılık ucu ikincisini
cevaplayamaz. Bir müşteriye bir haftalık veri kaybettiren arıza, ayakta
olan ve salıdan beri her yazması başarısız olan bir collector'dır.

Her servis dakikada bir kendi satırını yazıyor: sürüm, başlangıç zamanı,
sayaçlar, son hata. **Hiçbiri ziyaretçiden türetilmiş değil** — adres
yok, site kimliği yok, yol yok. Panelin rolü bu tabloyu okuyabildiği
için, buraya trafiği anlatan bir sütun koymak yalıtımın etrafından ikinci
bir yol açardı, ve hiçbir `GRANT`'ın göstermeyeceği bir yol.

**Dört yazan, tek tablo** — bu şemada `GRANT`'in tüm kuralı ifade
edemediği tek yer. "Yalnız kendi satırın" `GRANT` ile söylenemez; satır
düzeyi güvenlik (RLS) söylüyor, `current_user` üzerinden. Projede ilk
kez, çünkü ilk kez gerekti.

Servis adı yapılandırmadan değil, **bağlantıdan** geliyor: reporter
veritabanına `current_user`'ı soruyor. RLS'in karşılaştırdığı değer o.

##### Sayfanın tek kuralı: her bölüm kendi başına düşer

Bütün bölümleri aynı anda kararan bir sağlık sayfası, tam ihtiyaç
duyulduğu anda hiçbir şey söylemez. Üç kaynak, üç bağımsız arıza:
servisler kalp atışı tablosundan, depolama ikinci bir sorgudan, okuma
API'si bir HTTP isteğinden.

Depolama bölümü **boyut** gösteriyor, satır sayısı değil — ve bunu
yazıyor. Panel bu tabloların satırlarını sayamaz; bu bir eksiklik değil,
sistemin dayandığı ayrımın kendisi. Sayfadaki cümlenin doğru kaldığını
test `has_table_privilege` ile doğruluyor.

**Kimler görür:** sahip ve geliştirici. Posta sayfasından bilerek farklı:
orada geliştirici reddediliyor çünkü giden postayı kontrol eden her
parola sıfırlama bağlantısını alır. Sağlık sayfasını okumak hiçbir şey
vermiyor — bir yapı numarası, bir bayt sayısı, bir çalışma süresi — ve
zaten geliştiricinin kendi teşhis aracı.

**Ölçülen kusur:** ilk hâl servis adını `Run` içinde bir kez çözüyor ve
başarısız olunca dönüyordu. Yani açılışta veritabanı hazır değilse servis
**ömrü boyunca** izlenmez kalıyordu. systemd bu süreçleri PostgreSQL ile
paralel başlatır; o pencere istisna değil, normal durum.

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

#### B6 — Çok müşterili VDS'te yalıtım ✅ **yapıldı** (2026-08-30)

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

> **Yapıldı — panel ve API kapıları.** İki dosya:
> `internal/panel/web/isolation_integration_test.go` (dört test, altı
> mutasyonla ölçüldü) ve `internal/api/isolation_test.go` (üç test, beş
> mutasyonla ölçüldü).
>
> **Asıl iddia şuna sıkıştırıldı:** *yabancı, var olan bir siteyi var
> olmayandan ayırt edemez.* Durum değil **gövde eşitliği** isteniyor —
> tek kelimeyle ayrılan iki 404 hâlâ bir orakldır. Panelde iki handler
> tam bunun için 404 döndürdüğünü yorumda yazıyordu; hiçbiri kontrol
> edilmemişti.
>
> **API tarafında bir eksik bulundu ve kapatıldı:** router 34 site
> kapsamlı rota kaydediyor, iki elle yazılmış test listesi bunların
> 26'sını kapsıyordu. Sekiz rota (`beacon/titles`, `beacon/refs`,
> `beacon/click-sources` ve beş `utm-*`) hiç sorulmamıştı. Davranışları
> doğruydu — hepsini tek bir `for` döngüsü sarıyor — ama on altıncı
> kırılım o döngünün dışında kaydedildiği gün hiçbir şey uyarmazdı.
> Yeni test liste tutmuyor: kayıtları kaynaktan okuyup döngüyü açıyor.
>
> **Sonradan bulundu *(2026-08-30)*:** aynı hata CSRF testinde de vardı.
> `TestEveryPostRouteRefusesARequestWithNoToken` elle yazılmış on rota
> tutuyordu, `acceptPost` çağıran on üç yer vardı — kurtarma formu,
> posta sayfası ve ayarlar sayfası hiç sorulmuyordu. Artık o test de
> rotaları kaynaktan türetiyor. Ayrıca `TestEveryRouteIsEitherGuarded‑
> OrDeliberatelyNot` eklendi: hiç `acceptPost`'a uğramayan bir handler
> bütün CSRF testlerini *hiçbirinde olmayarak* geçiyordu.
>
> **Kalanlar bilinçli olarak dışarıda:** `panel_logs` filtresi **D4b'ye**
> bağlı — tablo B1'de geldi, ama ertelemenin gerçek sebebi parantez
> içindeydi (*log sayfası henüz yok*), ve o sayfa D4b'dir. Filtreyi
> yapacak sorgu yazılmadan, olmayan bir fonksiyonun yalıtımı sınanamaz.
> Hata sayfası/yığın izi kontrolü C grubunda kapandı. `overview`'ın token kapsamıyla kesişimi zaten
> `TestServer_OverviewScopesToTheTokensSites` ile ölçülüyordu.

---

#### B7 — Sürüm kavramı ✅ *(2026-08-27)*

**Ne:** Projede hiçbir yerde sürüm numarası yok. `ApplyPendingMigrations`
hangi sürümden hangi sürüme gidildiğini bilmiyor; sağlık yoklaması
müşterinin hangi yapıda olduğunu söylemiyor.

**Neden gerekli:** On iki müşteriyi izlerken "hangisi hâlâ eski yapıda"
temel bir soru. Ayrıca bir hata raporunu doğru sürüme bağlamak
gerekiyor.

**Yapılacak:** derleme zamanı `-ldflags` ile gömülen sürüm + commit;
`crucible version`; sağlık sayfasında ve sağlık yoklaması cevabında
sürüm; `panel_operations`'ta operasyonun hangi sürümde çalıştığı.

> **Yapıldı, iki fazda.** Gömülü sürüm + commit ve `-version` G2'de
> (`internal/buildinfo`; damgasızsa Go'nun gömdüğü commit'e düşüyor).
> Sağlık sayfasında dört sürüm B4'te: üçü kalp atışı satırlarından,
> dördüncüsü panelin kendisinden — panel kendine canlı olduğunu söylemek
> için satır yazmıyor, ama sayfa üç yapı ve bir boşluk göstermemeli.
>
> **Kalan tek parça:** `panel_operations`'ta operasyonun hangi sürümde
> çalıştığı. O tablo B2 ve henüz yok; sıraya girmesi gereken yer orası,
> burası değil.

---

### C. Panelin HTTP yüzeyi

#### C1 — Türkçe katalog, şablonlar, gömülü HTMX, CSS ✅ **yapıldı**

**Ne:** Tüm metinler tek katalogda (`internal/panel/ui/messages.tr.toml`,
`go:embed` ile). `html/template`. HTMX ve CSS binary'ye gömülü — CDN
yok, npm yok, derleme adımı yok.

**Neden bu yığın:** "Kurulum ve çalıştırma yükünü de azaltacak şekilde
olmalı… 'şurada nginx'te bunu ayarla, burada şu var' istemiyorum."
Gömülü statik varlıklar tek dosya dağıtımı demek — ve ikinci bir etkisi
var: panel, dışarı hiç ağ erişimi olmayan bir makinede de çalışır.

**Yapılan:** iki yeni paket (`internal/panel/ui` render eder,
`internal/panel/web` yönlendirir) ve **beşinci binary `cmd/panel`**.

- **Katalog iki yönlü denetleniyor.** Şablonun andığı ama katalogda
  olmayan bir anahtar **panelin açılmasını engelliyor** (parse ağacı
  açılışta yürünüyor) — sayfada boş bir alan olarak görünmüyor. Ters
  yönde: hiçbir şablonun ve hiçbir Go dosyasının anmadığı anahtar testte
  hata veriyor. Bu yüzden bu fazın katalogu **küçük**: henüz olmayan
  sayfaların menü etiketleri içinde yok, sayfayla birlikte gelecekler.
- **Türkçe biçimlendirme** (`golang.org/x/text` artık doğrudan
  bağımlılık): `1.234.567`, `45,7`, yüzde işareti **önde** (`%45,7`),
  ve doğru büyük/küçük harf (`i→İ`, `I→ı`). Çoğul makinesi **yok** —
  Türkçede sayıdan sonra isim çekimlenmiyor.
- **Saat dilimi bir doğruluk sorusu.** Tanınmayan isim açılışta hata;
  sessizce UTC'ye düşmek, config dosyası başka şey derken her damgayı
  müşterinin saatinden saatlerce uzağa koymak demekti.
- **Önce tampona, sonra tele.** Doğrudan `ResponseWriter`'a yazmak,
  kırktaki nil alanı bulmadan önce `200` ve yarım belge göndermek
  olurdu. 500 sayfası açılışta bir kez üretilip saklanıyor — hata yolu,
  az önce bozulan şeye bağlı olamaz.
- **CSP'de ne `unsafe-inline` var ne `unsafe-eval`.** Şablonlarda tek
  bir satır içi `<script>`, `<style>`, `style=` veya `on…=` yok ve
  **yapısal test** kaynağa bakıp hata veriyor; ikinci bir test politika
  metninin kendisini koruyor.
- **Varlıklar içerik hash'li URL'den, bir yıl `immutable`; sayfalar
  `no-store`.** İkisi ayrı kural: sayfa müşterinin sayılarını ve CSRF
  jetonunu taşıyor, varlığın URL'i içeriğinin hash'ini taşıyor.

**Gerçek tarayıcının bulduğu ve hiçbir Go testinin bulamayacağı defekt:**
htmx açılışta `.htmx-indicator` için satır içi bir `<style>` enjekte
ediyor, CSP bunu **sessizce** reddediyor. Tek belirti, aylar sonra,
henüz yazılmamış bir sayfada hiç gizlenmeyen bir yükleniyor işareti
olurdu. Çözüm hash'i politikaya eklemek değil: enjeksiyon
`<meta name="htmx-config">` ile kapatıldı, dört kural `panel.css`'e
yazıldı — böylece bir htmx güncellemesi, politikanın kutsadığı şeyi
sessizce değiştiremiyor. Aynı koşu ikinci bir şey daha buldu: her sayfa
açılışında istenen `/favicon.ico`, catch-all route'a düşüp koca bir HTML
hata sayfasıyla cevaplanıyordu.

**Bir de günlük ağacının sakladığı şey:** veritabanına ulaşamayan panel,
terminalde **hiçbir şey yazmadan** `1` ile çıkıyordu — çünkü o noktada
logger dosyaya yazıyor. Açılış hataları artık ikisine birden gidiyor.

**Bugün ne servis ediliyor:** kabuk (başlık, işletmeci rozeti, izleyici
uyarısı, altbilgi), yazılmış 400/403/404/405/419/500/502/503 sayfaları,
varlıklar, ve giriş yer tutucusu. Sayfaların kendisi C2–C7 ile geliyor.

#### C1.5 — Çok dillilik ✅ **yapıldı** *(kullanıcı isteği)*

**Neden hemen:** "sonradan yeni dil eklenebilecek şekilde yap." Doğru an
buydu; sıradaki faz onlarca metin ekliyor ve her biri sonradan yeniden
elden geçirilecekti.

**Yeni dil eklemek = `internal/panel/ui/messages/` dizinine bir `.toml`
koyup yeniden derlemek.** Go tarafında güncellenecek liste yok; yükleyici
dizini geziyor. Şu an `tr` (temel) ve `en` var.

Her paketin üç bölümü var: `[dil]` (kod, dilin kendi adı, yazım yönü),
`[bicim]` (yerelin tarih ve birim verisi), `[metin]` (cümleler).

**İki kural bilerek asimetrik:**

- **Temel paket anahtar kümesinin sahibi.** Şablonun andığı ama onda
  olmayan anahtar, panelin açılmasını engelliyor.
- **Çeviri eksik olabilir.** Eksik anahtar temel dile düşüyor, açılışta
  tam listesiyle raporlanıyor, ve **testte hata veriyor.** Çünkü tersi
  daha kötü olurdu: bir Türkçe metin eklemek, o cümleyi hiç görmeyecek
  İngilizce kurulumları da düşürürdü. Çevirinin bedeli CI'da ödenir.

**Biçimlendirme de dile bağlı**, yalnız kelimeler değil: `1.234.567` /
`1,234,567`, `%45,7` / `45.7%`, `17 Ağustos 2026` / `August 17, 2026`.
Çoğul biçimleri gerçek CLDR kurallarından geliyor
(`golang.org/x/text/feature/plural`): paket yalnız dilinin sahip olduğu
biçimleri veriyor — Türkçe bir, İngilizce iki, Rusça dört — ve sayıdan
sonra çekimlenmeyen bir dil mekanizmanın varlığından hiç haberdar
olmuyor. **Test paketi bu repoda bulunmayan bir Rusça paket taşıyor**,
çünkü tr+en ikilisi mekanizmayı hiç sınamıyor: biri hiç çekimlemiyor,
diğerinin iki biçimi var.

**Dil seçimi sırası:** kurulumun `language` ayarı → tarayıcının
`Accept-Language` başlığı → temel dil. `?lang=` anahtarı **bilerek yok**:
aynı adresin farklı görünmesi, destek talebindeki her ekran görüntüsünü
belirsiz yapar. Hesap bazlı tercih (C4) en öne eklenecek; çözümleme
parametresi zaten bunun için variadic.

**Dürüstçe söylenmesi gereken:** `<html>` artık `lang` ve `dir`
taşıyor ve stil dosyası mantıksal özellikler kullanıyor, ama **hiçbir
sağdan-sola paket yazılmadı ve denenmedi.** Bu bir zemin, destek iddiası
değil.

**Kullanıcı kararı — ertelenen iki iş:**

1. **Arapça/İbranice (RTL):** *"ona çözüm daha sonra buluruz."* Bir RTL
   paket yazıldığında düzenin o paket önümüzdeyken gözden geçirilmesi
   gerekiyor; yapılacak iş şablonların retrofit'i değil, bir düzen
   incelemesi.
2. **Dil ayarının panele taşınması:** *"ilerleyen zamanlarda ayarlarada
   ekleyelim dil ayarlama."* Bugün dil yalnız config dosyasında
   (`language`) ve tarayıcıda. İki ayrı yere gidiyor:
   - **Kurulum varsayılanı → `panel_settings`** (A5 göçüyle birlikte).
     Burada çözülmesi gereken gerçek bir tasarım noktası var: ayar
     kaydı **kapalı küme** kullanıyor, ama mevcut diller **derlemeye**
     bağlı (gömülü paketler). Yani bu ayarın izin verilen değerleri
     çalışma zamanında belirleniyor — kayıttaki ilk dinamik enum bu
     olacak ve `Definition` bunu ifade edebilmeli.
   - **Hesap bazlı tercih → `panel_users`** (C4). Çözümlemenin en önüne
     giriyor; `Catalogs.Match` parametresi zaten bunun için variadic,
     başka hiçbir şey değişmiyor.

#### C2 — İlk çalıştırma tespiti ve geliştirici sihirbazı ✅ **yapıldı**

Hiç hesap yokken geliştirici erişimiyle ulaşılır. Teknik zemini kapsar
ve kurulumun devre hazır olduğunu onaylayarak biter.

**Kapı.** Hesabı olmayan bir kurulumda giriş formuna yönlendirmek bir
döngüdür; ön sayfa bunun yerine durumu söyler ve **girilecek komutu
yazar**:

```
panel -config panel.toml -dev-link
```

Bağlantıyı panel binary'sinin kendisi üretir (ayrı bir araç değil:
yalnız o config'in veritabanına ihtiyacı var, ve komutu çalıştırmak
sunucuda kabuk gerektirir — bağlantının temsil ettiği yetki tam olarak
budur). Çıktı **stdout**'a gider, günlük ağacına değil: bu, programın
insanın fareyle kopyaladığı tek çıktısı. Çıktı ayrıca **hangi onayın
verildiğini** her seferinde söyler — sahibi olmayan kurulumda anında
onay, sahibi olan kurulumda onay bekler. Mekanizmanın en önemli
özelliği bu ve sessizce değişebilecek bir şey.

**Altı adım:** başlangıç, veritabanı ve şema, siteler, yapılandırma
dosyaları, saklama süreleri, kontrol.

**İki kural sihirbazın şeklini belirliyor:**

1. **Yapılandırmaktan çok doğruluyor.** Veritabanı rolleri, şema, TLS,
   collector backend'i — panel bunları yapamaz ve yapabilmemeli. O
   adımlar gerçek durumu okuyup bildiriyor. Hiçbir şey yazmayan bir
   alan, "ne yapacağını söyleyen bir cümle"den **daha kötüdür**: kurucu
   doldurur, hata görmez, işin bittiğini sanır. Altı adımın ikisi
   yazıyor, dördü doğruluyor ve bunu söylüyor.
2. **Her adım değiştirdiğini anında kaydediyor.** Oturumda taslak yok,
   hepsini birden uygulayan bir "bitir" yok. Yarıda bırakan biri yarım
   kalmış bir kurulum bırakır — bu doğru ve görünür.

**Gerçek çalıştırmanın bulduğu defekt:** saklama adımı iki anahtarı da
global yazıyordu. Günlük saklama global, ama **analitik saklama siteye
bağlı** ve store yazıyı reddetti — hiçbir birim testinin beklemediği bir
mesajla. Düzeltme "siteyi parametre olarak geçirmek" değil, **sayfanın
doğruyu söylemesi** oldu: her yapılandırılmış site için ayrı alan, site
adıyla etiketli, ve hiç site yoksa bunun neden ayarlanamayacağını
söyleyen bir satır. Siteye bağlı bir ayarı tek global alan olarak
çizmek sadeleştirme değil, **başka bir ayar** çizmektir.

**Kapatılan denetim boşluğu:** `dev_access.*` eylemleri fazlardır
tanımlıydı ve hiçbir yer yazmıyordu. `panel_dev_access` zaten `used_at`
ve `used_from` tutuyor — kimsenin fark etmemesinin sebebi bu — ama o
tablo bir iş listesi (bir ay sonra temizleniyor, "hangi bağlantılar var"
sorusunu cevaplıyor). Denetim kaydı "bu kurulumda ne oldu" sorusunu
cevaplar. Artık her kullanım kayda geçiyor, ayrı bir geliştirici kimliği
altında, ve **bootstrap onayının kendi eylemi var**: "henüz sahibi yoktu
diye verildi" ile "sahibi evet dedi diye verildi" tek satıra
düzleştirilmiyor.

**Bilinen eksik:** sihirbazın **kabuğu** iki dilde, ama **kontrol
sonuçları ve elle yapılacaklar listesi yalnız Türkçe** — onlar kuralı
yazan dosyanın yanında duruyor (`internal/panel/preflight.go`), dil
paketlerinde değil. Taşımak `CheckResult.ID` üzerinden anahtara çevirmek
demek; `Detail` alanı dinamik olduğu için tamamı değil.

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

#### C3 — Sahip sihirbazı ve teknik kapı ✅ **yapıldı**

**Bu fazın kapattığı boşluk:** bugün ilk sahip hesabını oluşturmanın
**hiçbir yolu yok.** Teknik sihirbaz hesap açmıyor, `-dev-link` hesap
açmıyor, ve C4'ün giriş formu var olmayan bir hesaba giriş yaptıramıyor.
Zincirin eksik halkası devir teslim.

**C3.1 — Devir teslim ve sahiplenme bağlantısı.**

Teknik sihirbazın son adımı (`kontrol`) bir eylem kazanıyor: zorunlu
kontrollerin hepsi geçtiyse geliştirici sahibin e-postasını yazar ve
panel **tek kullanımlık bir sahiplenme bağlantısı** üretir. Bağlantı bir
kez ekranda gösterilir; saklanan yalnız SHA-256'sı.

- **Hesap bağlantı kullanılana kadar açılmıyor.** Devir teslim bir davet
  kaydı yazıyor, kullanıcı satırı değil. Parolasız veya "pasif" bir
  kullanıcı satırı, iki durumu senkron tutmak demek; davet kaydı tek
  durum.
- Sahiplenme tek işlemde: kullanıcıyı oluşturur, **yapılandırılmış her
  siteye sahip üyeliği** verir, daveti tüketir. Yarısı olan bir devir
  teslim, kimsenin sahibi olmadığı bir site bırakır.
- `panel -owner-link <eposta>` aynı şeyi kabuktan yapar — `-dev-link`'in
  eşi. Müşteri bağlantıyı kaybettiğinde tek çıkış yolu bu; e-posta yok
  (C7) ve olmadan da kurtarılabilir olmalı.
- Devir teslim `setup.completed` yazıyor. Bu eylem sabiti A grubunda
  tanımlanmıştı ve **hiç yazılmıyordu**; A10'daki `dev_access.*` ile
  aynı hata.

**C3.2 — Sahip sihirbazı.** `/hosgeldiniz/`, beş adım:

1. `hesap` — parolasını belirler. Sahiplenme burada gerçekleşir.
2. `site` — sitesini kendi diliyle adlandırır. Yeni ayar
   `panel.site_name`, **site kapsamlı**.
3. `saat` — saat dilimi. Yeni ayar `panel.timezone`, **global**, ve
   config dosyasındaki değer artık varsayılan. Müşteri kendi saat
   dilimini kurulumu yapandan iyi bilir.
4. `olcum` — gömülecek snippet. Panel beacon'ın genel adresini bilmiyor;
   `panel.toml`'a `beacon_url` ekleniyor. Boşsa adım snippet'i uydurmak
   yerine nereden alınacağını söyler.
5. `ekip` — meslektaş daveti. Üye sayfasına yönlendirir; hesabı olmayan
   birini davet etmek e-posta demek ve o C7.

**Kural: asla teknik bir adım zorunlu kılmaz.** Teknik bir değere
ihtiyaç duyduğunda boş alan değil, geliştiricinin yapılandırdığını
gösterir.

**C3.3 — Teknik kapı.** Sahip panelinde gösterişsiz bir bağlantı. İlk
tıklama teknik sihirbazı açmaz; onay ister:

> Bu bölüm geliştiriciniz tarafından tamamlandı. Yine de baştan yapmak
> isterseniz onaylayın.

Onaylamak tam teknik sihirbazı açar. Uyarı var çünkü yaygın durum birinin
merak edip gezinmesi ve çalışan bir kurulumu kazara yeniden
yapılandırmak en iyi ihtimalle bir destek çağrısı. Gizli sayfa değil de
onay olması bilinçli: **sunucu onların**, ve teknik bir sahip kendi
ayarlarına ulaşmak için bizden izin istemek zorunda kalmamalı.

**Bu, teknik sihirbazın erişimini genişletiyor** ve dikkat ister:
bugün yalnız geliştirici oturumu açıyor. Sonrasında **owner rolü veya
superadmin** de açabilecek — admin ve viewer **asla**. Hukuki ağırlıklı
ayarları koruyan geliştirici parolası bundan bağımsız ve yerinde kalır:
sihirbaza girmek o ayarları değiştirebilmek demek değil. Her onay
denetim kaydına yazılır.

**Tuzaklar:**

- **Sahiplenme bağlantısı tek kullanımlık olmalı ve yarış
  kaybetmemeli.** İki sekmede aynı anda açılırsa iki sahip değil bir
  sahip oluşmalı. Tüketme ve oluşturma tek işlemde, `used_at IS NULL`
  koşuluyla.
- **Süresi dolmuş veya kullanılmış bağlantı, var olmayan bağlantıdan
  ayırt edilmemeli.** Aksi halde bağlantı tahmini için bir oracle olur.
- **Devir teslim, zorunlu kontroller geçmeden yapılamamalı.** Bozuk bir
  kurulumu devretmek, müşterinin ilk deneyiminin bir hata sayfası olması
  demek.
- **Saat dilimi ayarı tanınmayan bir değeri kabul etmemeli** ve sessizce
  UTC'ye düşmemeli — panel config dosyasında zaten bu kuralı uyguluyor.

**Bitti ölçütü:** `go test -race ./...` temiz; entegrasyon paketi gerçek
veritabanına karşı temiz; **gerçek tarayıcıda** devir teslim → sahiplenme
→ sahip sihirbazının beş adımı → teknik kapının onayı baştan sona
yürütülüyor; tek kullanımlık bağlantı için **eşzamanlı** bir yarış testi
var; ve her metin katalogda (tr + en).

#### Ne oldu

Zincir tamamlandı. Yeni: `internal/panel/ownerclaim.go`,
`internal/panel/web/welcome.go`, `internal/panel/web/technicaldoor.go`,
`panel_owner_claims` tablosu, `panel.site_name` ve `panel.timezone`
ayarları, `panel -owner-link`, ve `[roles]` config bölümü.

**Yarış testi 8 eşzamanlı sahiplenme ile geçiyor: tam bir hesap, yedi
red.** Kazananın siteye sahip olduğu ve superadmin *olmadığı* ayrıca
doğrulanıyor — işlemin kaybedilmesi kolay yarısı o.

**Zamanlayıcı olmayan üç bulgu:**

1. **`cmd/panel` rol adlarını preflight'a hiç geçirmiyordu.** İzolasyon
   kontrolleri "bakılmadı" diyordu, ve zorunlu+atlanmış handover'ı
   bloke ediyor — yani **üretimde devir teslim hep kapalı olacaktı.**
   `[roles]` bölümü eklendi; ayarlanmamış olması artık yüksek sesle
   bloke ediyor, ki doğrusu bu: izolasyonu hiç doğrulanmamış bir
   kurulumu devretmek tam olarak kimsenin fark etmediği durumdur.
2. **`Complete()` kendi belgesiyle çelişiyordu.** `CheckWarn`'ın tanımı
   "bilinmeye değer, **devir teslimi engellemez**" — ama `Complete()`
   `!= CheckPass` diyordu, yani uyarı da engelliyordu. 0755 izinli bir
   log dizini bir kurulumu devredilemez yapıyordu. Kural artık tanımla
   aynı: **zorunlu + (fail veya skip)** engeller, uyarı engellemez.
   Atlama hâlâ engelliyor — "baktık, kusurlu" ile "bakamadık" farkı
   paketin tamamının üstüne kurulu olduğu ayrım.
3. **Sihirbaz reddedilen gönderime 200 dönüyordu.** Tarayıcıda düzgün
   görünür, başka her şeye yalan söyler. Artık panelin geri kalanıyla
   aynı: 400.

**Kararlar:**

- **Davet bir satır, kullanıcı değil.** Parolasız + "henüz alınmadı"
  bayraklı bir kullanıcı satırı, uyuşmadıklarında ya kimsenin giremediği
  ya da herkesin girebildiği bir hesap demek.
- **Sahiplenme superadmin üretmez.** Bir siteye sahip olmak ile kurulumu
  işletmek farklı işler; superadmin kabuktan, bilerek açılır.
- **Teknik kapının onayı oturumda, kullanıcı satırında değil.** Uyarı
  *bu ziyaret* hakkında: geçen mart saklama ayarına bakan biri bir
  dahakine cümleyi yeniden görmeli, çünkü uyardığı şey aşinalıkla
  azalmıyor.
- **Saat dilimi ayarı doğrulanıyor.** `Definition`'a `Check` alanı
  eklendi: `KindString` "bir metin" demek, ki bir site adı için doğru ve
  bir saat dilimi için işe yaramaz.

**Açık iş olarak kayıtlı:** davet e-postası yok (bağlantı elden
iletilir, `-owner-link` ile yenilenir); e-posta değiştirme C7.

---

#### C4 — Giriş, iki faktör, hesap ayarları, üye yönetimi ✅ **yapıldı**

Çekirdek yazıldı (B grubu commitleri): argon2id, scs oturumları, TOTP,
CSRF, deneme kısıtlama, roller ve yetenekler, son-sahip koruması, denetim
kaydı. **Bu fazda yazılan tek şey HTTP yüzeyi ve şablonlar.** Yeni bir
güvenlik ilkesi icat edilmiyor; var olanların hepsinin gerçekten
çağrıldığı doğrulanıyor.

**C4.1 — Giriş.** `GET/POST /giris`. E-posta + parola.

- Kısıtlama parola doğrulanmadan **önce** kontrol edilir, ve hesap
  olmadığında da `VerifyDummy` çağrılır: cevap ile geçen süre, hesabın
  var olup olmadığını ele vermemeli. Tek bir hata cümlesi — "e-posta
  veya parola hatalı" — çünkü hangisinin yanlış olduğunu söylemek
  kayıtlı e-posta listesi çıkarmanın en kolay yoludur.
- TOTP tanımlıysa `AwaitSecondFactor`, değilse `LogIn`.
- **Tuzak: açık yönlendirme.** Giriş sayfası nereden geldiğinizi
  hatırlamalı, ve `?next=` parametresi doğrulanmazsa `//kotu.site`
  panelin kendi giriş sayfasından yapılan bir kimlik avı sıçrama
  tahtasıdır. Kural: yalnız tek `/` ile başlayan, `//` veya `\\` ile
  başlamayan, ayrıştırılabilir ve şeması/host'u olmayan yollar.

**C4.2 — İkinci faktör.** `GET/POST /giris/dogrulama`. Yalnız bekleyen
bir kullanıcı varken açılır.

- **Kısıtlama burada da geçerli.** Altı haneli bir kod, parolası zaten
  bilinen bir hesap için bir milyonluk arama uzayıdır; ikinci faktörü
  kısıtlamayan bir sayfa ikinci faktör değildir.
- `ErrTOTPReplayed` ayrı bir cümle alır: aynı kodu iki kez girmek yanlış
  kod girmekten farklı bir şeydir ve kullanıcı bunu bilmek ister.
- **Kurtarma kodu bu fazda yok, ve bu bilinçli.** Telefonunu kaybeden
  birini kurtaran yol: bir sahip veya superadmin o hesabın TOTP'sini
  sıfırlar. Zaten o kişiyi tamamen silebilecek biri, ve ayrı bir
  kurtarma-kodu tablosu kendi saklama, hash'leme ve tek-kullanımlık
  sorunlarını getirir. Tek başına bir sahibin telefonunu kaybetmesi
  hâlâ kabuk erişimi gerektirir — **açık iş olarak kayıtlı.**

**C4.3 — Hesap ayarları.** `GET/POST /hesap`.

- Görünen ad, parola değiştirme, geliştirici modu anahtarı, iki faktör.
- **Parola değiştirmek mevcut parolayı sorar.** Çalınmış bir oturum
  çalınmış bir hesaba dönüşmemeli; bu tek alan aradaki farkı korur.
- E-posta bu fazda salt okunur: değiştirmek doğrulama e-postası
  göndermek demek, ve e-posta yolu C7.
- Geliştirici modu anahtarı yalnız `CapUseDeveloperMode` olana gösterilir
  **ve** sunucu tarafında da o yetenek aranır.
- **İki faktör kaydı ve QR kodu.** Sır önce oturumda tutulur, kullanıcı
  bir kod doğrulayana kadar kullanıcı satırına **yazılmaz**: yarıda
  bırakılan bir kayıt, kimsenin elinde olmayan bir uygulamaya bağlı
  kilitli hesap üretmemeli. QR, sırrı HTML'e gömmek yerine kendi
  ucundan (`/hesap/iki-faktor/qr`) sunulur — gömülü bir `data:` URI
  sayfa kaynağına, tarayıcı önbelleğine ve sayfanın her kopyasına sırrı
  yazar. Uç oturuma bağlı ve `no-store`.
- İki faktörü kaldırmak parola ister.

**C4.4 — Üyeler.** `GET/POST /site/{site}/uyeler`, `CapManageMembers`
gerektirir.

- Var olan bir kullanıcıyı e-postasıyla ekler. Yeni hesap **açmaz** —
  o davet e-postası demek, ve C7. Kullanıcı yoksa sayfa bunu söyler.
- **Tuzak: `CanAssign` sunucuda aranmalı.** `<select>`'e daha az seçenek
  koymak yetki denetimi değildir; formu elle gönderen bir admin kendini
  sahip yapabilir. Aynı kontrol POST işleyicisinde tekrar edilir.
- **Son sahip koruması bir cümle olarak görünür.** `ensureNotLastOwner`
  zaten reddediyor; sayfanın işi o reddi 500 yerine okunur bir uyarıya
  çevirmek.
- Viewer bu sayfayı hiç görmez — bağlantı gizlenmez, **işleyici
  reddeder.**

**C4.5 — Kabuk: gezinme, site listesi, viewer uyarısı, çıkış.**

- Giriş sonrası `/` erişilebilen siteleri listeler. Analitik kartları D
  grubuyla geliyor; bu sayfa şimdilik siteleri ve rolü gösterir, ve ne
  olmadığını söyler.
- Gezinme yalnız erişilebilecek yerleri gösterir, ama **gizlemek denetim
  değildir**: her işleyici kendi yeteneğini ayrıca arar.
- Viewer bölümünde neden teknik görünümlerin olmadığını söyleyen bir
  uyarı.

**Bu fazda yapılmayan, ve neden:**

- **Parola değişince diğer oturumlar düşmüyor.** scs oturumları
  veritabanında tutuyor ama kullanıcıya göre indekslemiyor, yani "bu
  kullanıcının diğer oturumlarını sil" bugün tablo taraması. Doğrusu
  oturum satırına bir `user_id` eklemek; ayrı ve küçük bir iş, **açık iş
  olarak kayıtlı.**
- **Sahip sihirbazı (C3) ve geliştirici onay ekranı (C5)** kendi
  fazlarında. C4 giriş kapısını açar, arkasındaki odaları değil.

**Bitti ölçütü:** `go test -race ./...` temiz; entegrasyon paketi gerçek
veritabanına karşı temiz; **gerçek tarayıcıda** parola girişi, TOTP
kaydı, TOTP ile giriş, rol değişikliği ve viewer reddi baştan sona
yürütülüyor; her yeni yetki denetimi için hem "izin verilen" hem
"reddedilen" testi var; ve her metin katalogda (tr + en).

#### Ne oldu

Hepsi yazıldı ve doğrulandı. Yeni dosyalar: `internal/panel/web/`
altında `auth.go`, `account.go`, `members.go`, `chrome.go`, `pages.go`;
`internal/panel/ui/templates/pages/` altında `giris`, `dogrulama`,
`siteler`, `hesap`, `uyeler`.

**Her yetki denetiminin iki testi var.** Viewer üye sayfasında 403
alıyor (404 değil: siteyi zaten görüyor), üye olmayan 404 alıyor (403
değil: 403 sitenin varlığını doğrular), admin kendini sahip yapamıyor —
ve bu üçü hem GET hem **elle gönderilen POST** için sınanıyor. Formu
gizlemek denetim değil; gizlenmiş formu elle göndermek testin kendisi.

**Tarayıcının bulduğu şey:** çıkış ucu ve işleyicisi vardı, yönlendirme
tablosuna bağlıydı, ve entegrasyon testi doğrudan POST ile geçiyordu —
**ama hiçbir sayfada çıkış düğmesi yoktu.** Giriş yapan birinin çıkış
yolu yoktu ve hiçbir test bunu göremiyordu, çünkü hepsi ucu URL ile
çağırıyordu. Artık kabukta bir POST formu var ve `ui` paketinde ucuz bir
yapısal test tutuyor: kabuk birini adlandırıyorsa kapıyı da gösterir.

**Tarayıcının bulduğu ikinci şey:** giriş alanındaki `autofocus`,
sayfanın en üstündeki "içeriğe atla" bağlantısını klavyeyle
ulaşılamaz hale getiriyordu. Odak alana taşınınca Tab o bağlantıyı
geçiyor. `autofocus` kaldırıldı — atlama bağlantısı bu panelin zaten
söz verdiği ve test ettiği bir erişilebilirlik özelliği.

**Bulunan bir hata:** `Server.Handler` nil bir `Sessions`'ı zaten
tolere ediyordu, ama yeni `requireUser` onu koşulsuz çağırıyordu — nil
pointer paniği. `Sessions`'ın kimlik doğrulamadan önce çağrılan her
metodu artık nil alıcıya güvenli cevap veriyor (**kapalı yönde**:
oturum yok, jeton yok, CSRF geçmez), ve `ListenAndServe` oturum
yöneticisi olmayan bir sunucuyu **reddediyor** — yoksa panel çalışır,
sağlıklı görünür ve sonsuza dek her girişi reddederdi.

**Açık iş olarak kayıtlı:** kurtarma kodları yok (telefonunu kaybedeni
sahip veya işletmeci kurtarır); parola değişimi diğer cihazlardaki
oturumları düşürmüyor (scs oturum satırında `user_id` yok); e-posta
değiştirme ve davet e-postası C7'de.

#### C5 — Geliştirici erişimi onay ekranı ✅ **yapıldı**

**Bu fazın kapattığı boşluk:** C2 bir kural yazdı ve o kurala uymanın
hiçbir yolunu bırakmadı. Bağlantı, kurulumun sahibi olduğu andan sonra
**sahip onaylayana kadar** çalışmıyor — doğru, test edilmiş, ve
ulaşılamaz: istek sayfası olmayan bir tabloda duruyordu ve tek yol bir
SQL istemcisiydi. Rızanın verilemediği bir rıza mekanizması rıza
mekanizması değil, üzerinde sessizce çalışılamayan bir kurulumdur.

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

**C5.1 — Onay sayfası (`/erisim`) ve kapısı.** Bekleyenler kart olarak
(gerekçe, ne zaman istendi, karar süresi, onaylanırsa oturum süresi),
sonra son otuz günün geçmişi. Onayla ve reddet **ayrı iki form**: iki
submit düğmeli tek form, "onayla"nın anlamını tarayıcının hangi düğmeyi
gönderdiğine bağlar.

**Kapı `ownsAnySite` değil.** Kullanılmış bir bağlantı `Superadmin`
taşıyan bir principal üretiyor — geliştirici işini yapmak için her siteye
ulaşmak zorunda — yani `ownsAnySite` onlar için **evet** diyor. Bu
sayfada bu, mekanizmanın tamamının sığdığı bir delik olurdu: onaylanmış
bir geliştirici bir sonraki isteği onaylar, sonra bir sonrakini, ve sahip
hayatında **bir kez** sorulmuş olur. Bu yüzden önce `Kind`, sonra
sahiplik; sahiplik sorusu `Kind` geçmeden hiç sorulmuyor.

*Testin söylediği ve yorumu düzelttiren şey:* kontrol kaldırılıp
koşulduğunda geliştirici **yine** reddediliyor — bir sonraki satır
geliştiricide olmayan bir kullanıcı kimliğiyle `User` yüklüyor ve
başarısız oluyor. Ama **500** ile, kimsenin tasarlamadığı bir kaza
sonucu. `Kind` kontrolü reddi yaratmıyor; reddi **kasıtlı** ve doğru
statülü yapıyor. Tek dayanağı alakasız bir sorgunun patlaması olan
kural, o sorgu düzeldiği gün biter.

**C5.2 — "Kim" diye bir şey yok, ve sayfa bunu söylüyor.** Planın bu
maddesi "kim" diyordu; panel bunu **bilemez**. İstek sunucuda kabuk
erişimi olan biri tarafından üretiliyor ve gerekçe o kişinin yazdığı bir
cümle. `requested_by` sütunu eklemek, bilinen hiçbir şeyi değiştirmeden
sayfayı soruyu cevaplıyormuş gibi gösterirdi. Bunun yerine ilk isteğin
**üstünde**, panelin kimi doğrulamadığını ve gerekçenin bir iddia
olduğunu söyleyen cümle var — gerekçeden önce, çünkü karar verecek kişi
metni okuyup bir izlenim edinmeden önce ne kadarının kontrol edildiğini
bilmeli.

**C5.3 — Afiş, her sayfada.** Kabukta duruyor, açılış sayfasında değil:
istek geldiğinde sahip büyük ihtimalle başka bir yerde çalışıyor. "Bu
okuyucu karar veriyor mu" sorusu **bir kez** çözülüp hem gezinmeye hem
afişe veriliyor; ilk hâli iki kez soruyordu (her sayfada iki aynı üyelik
sorgusu) ve üstelik farklı sıralarla. Sayaç yalnız yetkili bulunan kişi
için çalışıyor ve sayaç olarak kalıyor: afişin sayıya ihtiyacı var,
satırlara ihtiyacı olan sayfa bir tık ötede.

**C5.4 — Tanımlanıp hiç yazılmayan dört denetim eylemi.**
`dev_access.requested`, `.approved`, `.denied`, `.rejected` sabitleri
baştan beri vardı ve **hiçbiri yazılmıyordu**; kayıt "biri bir bağlantı
kullandı"dan başlıyordu. Hepsi artık store'da, kararı veren kuralın
yanında yazılıyor. Yarışı kaybeden karar hiçbir şey yazmıyor.

`.rejected` diğerlerinde olmayan bir sınır istedi: kullanım adresi
herkese açık, yani sunulan her dize için satır yazmak, panelin
**silemediği** bir tabloya yabancı birinin bağlantı hızında satır
yazması demekti. Entry yalnız jeton **gerçek bir satırla eşleşirse**
yazılıyor.

**C5.5 — Gerekçe uzunluğu.** Sütun `TEXT` ve hiçbir sınır yoktu, çünkü bu
faza kadar onu müşteriye **okuyan** bir şey yoktu. Artık sınırlı ve
**kırpılmıyor, reddediliyor**: kelimenin ortasında kesilen bir cümle
sahibin kararını değiştirebilir, ve yazan kişi kabukta, tekrar yazabilir.
Bayt değil **rune** sayılıyor. Stil tarafı diğer yarısı: dışarıdan gelen
tek dize o sayfada, uzunluğu sınırlı biçimi değil — 500 karakterlik tek
bir kelime sayfayı yana itmemeli. Tarayıcı testinin göremeyeceği bir
şey; ihlal yok, konsol hatası yok, sadece bozuk görünen bir sayfa.

**Doğrulama:** birim + entegrasyon (gerçek veritabanı) + gerçek
Chromium. Yarış testi: sekiz eşzamanlı karar → tam bir kabul, yedi
"zaten karar verilmiş", ve denetim kaydında **tek** karar. Sahte POST'lar
geçerli CSRF jetonu taşıyor, yani jeton eksikliğinden değil yetkiden
düşüyorlar.

#### C6 — Görünür kart **ve kırılım** seti, kurulum başına yapılandırılır ✅ **yapıldı**

**Karar (kullanıcı):** Müşterinin panelinde ne göründüğü sabit değil,
**ayardır** — geliştirici sihirbazında biz belirleriz, sonra geliştirici
ayarlarından değiştirilebilir.

**Kapsam D2'den sonra genişledi.** Bu madde yazıldığında sayfada altı
kart vardı; D2 altı **bölüm** daha ekledi ve onlar da sabit. Müşterinin
sayfası şu an on iki blok ve hiçbiri seçilebilir değil, yani maddenin
gerekçesi ikiye katlandı. C6 ikisini birden kapsıyor: **kart seti ve
kırılım seti, iki ayrı ayar, tek mekanizma.**

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

**Hız, ve bu ölçülebilir bir iddia.** "Kapalı olanın sorgusu hiç
atılmaz" kuralı D2'den sonra gerçek bir rakama oturuyor: gerçek bir
veritabanında ölçüldüğünde site sayfası sekiz çağrı yapıyor ve bunun
sayfalar kırılımı +3,3 ms, ülkeler kırılımı +2,0 ms tutuyor (kalan
dördü özet çağrılarının içinde bittiği için ölçülebilir maliyeti yok).
Yalnız iki kart ve bir bölüm isteyen bir kurulum, o sorguları hiç
atmamalı — ve bunun testi "daha hızlı" demek değil, **stub API'ye o
yolun hiç gelmediğini** doğrulamak.

**Bitti ölçütü:** kart seti ve kırılım seti `panel_settings`'te, site
başına; sihirbaz ikisini de yazıyor; geliştirici ayarlarından
değiştirilebiliyor; **kapalı olanın verisi API'den hiç istenmiyor** — ve
bunu isteği sayan bir test kanıtlıyor, süre ölçen bir test değil; boş
küme "hiçbir şey gösterme" demek değil, çünkü kartsız bir pano ürünün
kendisini gizlemek olurdu (D5'in "görünüm asla gizlenmez" kuralının
kart tarafındaki karşılığı).

#### C7.1 — Boş durumlar ve API kesintisi ✅ **yapıldı**

*(Bu madde "C7 — boş durumlar, API kesintisi, ve e-posta yolu" idi.
Tartışmadan sonra üçe bölündü: C7.1 burada, parola sıfırlama C7.2,
e-posta sihirbazı C7.3. A5.1/A5.2 ile aynı desen; eski ad commit
mesajlarında geçtiği için "C7" olarak anılmaya devam edebilir.)*

**Faz açılırken ölçüldü (2026-08-26) ve üç parçadan ikisi çoktan
yapılmıştı.** D1 boşluk sözlüğünü (`hasData`, `neverInstalled`,
`nothingInRange`, `unreachable`, `refused`), D2 de aynısını kırılımlar
için yazmış; kesinti cümlesi iki katalogda da duruyor ve panonun
kesintide ayakta kaldığını doğrulayan bir test var. Yani bu maddenin
büyük kısmı, planda "kalan iş" diye dururken, arayüz fazları sırasında
yan ürün olarak teslim edilmiş.

Bu, faz metnini yeniden yazmayı gerektirdi. Aşağıdaki üç bölüm ne
istendiğini anlatıyor; her birinin altında **bugün ne bulunduğu** var.

Üç küçük ama gerçek boşluk:

**Boş durumlar.** Kurulumdan sonraki ilk saat, müşterinin ürünün
çalışıp çalışmadığına karar verdiği andır ve planda hiç yoktu. Kritik
ayrım (§D5'in aynı kuralı): **"snippet henüz hiç görülmedi"** ile
**"snippet çalışıyor, henüz ziyaretçi yok"** aynı şey değil ve ikisi de
"0" olarak çizilmemeli. Birincisi bir kurulum hatası, ikincisi normal
bir pazartesi sabahı.

> **Bulundu: yapılmış (D1/D2).** Ayrım `emptinessFor` içinde ve
> önceliği doğru: önce hata, sonra "kaynak hiç yazdı mı", sonra dönem.
> `KnownSites` başarısız olursa "kurulmamış" **varsayılmıyor** — zaten
> kurulu bir snippet'i tekrar kurmasını söylemek, yalnız "bu dönem boş"
> demekten kötü.

**Okuma API'si düştüğünde.** Panelin tek sert bağımlılığı bu ve ne
göstereceği yazılmamıştı. Karar: panel çalışmaya devam eder (ayarlar,
üyeler, sağlık hepsi `panel_*` tablolarından okunur), yalnız analitik
kartları "veri kaynağına şu an ulaşılamıyor" der ve sağlık sayfasına
bağlantı verir. Sıfır göstermez.

> **Bulundu: kart tarafı yapılmış, kanıtın yarısı eksikti.** Panonun
> kesintide 200 döndüğü ve sıfır çizmediği test ediliyordu. Panelin
> **geri kalanının** kesintiden etkilenmediği ise yalnızca *inşa gereği*
> doğruydu — pakette yalnız iki dosya istemciye dokunuyor — ve inşa
> gereği doğru olan şey, biri site listesine bir ziyaretçi sayısı
> koyduğu gün sessizce yanlış olur.
>
> **Bu fazda eklendi:** yapısal test, analitik istemcisine dokunmasına
> izin verilen dosyaları kapalı bir listeye bağlıyor (yeni bir sayfa
> eklemek testi düşürüyor, ve düşme mesajı kesintide ne göstermesi
> gerektiğini söylüyor); davranışsal test ise API ölüyken site listesi,
> hesap, üye listesi ve çıkışın hâlâ çalıştığını gösteriyor — müşterinin
> bir kesinti sırasında **en çok** ihtiyaç duyduğu sayfalar bunlar,
> çünkü biri sorunu çözebilecek kişiye ulaşma yolu.
>
> **"Sağlık sayfasına bağlantı verir" yapılamadı: sağlık sayfası yok.**
> O **B4**'ün işi ve B grubu 1/7. Bağlantıyı şimdi eklemek, var olmayan
> bir sayfaya yönlendirmek olurdu. B4 yapıldığında bu satır oraya
> bağlanacak — kayıt burada duruyor ki unutulmasın.

**E-posta yolu — projede hiç yok.** *(Tartışma yapıldı, 2026-08-26.
Sonuç aşağıda; bu parça ikiye bölündü ve C7.2 / C7.3 oldu.)* (`grep`: sıfır SMTP/mail). Ama iki
planlı özellik e-posta gerektiriyor: `SendOwnerPasswordReset` (22.
operasyon) bir bağlantı yolluyor, ve sahip sihirbazı "meslektaş davetini
öneriyor".

##### Tartışmada ölçülen: sorun sanıldığı yerde değildi

Fazı açarken üç şey ölçüldü ve tartışmanın zeminini değiştirdiler:

1. **Davet zaten e-postasız çalışıyor.** Devir teslim bağlantısı
   ekranda bir kez gösteriliyor, `sha256` ile hash'li saklanıyor,
   süreli ve tek kullanımlık. Aynı desen geliştirici erişiminde de
   kullanılıyor. Yani ürünün **en hassas iki akışı** e-posta olmadan
   çözülmüş durumda ve mekanizma iki kez kanıtlanmış.
2. **Parola sıfırlama hiç yok.** Ne e-postalısı ne e-postasızı — akış
   mevcut değil. Yani mesele "sıfırlamaya e-posta eklemek" değil,
   "sıfırlamayı yazmak".
3. **Panel kurtarma kodu sözü veriyor ve tutmuyor.** Katalogda şu cümle
   duruyor: *"Kurtarma kodu henüz yok: telefonunuzu kaybederseniz
   sitenin sahibi veya işletmeci iki faktörünüzü sıfırlayabilir."*
   "Henüz" diyen bir cümle, tutulmayı bekleyen bir söz.

##### Karar (müşteri, 2026-08-26)

**Ölçek gerçek:** "çok sayıda olabilecek" — yani otuz kurulumda
"beni ara" bir destek kuyruğudur. Bu, kurtarma kodlarını nezaket
olmaktan çıkarıp gereklilik yapıyor.

**İki yol birden.** Kurtarma kodları birincil, operatör bağlantısı
kodlarını da kaybeden için ikinci ağ.

**E-posta olacak, ama API anahtarı istemeyen biçimde**, ve adım adım
doğrulanan bir sihirbazla: *"bunları yap, doğrula butonu, doğrulayınca
sıradaki adıma geçer, DNS'ler falan her şey"*. Bu tarif, panelin zaten
kullandığı preflight desenidir — **panel yapamaz ama kontrol eder ve
ne yapılacağını söyler**.

**SMTP'nin bu fazda mı olacağına müşteri karar veremedi** ("ne cevap
vereceğimi bilmiyorum"), ve karar bana bırakıldı: **ayrı faz.**
Gerekçe, tarifin kendisi — doğrulama butonlu, DNS kontrollü, ilerlemesi
kaybolmayan bir kurulum sihirbazı "SMTP ayarı eklemek" değil, kendi
başına bir faz. Yarım yapılmış hâli hiç yapılmamışından kötü olur.

---

#### C7.2 — Parola sıfırlama, e-postasız ✅ **yapıldı**

**Ne:** hesabını açamayan sahibin kendi başına içeri girebilmesi.

**Kurtarma kodları.** Hesap kurulurken (`RedeemOwnerClaim` anında)
üretilir, **bir kez** gösterilir, `sha256` ile hash'li saklanır — davet
jetonlarının bugün kullandığı desenin aynısı. Tek kod hem parola
sıfırlamaya hem kayıp ikinci faktöre yarar, çünkü müşteri tarafından
bakınca "telefonumu kaybettim" ile "parolamı unuttum" aynı problemdir:
*giremiyorum*.

**Operatör bağlantısı.** Kodlarını da kaybeden için, bugünkü desenin
aynısı: panele girmiş biri bağlantı üretir, ekranda görünür, nasıl
isterse öyle iletir.

**Neden e-posta yok:** bu fazın hiçbir yapılandırmaya ihtiyacı yok ve
ürünü **e-postasız eksiksiz** hâle getiriyor. C7.3 ondan sonra saf
katkı olur, zorunluluk değil.

**Bitti ölçütü:** kodları olan bir sahip, operatöre hiç dokunmadan,
gerçek tarayıcıda parolasını sıfırlayıp giriyor; kullanılan kod ikinci
kez çalışmıyor; kalan kod sayısı panelde görünüyor; ve katalogdaki
"henüz yok" cümlesi kalkıyor çünkü artık yalan.

##### Karşılandı (2026-08-26)

Dördü de ölçüldü. Sahip HTTP üzerinden kodla girip yeni parolasını
kuruyor; kod ikinci denemede reddediliyor; hesap sayfası kalan sayıyı
gösteriyor (2'de uyarı, 0'da alarm); ve "henüz yok" cümlesi yerini
`kurtarma_var`'a bıraktı.

**Operatör yolu ayrı bir jeton türü olmadı**, ve bu tasarımı
basitleştiren karar: operatör bağlantı üretmiyor, **kurtarma kodlarını
yeniliyor**. Tek kullanım yolu, tek denetim izi, tek redemption akışı.

**İkinci faktör istenmedikçe korunuyor.** "Parolamı unuttum" ile
"telefonumu kaybettim" aynı forma geliyor ama aynı istek değil; her
sıfırlamada 2FA'yı silmek, onu kullanan her hesabı sessizce zayıflatmak
olurdu. Kutucuk işaretlenmezse hesabın 2FA'sı duruyor ve kod
sahibi **yine de** 2FA'yı geçmek zorunda — yoksa kurtarma kodu, onu
bulan herkes için ikinci faktörü isteğe bağlı yapardı.

---

#### C7.3 — E-posta kurulum sihirbazı ✅ *(2026-08-27)*

> İlk taslak konuşmadan çıkmıştı ve kabaydı. Kullanıcı isteği üzerine
> açıldı; **dört ölçüm** taslakta üç gerçek sorun gösterdi ve bir adımın
> **yazıldığı hâliyle imkânsız** olduğunu ortaya çıkardı. Aşağısı kararın
> kendisi.

##### Önce çerçeve: e-posta bu üründe **taşıyıcı değil**

Ölçüm: `internal/` ve `cmd/` altında **sıfır e-posta kodu**; adres
yalnızca giriş kimliği. Parola sıfırlama C7.2'de kurtarma kodlarıyla,
davet C7.1'de bağlantıyla çözüldü — **ikisi de çalışıyor.**

Yani e-posta, çalışan iki akışa eklenen bir **kolaylık**. Bundan çıkan
yönetici kural:

> **E-posta hiçbir zaman tek yol değildir.** E-postayı kullanabilen her
> akış e-postasız da çalışır, ve panel e-postayı tek yol gibi sunmaz.
> Bunu bir arayüz notu değil, **test edilen bir değişmez** yapıyoruz.

> **Yapıldı.** `TestTheInvitationLinkSurvivesEveryMailOutcome` üç durumu
> da geçiyor — posta hiç tanımlı değil, çalışıyor, ve tanımlı ama sunucu
> yok — ve üçünde de aynı iddia: bağlantı sayfada. Çalışan durumda
> ayrıca sahte SMTP sunucusuna iletinin gerçekten ulaştığı sayılıyor,
> çünkü "gönderildi" yazıp hiçbir şey ulaşmaması bu özelliğin karşı
> durduğu hatanın ta kendisi.

Sebep teslimat: taze bir VDS'ten SPF/DMARC'sız çıkan posta sessizce
spam'e düşer. Sessizce kaybolan bir parola sıfırlama en kötü hata
biçimidir — kişi bekler, panel "gönderildi" der.

##### Değişiklik 1: SMTP parolası `panel_settings`'e **yazılamaz**

İlk taslak "her adımın durumu `panel_settings`'e yazılır" diyordu.
Ölçüm, `release/sql/grants.sql`'den:

```sql
GRANT SELECT ON panel_settings TO collector, beacon_writer;
```

Yani parola oraya yazılsa, **internete en yakın duran iki süreç** onu
okuyabilirdi. Collector müşterinin sitesinin önünde duran vekil.

**Karar:** sırlar için ayrı tablo — `panel_smtp` — ve yetkisi **yalnız
`panel_user`'a**. Collector ve beacon'a asla. `verify.sql`'e negatif
iddia ekleniyor: *"collector `panel_smtp`'yi okuyamaz"*. Böylece yalıtım
varsayım değil, F2'nin doğruladığı matrisin bir satırı olur.

Sihirbazın **durumu** (hangi adım doğrulandı) `panel_settings`'te
kalabilir — o sır değil.

##### Değişiklik 2: 465 de destekleniyor, yalnız 587 değil

Ölçüm: `smtp.SendMail` içeride `Dial` çağırıyor — düz TCP, yani yalnız
STARTTLS yolu. **Örtük TLS (465) `tls.Dial` + `smtp.NewClient` ister.**
İlk taslak "port 587, 25 değil" diyordu ve 465'i hiç anmıyordu; oysa
Türkiye'deki barındırma sağlayıcılarının çoğu 465 veriyor.

**Karar:** ikisi de. Hangisinin çalıştığı ekranda yazılır.

##### Değişiklik 3: Tanı, gönderimin kendisinden değerli

Ölçüm: `net/smtp` yalnız **PLAIN ve CRAM-MD5** biliyor; XOAUTH2 yok.
Sonuç: SMTP AUTH'u kapatılmış bir Microsoft 365 kiracısı **hiç
çalışmaz**, Google Workspace ancak uygulama parolasıyla çalışır.

Bunu desteklemeye çalışmıyoruz — **teşhis ediyoruz.** Sunucu
`AUTH XOAUTH2` duyurup `PLAIN` duyurmuyorsa panel bunu aynen söyler ve
ne yapılacağını yazar. "Kimlik doğrulama başarısız" demez.

Aynı şekilde `smtp.PlainAuth` şifresiz bağlantıda parolayı **hiç
göndermiyor** (`unencrypted connection`). Bu iyi bir davranış ve
gösterilecek: *"sunucu STARTTLS önermedi, parolanız hiç gönderilmedi."*

**Bu fazın asıl değeri burada:** ürün "e-posta gönderebiliyoruz" değil,
**"e-posta çalışmıyorsa kuran kişi tek ekranda tam olarak neden
çalışmadığını görüyor"**.

##### Değişiklik 4: DKIM adımı kaldırıldı — yazıldığı hâliyle imkânsızdı

İlk taslak: *"DKIM — test postasının başlığında imza var mı."*

Panel postayı **gönderiyor**, hiç almıyor. Gönderdiği bir mesajın DKIM
başlığını okuyamaz; posta kutusu yok. Adım, yazıldığı hâliyle
yapılamaz.

Alan adının bilinen bir seçicide DKIM kaydı yayımlayıp yayımlamadığına
bakmak da genel olarak mümkün değil: seçici sağlayıcıya özel ve
tahmin edilemez.

**Karar:** DKIM adımı yok. Yerine, Gmail/Yahoo kabulünü fiilen belirleyen
**DMARC** kaydı okunur, SPF bildirilir, ve DKIM'in sağlayıcının işi
olduğu ve buradan doğrulanamayacağı **açıkça yazılır**. Yapılamayan bir
kontrolü listede tutmak, yapıldığını sanmaktan kötü.

##### Değişiklik 5: DNS adımları **engellemez**, bildirir

SPF/DMARC kayıtları yayılması zaman alır. Bir dakika önce eklenmiş kaydı
"başarısız" diye gösteren bir sihirbaz, insanlara doğrulamayı yok saymayı
öğretir.

**Karar:** yalnız **SMTP bağlantı testi engelleyici** — o deterministik.
DNS kontrolleri *ne bulunduğunu* söyler ve ilerlemeyi durdurmaz:
*"bu alan adı için henüz SPF kaydı yok; DNS değişiklikleri bir saati
bulabilir."*

##### Değişiklik 6: Test postası **yalnız oturum açmış kişiye**

İlk taslak "kurulumu yapanın yazdığı adrese" diyordu. Yazılan adrese
posta gönderen bir düğme, panel erişimi olan birinin **istediği adrese
posta göndermesi** demektir — açık röle şekli.

**Karar:** test postası yalnız **oturumu açık hesabın kendi adresine**
gider, yazılan bir adrese değil. Teslimatı aynı şekilde kanıtlar, ve
kötüye kullanım yüzeyi tamamen kalkar. Ayrıca hız sınırlı.

##### Değişiklik 7: Kurulumdan sonra bozulduğunda

İlk taslak yalnız kurulum anını kapsıyordu. Parolanın süresi dolduğu gün
ne olacağı yazılı değildi.

**Karar:** her gönderim sonucu kaydedilir ve panelde **görünür** —
yalnız loga değil. E-posta taşıyıcı olmadığı için başarısızlık akışı
durdurmaz, bağlantı yoluna düşer; ama sessizce düşmez.

##### Adımlar, son hâli

| Adım | Doğrulama | Engeller mi |
|---|---|---|
| Gönderen adresi | Biçim + alan adı çözülüyor mu | Evet |
| SMTP bağlantısı | Gerçekten bağlan (587 STARTTLS / 465 örtük), sunucunun duyurduğu AUTH listesi gösterilir | Evet |
| Kimlik doğrulama | Gerçek AUTH; reddedilirse sunucunun cevabı **ve** teşhis | Evet |
| Test postası | Oturum açmış kişinin kendi adresine tek posta | Evet |
| SPF | `net.LookupTXT` — bulunanı bildirir | **Hayır** |
| DMARC | `net.LookupTXT` — kayıt ve politika | **Hayır** |
| DKIM | *Yok.* Sağlayıcının işi, buradan doğrulanamaz — yazılı olarak söylenir | — |

##### Değişmeyenler

**API anahtarı gerekmiyor, üçüncü taraf e-posta API'si yok.** Kendi
sunucusunda barınan, gizlilik odaklı bir ürünün müşteri adreslerini
başkasının servisinden geçirmesi, üstüne kuruluma bir hesap ve fatura
eklemesi olurdu.

**Yeni bağımlılık yok** *(ölçüldü)*: `net.LookupTXT` ve `net/smtp`
stdlib'de. `net/smtp` donmuş bir paket ama ihtiyacımız olan yüzey
donmuş hâliyle yeterli — eksik olan XOAUTH2 ve onu zaten desteklemiyoruz.

**İlerleme kaybolmaz.** Doğrulanmış adım durumu `panel_settings`'e
yazılır; sekmeyi kapatan kaldığı yerden devam eder.

##### Bitti ölçütü

Gerçek bir SMTP sunucusuna karşı: yanlış parolayla **reddedildiği** ve
doğrusuyla **teslim edildiği** görülür; 587 ve 465 ayrı ayrı denenir;
STARTTLS önermeyen bir sunucuda parolanın **hiç gönderilmediği**
gösterilir. SPF/DMARC gerçek DNS'e karşı hem kaydı olan hem olmayan bir
alan adıyla denenir ve **ikisi de ilerlemeyi durdurmaz**. Doğrulanmış
adım süreç yeniden başlatıldıktan sonra doğrulanmış kalır. Ve iki
yapısal test: **`panel_smtp`'yi collector okuyamaz**, ve **e-posta
yapılandırılmamışken davet ile sıfırlama akışları çalışmaya devam
eder**.

### D. Panelin kendisi

#### D1 — Site seçici, sonra site başına altı kartlık varsayılan görünüm ✅ **yapıldı**

**Neden şimdi, plandaki sıradan önce.** *(kullanıcı kararı: "olması
gereken sırayla gidelim")* Ölçüm şunu söyledi: altyapı ürün seviyesinde,
arayüz yarım. Panel bugün analitik API'sine **tek bir çağrı bile
yapmıyor** — 21 şablonun hiçbiri sayı göstermiyor. Müşteri kuruyor,
giriş yapıyor, site listesini görüyor ve orada duruyor. A5.2 çalışan bir
mekanizmaya anahtar ekliyor; D1 ise panelin **var olma sebebi**. Sıra
buna göre değişti.

**Mimari kural, hatırlatma değil kısıt:** panelin veritabanı rolünün
analitik tablolarına **hiç** erişimi yok. Sayılar HTTP üzerinden okuma
API'sinden geliyor — harici bir panelin alacağı yoldan. Bu faz o kuralın
ilk gerçek sınavı, ve yapısal bir test onu koruyacak: `internal/panel`
ağacı `internal/api`'nin store'unu import edemez.

**D1'in kapsamı:**

1. **`internal/panel/analytics`** — okuma API'si için tipli istemci.
   Zaman aşımı çağrı başına, gösterge paneli iki özeti (collector +
   beacon) **eşzamanlı** çekiyor tek bir son teslim süresiyle.
2. **Kart kaydı, kapalı küme.** C6 "görünen kart seti kurulum başına
   yapılandırılır" diyor. D1 altı kartı şablona gömerse C6 onu sökmek
   zorunda kalır. Onun yerine kart kümesi ayar kaydının şeklinde bir
   **kapalı küme** olarak yazılıyor ve D1 varsayılan seçimi çiziyor;
   C6'nın işi seçimi ayara bağlamak olur.
3. **Site panosu**: aralık seçici + kartlar.
4. **Üç ayrı boşluk, üç ayrı cümle** (aşağıda).

**Aralık sınırları panelin saat diliminde tam gündür.** §6 zaten
söylüyor: oturumlar aralık sınırında kesiliyor, "panel tam günleri
tercih etmeli". "Şu andan 7×24 saat geri" göndermek, müşterinin
"geçen hafta" dediği şeyle uyuşmayan ve komşu aralıklarla toplanamayan
sayılar üretir. Sınırlar `panel.timezone`'da hesaplanıyor — o ayarın
neden var olduğunun ta kendisi.

**Üç boşluk, ve neden üç ayrı cümle.** §D5'in "artık toplamıyoruz" ile
"hiç toplamadık" ayrımı kart düzeyinde:

| Durum | Ne demek | Ne yazılmalı |
|---|---|---|
| Kaynak hiç kurulmamış | Sitede snippet yok, beacon o site için hiç satır görmemiş | "Bu sayılar için snippet gerekiyor" — eksiklik değil, yapılmamış bir kurulum adımı |
| Kurulu, bu aralıkta veri yok | Gerçek bir ölçüm: sıfır | "Bu dönemde hareket yok" |
| API'ye ulaşılamıyor | Panel çalışıyor, veri kaynağı yok | 502 sayfası — metni **zaten yazılmış**: "Sayılar eksik değil, henüz gelmedi" |

Üçünü tek "veri yok" cümlesine indirmek, kurulum hatasını ölçüm sonucu
gibi gösterir — bu projenin her yerde reddettiği şey.

**Jetonun patlama yarıçapı, açıkça.** Panelin API jetonu her siteyi
okuyabiliyor; müşteriler arasındaki tek şey panelin **kendi** yetki
katmanı (`siteAccess`). Bu kabul edilebilir ve veritabanı havuzuyla aynı
şekil — geniş kimlik bilgisi, istek başına dar kontrol — ama sonucu
yazılı olmalı: `siteAccess`'teki bir hata başka bir müşterinin
sayılarını sızdırır. O yüzden bu fazın testi çiftli: erişimi olan
kullanıcı **ve** olmayanın aynı isteği.

**Bu fazda yapılmayanlar:** detaya inişler (D2), geliştirici modu
sütunları (D3), kesişim görünümleri ve "maskeli" uyarısı (D5), kart
setinin ayara bağlanması (C6). D1 bir sayfa açıyor ve altı sayı
gösteriyor; derinlik sonraki fazların işi.

**Bitti ölçütü:** panel gerçek bir API sunucusuna karşı gerçek sayı
çiziyor; API kapalıyken sayfa 502 metnini gösteriyor ve panel ayakta
kalıyor; üç boşluk üç ayrı cümle üretiyor; aralık sınırları panelin
saat diliminde tam gün; yapısal test panelin analitiği veritabanından
okumadığını tutuyor; erişimsiz kullanıcı çiftli testte reddediliyor.

##### Ne oldu

**Mimari kural ilk gerçek sınavını geçti.** Panel sayıları HTTP'den
okuyor; yapısal test artık `internal/panel/web` içinde
`traffic_snapshots`, `beacon_events` ya da `internal/api` geçmesini
reddediyor. Doğrudan havuza uzanan bir handler derlenir, geliştirmede
superuser'la çalışır ve **üretimde** patlardı — bir şeyi keşfetmenin en
kötü sırası.

**Jetonun patlama yarıçapı test edildi, varsayılmadı.** Panelin jetonu
her siteyi okuyor; müşteriler arasındaki tek sınır panelin kendi
kontrolü. O yüzden çiftli test: sahibin isteği, ve üyeliği olmayan bir
hesabın aynı isteği — **404, 403 değil**, çünkü 403 sitenin var olduğunu
doğrular ve URL'yi müşteri listesi çıkarma aracına çevirir.

**Dördüncü boşluk durumu eklendi:** API cevap verdi ve jetonu
reddetti. "Bekleyin" ile "birinin yapılandırmayı düzeltmesi gerekiyor"
okuyucuyu farklı yerlere gönderiyor. Ve hepsinin üstünde tek kural:
**başarısızlık asla sıfır olarak okunmaz.** Çağrı zaman aşımına
uğradığı için "0 ziyaretçi" yazan bir kart eksik sayı değil, **yanlış**
sayıdır ve müşterinin bunu anlamasının yolu yoktur.

**Yanlış olan üç test.** Biri kendi girdisiyle panikledi
(`httptest.NewRequest` hedefi istek satırı gibi ayrıştırıyor, kodlanmamış
boşluk testi öldürüyor); biri eşzamanlılık testinin iddia ettiği yarışı
üretti (`-race` yakaladı); biri **doğru sayfaya karşı** patladı —
`snippet'in` içindeki kesme işaretini `html/template` kaçırıyor, test ham
metinle karşılaştırıyordu.

**Tarayıcının eklediği:** altı kart, masaüstünde dört sütun, telefonda
bir sütun, taşma yok, yatay kaydırma yok, CSP ihlali yok; aralık seçici
üç bağlantı + bir "şu an buradasınız", ve tıklamak alttaki tarihleri
gerçekten değiştiriyor. Hiçbiri ResponseRecorder'dan görülemez.

#### D2 — Detaya inişler: sayfalar, kaynaklar, kampanyalar, cihazlar, ülkeler, olaylar ✅ **yapıldı**

D1 altı sayı gösteriyor. Bir sayının **neden** o olduğunu gösteren
hiçbir şey yok: hangi sayfa, nereden gelindi, hangi kampanya. API'de
otuza yakın kırılım ucu yazılı ve test edilmiş halde duruyor; panelde
karşılığı sıfır.

**Kapsam: altı kırılım.** Başlıktaki altısı, ve yalnız o altısı:

| Kırılım | Uç | Satır tipi | Kaynak |
|---|---|---|---|
| Sayfalar | `/beacon/pages` | `BeaconGroupStat` | beacon |
| Kaynaklar | `/beacon/referrers` | `BeaconGroupStat` | beacon |
| Kampanyalar | `/beacon/campaigns` | `CampaignStat` | beacon |
| Cihazlar | `/beacon/devices` | `BeaconGroupStat` | beacon |
| Ülkeler | `/beacon/countries` | `BeaconGroupStat` | beacon |
| Olaylar | `/beacon/events` | `EventStat` | beacon |

**Kapsam dışı, bilerek:** parmak izi, ASN, skor dağılımı, kesişim, ham
dışa aktarma. Hepsi D3 — ve D3'ün kendi kuralı "yeni sayfa değil, aynı
sayfaya sütun". D2 o sayfaları yazan faz; D3 sütunu ekleyen faz. D6 de
aynı yerden bakıyor: "varsayılan görünüm... parmak izi yok, ASN yok,
skor yok".

##### Altı karar

**1. Kırılım kaydı kapalı küme.** D1 kart kaydının aynısı, aynı iki
sebeple: C6 bunu ayara bağlayacak, ve daha önemlisi **kırılım adı bir
URL yoluna giriyor.** İstekten gelen bir dize `/beacon/<x>` haline
gelemez. Kayıtta olmayan bir ad 404, uç denemesi değil.

**2. "Ülkeler" hangi ülkeler — ve bu soru tuzak.** API'de iki tane var:
`/countries` collector'ın gördüğü **adresleri**, `/beacon/countries`
sayfa açan **kişileri** sayıyor. `server_beacon.go` bunu route
yorumunda açıkça uyarıyor: ikisini tek ad altında sunmak, paneli
onları birbiriyle karşılaştırmaya davet eder. D2 beacon olanı alıyor
(kart seti de öyle: ziyaretçi/görüntüleme/oturum beacon), ve bölüm
başlığı hangi kaynaktan geldiğini **söylüyor**. Collector'ın ülke
kırılımı D3'e ait — orada zaten ASN ve skorun yanında duracak.

**3. Boş grup satır, boşluk değil.** `BeaconGroupStat.Empty`, değerin
hiç belirlenemediği grubu işaretliyor: referrer'sız doğrudan giriş,
tanınmayan tarayıcı, çözülemeyen ülke. API bunu **düşürmüyor, işaretli
döndürüyor**, çünkü sayıların toplamı siteninkini tutsun diye. Panel de
düşürmeyecek: adı olan bir satır olarak çizecek ("Doğrudan",
"Bilinmiyor"). Boş etiketli bir satır çizmek ya da satırı atmak,
"baktık ve bulamadık" ile "bakmadık" ayrımını — D5'in üzerine kurulduğu
ayrımı — bir kırılım tablosunda yeniden kaybetmek olurdu.

**4. Yüzde paydası aynı filtreden gelmeli.** Kırılım `total`'ı grup
sayısı, toplam görüntüleme değil — yani pay/payda için özet çağrısı
gerekiyor. Buradaki gerçek risk: beacon özeti de kırılımlar da `bots`
filtresini uyguluyor ve ikisinin de varsayılanı `exclude`. Biri
değişirse yüzdeler sessizce %100'ü tutmaz. Panel ikisine de aynı
filtreyi gönderiyor **ve bunu bir test kilitliyor** — varsayım olarak
bırakılırsa bir gün bir tarafta değişir, sayfa yine çizilir, ve yanlış
olan tek şey okunan sayı olur. Payda sıfırsa yüzde **hiç** yazılmaz;
"%0" da "%NaN" da uydurma olurdu.

**5. İki yer, tek kayıt.** Site sayfasında her kırılım bir bölüm, ilk 8
satır. Toplam gösterilenden fazlaysa kendi sayfasına bağlantı:
`/site/{site}/detay/{kirilim}`, orada sayfalama var. İkisi de aynı
kayıttan ve aynı tablo çizicisinden besleniyor; ikinci bir render yolu
yok, yoksa biri düzeltilip diğeri unutulur.

**6. Boşluk üç türlü, yine.** D1'in dört durumu (kurulmamış / bu dönem
boş / ulaşılamıyor / reddedildi) kırılımlarda da aynen geçerli ve aynı
sebeple ayrı: bir kırılımı "veri yok" diye çizmek, kurulmamış snippet'i
ölçüm gibi gösterir.

##### Ölçülecek

- Sayfa başına çağrı sayısı ve süresi. D1 iki çağrı yapıyordu; bu faz
  onu sekize çıkarıyor (2 özet + 6 kırılım). Eşzamanlı, tek `PageTimeout`
  altında — ama **ölçülmeden "hızlı" denmeyecek.**
- Gerçek bir tarayıcıda: telefon genişliğinde tablo taşmıyor mu,
  sayfalama bağlantıları gerçekten dönemi koruyor mu.

#### D3 — Aynı sayfalar üzerinde geliştirici modu katmanları ✅ **yapıldı** *(ham dışa aktarma hariç, karar bekliyor)*

Parmak izleri, ASN'ler, skorlar, kesişim görünümleri, ham dışa aktarma.
**Yeni sayfa değil, aynı sayfaya sütun.** Sayfalar sayfası sütun kazanır;
farklı bir sayfalar sayfasına dönüşmez.

##### Yapılan: collector'ın üç kırılımı

`parmak-izi`, `asn`, `sunucu-ulke` — D2'nin yazdığı kayda, aynı tek
çiziciye, aynı detay sayfası mekanizmasına eklendi. Yeni sayfa yok.

**Neden `sunucu-ulke` ayrı bir kırılım, `ulke`'nin bir kipi değil.**
İkisi farklı soruya cevap veriyor: biri sayfa açan **kişileri**, diğeri
sunucuya ulaşan **her adresi** sayıyor — sayfayı hiç açmayanlar dâhil.
API'nin kendi route yorumu bunu uyarıyordu. İki ayrı kırılım demek iki
ayrı başlık demek, ve birini çizip diğerinin adıyla etiketlemenin yolu
kalmıyor. Bölüm sırasında da en sona konuldu: beacon'ın ülkelerinin hemen
altına koymak, tam da ayrı tutulma sebebi olan karşılaştırmaya davet
olurdu.

**Üçüncü metrik: `adres`.** Beacon satırları görüntüleme sayıyor,
collector satırları adres. Payda da farklı — collector kırılımları
trafik özetinin adres sayısına bölünüyor. Aynı payda ile bölmek "dört
yüz görüntülemenin on iki adresi" gibi, mükemmel çizilen ve hiçbir şey
ifade etmeyen bir yüzde üretirdi.

**İkinci sütun ayrı bir alan.** `Visitors` "bu sayının arkasında kaç
kişi var" demek; collector'da o soru cevapsız (bir adresin arkasında kaç
kişi olduğu bilinemez). O sütun bot adres sayısını taşıyor ve **ayrı bir
alan** — aynı alanı kullanmak, başlığı doğru ama altındaki sayısı başka
bir soruya cevap veren bir tablo üretirdi.

##### Kapı: rol ile tercih aynı şey değil

| Yer | Neye bakıyor | Neden |
|---|---|---|
| Site sayfası | **Rol + tercih** (`ShowsTechnical`) | Bu sayfa kimse istemeden açılıyor; D6 varsayılan görünümde parmak izi olmamasını söylüyor |
| Detay sayfası | **Yalnız rol** (`CapUseDeveloperMode`) | Bir şeyin adresine gitmek, o şeyi görmek istemenin ta kendisi. Tercihle reddetmek, tercihi bir yetkiye çevirirdi |

Reddetme **404**, 403 değil — `siteAccess` ile aynı sebep: ne
reddettiğini söyleyen bir ret, okuyucuya kendisi gibi olmayan insanlar
için bir sayfa olduğunu söyler.

Eşli test (D1'in jeton yarıçapı testinin aynısı): aynı sitede bir sahip
**ve** bir izleyici, ikisinde de geliştirici tercihi **açık**. Sahip 200
alıp parmak izini görüyor, izleyici 404 alıyor ve cevapta parmak izi
yok. Tercihin izleyicide de açık olması bilerek: kendi kutusunu
işaretleyerek açılabilen bir kapı, kapı değildir.

##### Ölçerken çıkan iki hata

**`request()` ve `detailData` aynı hatayı iki yerde yapıyordu.** İkisi de
beacon özetini sabitliyordu. D2'de bu doğruydu ve kodun yanına "bu doğru
olmaktan çıkarsa şöyle olmalı" diye yazılmıştı; D3 onu çıkardı. Sessiz
bir hata: satırlar ve sayılar çiziliyor, yalnız pay sütunu tireye
düşüyor — çünkü istenmeyen özet, hata değil **meşru sıfır** dönüyor.
Tek `summaryFlags` fonksiyonuna alındı, iki çağıran da onu kullanıyor.

**Çözülemeyen ASN `"0"` geliyor, `""` değil.** API `asn::text` seçiyor ve
sütun INTEGER, varsayılanı 0; ülke sütunu ise TEXT, varsayılanı ''.
Tek çözücü paylaşmak, çözülemeyen adresleri **0 adlı bir grup** olarak
çiziyordu — gerçek bir ağ numarası gibi görünen bir şey. Gerçek
veritabanı buldu; artık birim testi de var.

##### Yapılan, ikinci kısım: kırılım olmayan görünümler

**Skor dağılımı** ve **kesişim özeti** site sayfasında iki bölüm;
**sessiz adresler** ve **JavaScript çalıştıran botlar** kendi sayfalarında,
kesişim bölümünden ulaşılıyor. Gezinmeden değil — o, onları o bölümün
parçası olmaktan çıkarıp ayrı bir hedefe dönüştürürdü.

**Kesişim, ürünün asıl iddiası.** Yalnız ölçüm koduna dayanan bir araç,
kodu çalıştırmayan ziyaretçiyi göremez — dolayısıyla **kaçırdığı
kitlenin büyüklüğünü de bildiremez**. Bu bölüm o farkı sayıyor, ve
uçtan uca ölçüldü: beş adres collector'a ulaştı, hiçbiri beacon
çalıştırmadı; beacon tabanlı bir araç "sıfır ziyaretçi" derdi, bu sayfa
beşini listeliyor.

**`beacon_only_ips` bir ölçüm değil, yapılandırma işareti.** Doğru
kurulumda sıfırdır, çünkü sayfayı açan tarayıcı zorunlu olarak önce
collector'dan geçmiştir. Sıfır olmadığında diğer sayıların yanına
çizilmiyor — kendi cümlesiyle, ne anlama geldiği yazılarak gösteriliyor.
Sıfırken hiç çizilmiyor: bir arıza göstergesini ölçüm gibi okutmamak
için.

**Kapı aynı kapı.** Adres listeleri de rolle reddediliyor (tercihle
değil), 404 ile, ve eşli test tekrarlandı — varsayılmadı: bunlar farklı
bir handler ve farklı bir kayıt, ve bir handler'daki kapı diğeri
hakkında hiçbir şey kanıtlamaz. Kaldırma testiyle doğrulandı: kontrol
çıkarılınca izleyici 200 alıp adresleri görüyor.

##### İki hata, ikisi de yapısal testlerden

**İnline stil CSP'ye takıldı — ve yorumumda tersini yazmıştım.**
Histogram çubuklarını `style="width: N%"` ile çizmiştim ve yorumda
"politika stil özniteliğine izin veriyor" demiştim. Vermiyor:
`style-src 'self'`, `unsafe-inline` yok. Yapısal test tarayıcıya
gitmeden söyledi. On bir genişlik sınıfına çevrildi; %10 çözünürlük bir
bakışta okunan şekil için fazlasıyla yeterli.

**Tarayıcı testinin değişmezi eksik kaldı.** "Her bölüm ya tablo çizer
ya sebebini söyler" doğruydu, ta ki histogram üçüncü bir şekil
(`<ul>` çubuk listesi) getirene kadar: 11 bölüm, 4 tablo, 6 açıklama —
biri sayılmıyordu. Değişmezi gevşetmek yerine ölçüm genişletildi, ve
çubuk sayısının sıfır olmaması ayrıca kontrol ediliyor: sıfır olması,
engellenmiş bir inline stilin görüneceği şeyin ta kendisi.

##### Yapılmayan: ham dışa aktarma

**Bir çizim işi değil, bir karar.** `Snapshot` satırı her satırında `IP`
taşıyor. Veritabanında ham adres yok (A7.8) ama dışa aktarma **panelden
çıkan bir dosya** üretir: hiçbir saklama politikasının kapsamadığı,
oturumdan uzun yaşayan, müşterinin istediğine gönderebileceği bir şey.

**Verilen karar (kullanıcı, 2026-08-27):** maskeli mi maskesiz mi
olacağı **geliştirici şifresine bağlı bir ayar** olacak,
`KeyPrivacyIPStorage` ile aynı kapının arkasında. "Daha sonra istersek
değiştiririz" — varsayılan bugünden bağlanmıyor, ama ayarın kapısı
belli.

*Not: `privacy.ip_storage` **zaten** o kapının arkasında — A7.5'in
`RequiresDeveloperPassword` taşıyan altı ayarından biri. Eklenecek yeni
bir kapı yok.*

#### D4 — Ayar sayfaları ✅ **D4a yapıldı** *(2026-08-30)* · akan pencere (D4b) B1+B2'yi bekliyor

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

> **D4a yapıldı.** `/site/{site}/ayarlar`. Arka ucun tamamı hazırdı ve
> hiç yüzeye çıkmamıştı — `SettingsView`, `ApplySetting`, dört erişim
> seviyesi ve `devgate` — yani bu faz mekanizma inşası değil, var olanı
> gösteren sayfaydı. Ve o tek eksik **A2, B3 ve L3'ü birden**
> bekletiyordu.
>
> **İzleyici de görüyor**, üyeler sayfasının tersine: her satır değeriyle
> ve kontrol yerine gerekçesiyle. Yazmayı reddeden sayfa değil, sunucu —
> testler kontrolleri tıklamıyor, sayfanın çizmediği formu POST ediyor.
>
> B6'nın aynası yeni kapıyı anında yakaladı ve listeye eklettirdi.
>
> **Altı kusur çıktı**, üçü sayfa yazılırken (parola eksikken 200 cevabı,
> İngilizce iç hata mesajının panele sızması, TOML tablo tuzağı) ve üçü
> mutasyon turunda. Sonuncular arasında bir müşteriler-arası yazma
> yolunun test edilmemiş olduğu ve `ErrPreconditionUnmet`'in "değer
> geçersiz" diye gösterilmesi vardı — ikincisi okuyanı zaten doğru olan
> şeyi kontrol etmeye gönderiyordu.
>
> **Bir mutasyon, kodun değil testin yanlış olduğunu gösterdi:** doğru
> parolayla yazılan yönlendirme testi düzeltilmemiş kodda düştü, çünkü
> reddedilecek bir şey yoktu. İddia gerçekti ama zaten doğru yerde
> kanıtlanmıştı (`SetGuardedSetting`), ve handler'daki türetmenin ikinci
> katman olduğu artık yorumda yazıyor.
>
> **Korumalı ayarların `Superadmin` istediği doğrulandı** — o bayrak
> üretimde yalnız sahibin onayladığı geliştirici bağlantısıyla geliyor,
> yani müşterinin kendine veremeyeceği tek şey. SOZLUK §3'teki ilke
> çalışıyor.

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

### G. Yayın hattı — disiplinin insan hafızasına bağlı kalmaması

Bu grup, "ürün olmak için üç eksik" listesindeki ilk ikisine sahip
çıkıyor. Üçüncüsü (e-posta yolu) zaten C7'nin.

**Neden kendi grubu, F'nin içi değil.** F "bilerek sonraya bırakıldı"
demek. Bu ikisi bırakılmadı — engelliyor. Aynı listeye koymak
önceliği hakkında yalan söylemek olurdu.

---

#### G1 — CI: neyin birleşmeyi engellediği, neyin yalnız haber verdiği ✅ **yapıldı**

**Neden bu faz var.** Bu projenin "bitti" tanımı (§9) gerçek bağımlılığa
karşı doğrulama. Bugün o doğrulamayı elle yaptım: dört etiket kümesi,
tarayıcı paketi, dört hedefe çapraz derleme, ve arada konteyner
veritabanını geri aldığı için baştan kurma. Bu bedel **her faz**
ödeniyor ve ödeyen şey bir insanın hatırlaması.

Denetimin en kötü iki bulgusu bağımlılıktaydı ve **okumayla
bulunamazdı** — `govulncheck` buldu. O aracı bir insanın hatırlamasına
bağlamak, denetimin kendi dersine aykırı.

##### Asıl tasarım kararı: dört test yüzeyi aynı şey değil

Bu depoda dört ayrı test yüzeyi var ve **hepsi birleşmeyi engellememeli**:

| Yüzey | Neye ihtiyacı var | CI'daki rolü |
|---|---|---|
| Varsayılan (`-race ./...`) | Hiçbir şey | **Kapı** |
| `integration` | PostgreSQL 16 + TimescaleDB 2.17.2, dört şema | **Kapı** |
| Tarayıcı (`CA_BROWSER_TEST=1`) | node + playwright + chromium | **Kapı** |
| `loadtest` | Gerçek eşzamanlı TCP, zamanlamaya duyarlı | **Gecelik** — aralıkları gerçek makinede ayarlandı; paylaşımlı koşucuda gürültülü olabilir. Kararı tahminle vermeyeceğiz: gecelik koşup N koşuda kararlı çıkarsa kapıya alınır |

Ve bir test **hiçbir zaman kapı olmamalı**: `internal/botdata.TestLiveFetch`
halka açık internete çıkıyor. Bugün oturum boyunca üç kez denendi,
üçünde de `thebotaquarium.com` erişilemedi — düz `curl` de zaman aşımına
uğradı. Değişiklikle ilgisi olmayan sebeplerle kırmızı olan bir test
kapı olamaz. **Silinmemeli de**: varlık sebebi tam olarak "yukarı akış
veri kaynağı taşındı mı, şekli değişti mi" sorusu. Yani gecelik koşar ve
**bildirir**, engellemez.

`govulncheck` de hem PR'da hem **zamanlanmış** koşmalı: değişmemiş koda
karşı çıkan yeni bir CVE, yalnız-PR koşusunun yapısal olarak kaçırdığı
durumdur.

##### Bugünün kanıtladığı, aşikâr olmayan gereklilik

**Entegrasyon paketi aynı işte, aynı veritabanına karşı iki kez
koşmalı.**

Bugün üç test bulundu ve üçü de yalnızca *daha önce koşmadıkları* bir
veritabanında geçiyordu: biri kendi yazdığı 120 günü ikinci koşuda
"değişen bir şey yok" olarak görüyor ve varlık sebebi olan parola
kontrolüne hiç ulaşmıyordu; biri hesabı olmayan bir adrese yapılan
girişleri biriktirip hız sınırını tetikliyor ve **panelde olmayan bir
hesap sızdırma açığı** bildiriyordu.

Her koşuda taze veritabanı sağlayan bir CI bu üçünü **gizlerdi** —
sonsuza kadar, ve yalnız bir geliştiricinin makinesinde ortaya
çıkarlardı. İki kez koşmak, başka türlü görünmez olan bir hata sınıfı
için ucuz bir kapı.

##### Kapıda ne var

Biçim ve statik: `gofmt -l`, `go vet` (iki etiket kümesi de),
`govulncheck`. Testler: yukarıdaki üç kapı yüzeyi, entegrasyon **iki
kez**. Derleme: dört hedefe çapraz derleme (`linux/amd64`,
`linux/arm64`, `darwin/arm64`, `windows/amd64`). Belgeler: Türkçe
karakter bozulması kontrolü — bugüne kadar elle yapıldı, kullanıcının
açık şartı ve bir `grep`'lik iş.

##### CI'nın gereksiz kılmadığı şey

Gerçek bağımlılığa karşı doğrulama **standart olarak kalıyor**; CI aynı
paketleri koşuyor, çıtayı indirmiyor. Ve CI'nın koşamadığı bir doğrulama
sınıfı var: "gerçek collector'ı çalıştır, veritabanından ayarı değiştir,
logda göründüğünü izle" gibi keşif niteliğindeki kontroller. Onlar faz
sonunda elle yapılmaya devam edecek.

**Bitti ölçütü:** bilerek bozulmuş bir biçimlendirme, bilerek eklenmiş
bir yarış koşulu ve bilerek eskitilmiş bir bağımlılık içeren üç ayrı
PR'ın hattı tarafından reddedildiği **bir kez gösterilir**. Yeşil bir
hattın bir şeyi yakaladığının tek kanıtı, bir şeyi reddettiğini
görmektir.

##### Karşılandı (2026-08-26)

**Reddettiği gösterildi**, üçü de ayrı bir kopyada, kapının kendi
komutlarıyla: biçimlendirilmemiş dosyayı `gofmt -l` listeledi; sekiz
goroutine'in paylaşılan bir sayacı artırdığı gerçek bir yarışı `-race`
`WARNING: DATA RACE` ile bildirdi; zafiyetli araç zinciri için uydurma
gerekmedi — **ağacın kendisinde vardı**, `govulncheck` 34 erişilebilir
stdlib bulgusu verdi ve `go.mod` 1.25.13'e alındıktan sonra sıfıra
düştü.

**Kabul ettiği de gösterildi**, gerçek GitHub Actions üzerinde
([koşu #1](https://github.com/cruciblelab/Crucible-Analytic/actions/runs/32952332945)):
yedi işin hepsi yeşil, 4 dk 15 sn. Entegrasyon işi gerçek
`timescale/timescaledb:2.17.2-pg16` servis konteynerine karşı **iki kez**
koştu (101 sn ve 73 sn) ve ikisi de geçti — yani tekrar koşulabilirlik
taze bir CI veritabanında da tutuyor.

Tarayıcı testleri **atlanmadı**: adımda `CA_BROWSER_TEST=1` ayarlıyken
atlama dalı hiç çalışmıyor, test ya koşuyor ya da `Prepare` hatasıyla
düşüyor. `CA_CHROMIUM` verilmedi, yani `internal/browsertest`'in
"kimse tarayıcı adı vermedi → `executablePath` hiç yazma" dalı — bu
makinede erişilemeyen dal — CI'da gerçekten çalıştı.

---

#### G2 — Sürüm paketi: müşterinin kurduğu şey ✅ **yapıldı**

**Neden erteleme değil.** KURULUM.md 17 bölüm elle yapılacak iş
anlatıyor. Dürüst — ama ürünün ilk izlenimi bu, ve elle yapılan her adım
yanlış yapılabilen bir adım. Rol ayrımı bu sistemin güvenlik temelinin
yarısı ve şu an elle yazılıyor.

##### Ölçülen bulgu: beş binary'nin dördü sürümünü söyleyemiyor

KURULUM.md §3 beş binary'yi de şu döngüyle derletiyor:

```bash
go build -ldflags "-X main.version=$VERSION" -o "bin/$b" "./cmd/$b"
```

Ölçüm (2026-08-26, `go tool nm`): `main.version` sembolü **yalnız
`panel`'de var**. Diğer dördünde yok, ve Go linker'ı olmayan bir sembole
`-X` verilmesinden şikâyet etmiyor — sessizce hiçbir şey yapıyor. Yani
belgedeki komut dört binary için işlevsiz ve bunu söyleyen bir şey yok.

Destek "hangi yapıdasınız" diye sorduğunda cevap veremeyen dört süreç
demek bu. **Sembolü eklemek G2'nin işi** (yapının özelliği), onu sağlık
sayfasında ve operasyon kaydında göstermek **B7'nin** — ve sıra bu
şekilde olmalı: hiçbir şeyin okumadığı bir sürüm damgası, A5.2'de
kaldırdığım "hiçbir şey yapmayan ayar" deseninin aynısı olurdu.

##### Kapsam

- **Tekrarlanabilir derleme:** `-trimpath`, `CGO_ENABLED=0`, sabitlenmiş
  Go sürümü. Aynı commit, aynı bayt.
- **Sürüm damgası beş binary'de de**, yukarıdaki ölçümün gereği.
- **systemd unit dosyaları** dört uzun ömürlü servis için, bu projenin
  duruşunun gerektirdiği sıkılaştırmayla: `User=`, `NoNewPrivileges=`,
  `ProtectSystem=strict`, `PrivateTmp=`, log ağacı için dar bir
  `ReadWritePaths=`.
- **Pakette ne var:** binary'ler, dört `schema.sql`, örnek
  yapılandırmalar, `LICENSE` + `NOTICE` + `THIRD-PARTY.md`, `KURULUM.md`.
- **Pakette asla ne yok:** bot verisi (A10 kuralı: mekanizmayı gönder,
  veriyi değil), toplanan analitik, loglar, gerçek yapılandırma
  dosyaları. `.gitignore`'daki liste — yayın anında da **makineyle**
  kontrol edilir, sözle değil.
- **SHA-256 toplamları.**

##### Kapsam dışı, bilerek

Kurulum betiği **F2**'nin (aşağıda) — kurulacak bir şey olduktan sonra
sırası gelir. İmzalama/noter onayı: kimsenin istediğine dair kanıt yok.

##### Ne oldu, ve ölçüm neyi çürüttü

**Sürüm damgası beşinde de tutuyor.** `internal/buildinfo` + her main'de
`-version`. Sembolü hiçbir şeyin okumadığı durum bu projenin reddettiği
desen olduğu için bayrak G2'de eklendi; sağlık sayfasında göstermek hâlâ
B7'nin.

Fazladan bir şey de çıktı: Go zaten her yapıya commit'i gömüyor, o yüzden
**damgasız bir yapı bile doğru bir şey söylüyor** (`beb1834b8960-dirty`).
"Boş bırakmak dürüsttür" yerine "bildiğini söyle" — ve binary gerçekten
biliyordu.

**Tekrarlanabilirlik iddiası ilk hâlinde yanlıştı.** İki yapı aynı
dizinden koşunca aynı baytları verdi; **farklı dizinden** koşunca beş
binary de farklı çıktı. Sebep `-trimpath` değildi (o ikisinde de
çalışıyordu) — benim az önce eklediğim **VCS gömme**siydi.

Mantık şu: tekrarlanabilirliğin amacı, kaynak tarball'ını indiren birinin
aynı baytları üretebilmesi. O kişinin git deposu **yok**. Yani git verisi
gömen bir yapı, tam da varlık sebebi olan kişi tarafından doğrulanamaz.
`-buildvcs=false` eklendi; sürüm zaten `-X` ile geliyor.

**Aynı dizinde iki kez derleyen bir test bunu kaçırırdı** — o yüzden test
`.git`'i olmayan bir kopyadan derliyor.

**Kontrol betikten ayrıldı: `release/verify.sh`.** İlk hâlinde "asla"
listesi `build.sh`'ın içindeydi, ve testi yazınca ortaya çıktı ki betik
başta `rm -rf` yaptığı için ekilen dosyalar kontrolden önce siliniyor.
Daha önemlisi: yalnız betiğin **kendi kopyaladığı** dosyaları görebilen
bir kontrol, paketi değil `cp` satırlarını denetler. Ayrı betik, indirilen
bir tarball'a da doğrultulabiliyor — ve Go kurulumu olmayan kişi de
koşabiliyor.

**systemd birimlerinde çalışmayan bir satır vardı.** `StartLimitBurst` ve
`StartLimitIntervalSec` `[Service]`'te yazılmıştı; systemd bunları v229'da
`[Unit]`'e taşıdı ve **yanlış bölümde sessizce yok sayıyor**. Elle
yazdığım direktif-adı kontrolü bunu memnuniyetle geçirdi — adlar gerçek,
yalnız bölüm yanlıştı. `systemd-analyze verify` söyledi. Bu, fazın var
olma sebebiyle **aynı şekil**: doğru görünen, kabul edilen, hiçbir etkisi
olmayan bir satır.

**Bitti ölçütü — karşılananlar:** aynı commit'ten `.git`'i olmayan bir
kopyada ve farklı önbellekle alınan iki yapının **toplamları aynı**;
pakette "asla" listesinden dosya olmadığı makineyle doğrulanıyor **ve
altı farklı yasak dosya ekilerek reddettiği gösterildi**; bozulmuş bir
binary yakalanıyor; birimler systemd'nin kendisiyle doğrulanıyor; beş
binary de sürümünü söylüyor.

**Karşılanmayan:** paketin temiz bir VM'de açılıp KURULUM.md izlenerek
çalıştığı. Bu konteynerde yapılamaz — gerçek bir sunucuda elle
yapılacak, ve **F2 (kurulum betiği) tam da bunu otomatikleştirecek faz**.

---

### H. Güvenlik taraması — okumanın bulamadığı sınıf

**Neden bu grup var.** Kullanıcının cümlesi şuydu: *"planlandığı gibi
gidiyoruz ama güvenlik açıklarına çok açık şekilde gittiğimizi
hissediyorum."* His ölçülebilir bir iddiadır, ve ölçüldü. İki soru
soruldu, ikisinin de cevabı boş döndü:

```
=== bağlantı yolunda recover var mı:   (boş)
=== fuzz hedefi var mı:                (boş)
```

Depoda `internal/` ve `cmd/` altında **tek bir `recover()` yoktu** ve
**tek bir fuzz hedefi yoktu**. AI.3 denetimi (OWASP/CWE/ASVS) kodu
*okuyarak* sekiz bulgu çıkarmıştı; bu grup, okumanın yapı gereği
bulamadığı sınıfı hedefliyor.

**O sınıfın somut ispatı, bu grubun kurulduğu gün bulunan iki delik.**
İkisi de kod okunarak fark edilmedi; ikisi de "diğer üç sunucu bunu
yapıyor, bu biri yapmıyor" şeklinde **yapısal bir soru** sorulunca
düştü:

| Bulunan | Nasıl | Etkisi |
|---|---|---|
| `internal/proxy` bağlantı goroutine'inde `recover()` yok | Yapısal soru | Herhangi bir panik **tüm süreci** öldürüyordu — collector müşterinin sitesinin önünde durduğu için **sitenin kapanması** demek |
| `internal/fullproxy` `http.Server`'ında hiçbir zaman aşımı yok | `gosec` G112 | TLS el sıkışması **hiç sınırlanmıyordu**; tek baytla süresiz bağlantı tutulabiliyordu |

İkisi de aynı gün düzeltildi ve ikisinin de testi, düzeltme geri
alınınca kırmızıya dönüyor (ölçüldü: biri test ikilisini panikle
öldürüyor, diğeri 10 sn sonra hâlâ bağlantıyı tutuyor).

**Grubun asıl dersi bu tabloda:** iki delik de *tek bir bileşenin,
diğerlerinin yaptığı bir şeyi unutması*ydı. Bu yüzden H4 var.

---

#### H1 — Saldırgan baytlarını okuyan ayrıştırıcıları fuzz'la 🟡 **ja4 yapıldı**

**Kapsam.** Doğrulanmamış, kimliklendirilmemiş, şifresi çözülmemiş
baytları okuyan her ayrıştırıcı. `internal/ja4` bugün yapıldı:
`FuzzParseClientHelloFromRecords` ve `FuzzParseClientHello`, tohum
korpusu beş gerçek FoxIO yakalaması **artı** her katmanda budanmış ve
sahip olduğundan fazlasını iddia eden uzunluklar.

**Ölçüm:** iki hedefte toplam **~19,6 milyon çalıştırma, sıfır çökme**
(9.641.740 + 9.984.884). `cursor.go`'nun her okumada `ok` döndürme
disiplini işini yapıyor. Bu bir "temiz çıktı" değil, **regresyon
koruması**: bundan sonra sınır kontrolünü kaldıran bir değişiklik
sessizce geçemez.

Kalan hedefler, saldırgana yakınlık sırasıyla: beacon'ın JSON gövdesi
(tarayıcıdan gelir), `asnlookup`'ın aralık tabloları, `botdata`'nın
yukarı akıştan çektiği JSON.

**Neden tohum korpusu gerçek el sıkışmalardan başlıyor.** Rastgele
baytlardan başlayan bir mutasyoncu uzantı ayrıştırmasının derinine
neredeyse hiç ulaşamaz; geçerli bir hello'dan başlayan ise ilk
saniyelerde ulaşır. Fark ölçüldü: korpus 10'dan 143'e çıktı, yani
mutasyoncu sürekli **yeni kod yolu** buluyordu.

**Bitti ölçütü:** dört ayrıştırıcının dördünde de hedef var; gecelik
işte korpus biriktirerek koşuyor; bulunan her çökme `testdata/fuzz/`
altına commit'leniyor (Go bunu kendisi yapar ve o dosya kalıcı bir
regresyon testine dönüşür).

---

#### H2 — Statik tarama, taban çizgisiyle ✅ **yapıldı**

> **Yapılırken bir karar tersine döndü ve bu başlıkta duruyor.** Bu faz
> "kapı değil rapor" diye planlanmıştı; gerekçe 28'de 1 oranıydı. Taban
> çizgisi o oranı ortadan kaldırdı: çıplak tarama 21 bulgu, taban
> çizgisine karşı **sıfır**. Artık kırmızı "değişikliğin yeni bir bulgu
> soktu" demek, yani değişiklik hakkında bir şey söylüyor — kapının
> tanımı bu. **Kapıya alındı.** Gerekçeyi çürüten şey ölçüm değişikliği
> değil, altındaki olgunun değişmesiydi.
>
> Üçlemenin yapıldığı yer de değişti ve bu ikinci sebep: satırı yeni
> yazan kişi neden güvenli olduğunu bilir; gecelik koşu o soruyu, kodu
> yazmamış birine sorar.

**Bugün ölçülen oran, bu fazın tüm tasarımını belirliyor.** `gosec`
111 dosya / 29.984 satırda **28 bulgu** verdi. Elle üçlendi:

| Bulgu | Adet | Gerçek mi |
|---|---|---|
| G112 zaman aşımı yok | 1 | ✅ **Gerçek** — düzeltildi (yukarıdaki tablo) |
| G710 açık yönlendirme | 4 | ❌ Hayır — `rawNext` zaten `//host` **ve** `/\host` reddediyor |
| G115 tamsayı taşması | 5 | ❌ Hayır — hepsi sınırlı; `Score` kaynağında `MaxScore = 100`'e kırpılıyor |
| G304 değişkenle dosya açma | 5 | ❌ Hayır — yollar kendi yapılandırmamızdan, kullanıcı girdisinden değil |
| G301/G302 izinler | 2 | ❌ Hayır — herkese açık bot verisi, `0644` bilerek |
| G703 yol geçişi | 1 | ❌ Hayır — kendi test yardımcım |
| G104 işlenmemiş hata | 9 | ❌ Hayır — çoğu `defer x.Close()` |

**28'de 1.** Bu oranı bir kapıya bağlamak, hattı %96 gürültüyle
kırmızıya boyamak olurdu — ve G1'in dersini tekrar ederdi: *her test
birleşmeyi engellememeli.* Kırmızının anlamı olması, kırmızının nadir
olmasına bağlı.

**Tasarım:** taban çizgisi dosyası. Bugünkü gürültü, **her biri neden
gürültü olduğu yazılarak** dondurulur; tarama yalnız **yeni** bulgu
çıkınca konuşur. Bir kapı isteniyorsa kapı "sıfır bulgu" değil,
**"taban çizgisinde olmayan bulgu yok"** olur. (G112 düzeltildikten
sonra `-severity=medium` ile kalan sayı **18**.)

##### gosec'in kendi bastırması bu iş için kullanılamaz — ölçüldü

Aracın tek yerleşik mekanizması `--exclude-rules="yol:KURAL"`, yani
**yola** göre. Bunun ne yaptığı tahmin edilmedi, denendi: bir dosyada
bir kural bastırıldıktan sonra **aynı dosyaya aynı kuraldan gerçek bir
ikinci bulgu** eklendi.

```
bastırma yokken:  2 G402
bastırma varken:  0 G402
```

Yani mekanizmanın kendisi, bu grubun kapatmak için var olduğu deliği
açıyor. Ve tam olarak yanlış dosyada açıyor: `internal/panel/web/auth.go`
bugün üç bulgu taşıyor **ve** C7.3 ile D grubunun büyüteceği dosya.

**Taban çizgisi bu yüzden kural + dosya + işaretlenen kodun hash'i ile
anahtarlanacak, yola göre değil.** gosec'in JSON'undaki `code` alanı
buna uygun (satır numarası önekleri soyulduktan sonra). Üç davranışı da
doğru veren tek anahtarlama bu:

| Olan | Olması gereken | Hash anahtarlı |
|---|---|---|
| Dosyada satırlar kayıyor | Ses çıkarma | Hash aynı — sessiz ✅ |
| İşaretli satırın kendisi değişiyor | Yeniden üçle | Hash değişti — konuşur ✅ |
| Aynı dosyaya aynı kuraldan **yeni** bulgu | **Bildir** | Yeni hash — konuşur ✅ |

##### Sıralama: neden C7.3'ten önce

Tarayıcının hangi fazda işe yarayacağı da tahmin edilmedi, denendi.
Sonraki fazların yazacağı kod sınıfları kasten yazılıp tarandı:

```
G402 (CWE-295) TLS InsecureSkipVerify → HIGH/HIGH   ... C7.3'ün SMTP'si
G204 (CWE-78)  değişkenle alt süreç   → MEDIUM/HIGH ... F2'nin betiği
```

"SMTP bağlanmıyor" diye sertifika doğrulamasını kapatmak, e-posta kuran
her projede en az bir kez yazılan satır — ve `gosec` onu en yüksek
güvenle yakalıyor. Aynısı B1'in sıkıştırılmış log görüntüleyicisi (arşiv
yolu geçişi) ve G2'nin paketi için de geçerli.

Yani tarayıcının **en çok işe yarayacağı fazlar sonrakiler**. H2'yi
onlardan sonraya almak, en riskli kodu taramasız yazmak olurdu. Ama
H2'yi öne alıp aracın kendi bastırmasını kullanmak daha kötü olurdu:
erken almanın tüm faydası, ilk bastırılan dosyada geri verilirdi.
İkisi birlikte tek sonuç veriyor — **önce, ve hash anahtarlı.**

##### Yapıldı: `internal/sast`, ve ölçülen dört davranış

Parmak izi **kural + dosya + işaretli kodun hash'i**; satır numarası
hiç girmiyor. Üç davranış birim testinde, dördü uçtan uca ölçüldü:

| Durum | Beklenen | Ölçülen |
|---|---|---|
| Temiz depo | sessiz | `no new findings (21 in the report, all in the baseline)`, çıkış 0 |
| **Zaten taban çizgisi girişi olan dosyaya** gerçek G402 eklendi | bildir | `NEW G402 auth.go:464 (HIGH/HIGH)`, çıkış 1 |
| Düzeltilmiş bulgu listede kaldı | bildir | `STALE G112 fullproxy/server.go`, çıkış 1 |
| Aynı kod farklı satıra kaydı | sessiz | parmak izi değişmiyor |

İkinci satır bu fazın tüm gerekçesi: **gosec'in kendi mekanizmasının
yuttuğu durum** (2 → 0), burada çıkış kodu 1 veriyor.

**Taban çizgisi 21 giriş, 18 değil.** Fark, aracın kendi kodunda çıkan
üç bulgu — tarayıcı kendini de tarıyor, muafiyet listesi sıfırdan
başlamıyor. `TestBaseline_CoversTheToolItself` bunu koruyor.

Yerleşim: `internal/sast/cmd/sastdiff`, kökteki `cmd/` değil. "Bu
ürünün kaç binary'si var" sorusunun kesin bir cevabı var ve bir
geliştirme aracı onu değiştirmemeli.

**Sürüm sabitlendi (`v2.29.0`), `@latest` değil.** Sabitlenmemiş bir
tarayıcı, kapının altındaki taban çizgisinin anlamını kimse karar
vermeden değiştirir — bir sabah herkes, kimsenin benimsemediği bir
kural yüzünden engellenir. Gecelikte `@latest` koşuyor, tam da bunu
öğrenmek için: yeni gosec'in gördüğü fazladan şey varsa bu bir insan
kararıdır, bozuk bir yapı değil.

**Bitti ölçütü — hepsi karşılandı:** taban çizgisi commit'li ve **her
satırında gerekçe var** (testi yazan kişinin kendi tembel gerekçesini
yakaladı: "Same tool, same reason" dört kelimeydi ve reddedildi);
kasten sokulmuş bir açık taramada görünüyor; eskiyen giriş bildiriliyor;
ve mutlak yol kontrolü var — `gosec` mutlak yol yazıyor, parmak izi
yolu içeriyor, yani normalleştirme olmasa taban çizgisi CI'da hiçbir
şeyle eşleşmez ve her gece 21 bulgu "yeni" görünürdü.

---

#### H3 — Sır taraması ve bağımlılık kapısı

**Neden ayrı.** `govulncheck` zaten kapıda ve *erişilebilirlik*
analiziyle iki gerçek bulgu çıkardı (AI.3). Ama iki soruyu hiç
sormuyor: (1) depoya **sır** sızdı mı, (2) bir bağımlılık **yeni**
eklendiğinde onu kim gördü.

Birincisi bu proje için sıradan bir hijyen maddesi değil: yapılandırma
dosyası geliştirici parola hash'ini ve IP anahtarını taşıyor, ve
`KURULUM.md` gerçek `GRANT` blokları içeriyor. Yanlış bir kopyala-yapıştır
tam olarak bu depoya sır sokar. Tarama **git geçmişini** de kapsamalı —
sonradan silinen bir sır silinmiş olmaz.

**Bitti ölçütü:** geçmiş dâhil tarandı ve çıkan her eşleşme ya gerçek
(döndürüldü) ya da yazılı gerekçeyle test verisi; yeni bağımlılık
eklendiğinde PR'da görünüyor.

---

#### H5 — Veritabanı yüzey denetimi ✅ *(2026-08-27)*

> Kullanıcı sordu: *"TimescaleDB'lerde yapay zekâlar bayağı arka uç
> bırakıyor diyorlar, tekrar bir güvenlik açığı var mı diye kontrol eder
> misin."* Genel iddiayı ne kabul ettim ne reddettim — **ölçtüm.**
> Gerçek TimescaleDB'ye (16.13 / 2.17.2) `install.sh` uygulandı ve her
> rolün gerçekte nereye ulaşabildiği tek tek soruldu.

##### Aranmayan yerde bulundu

Kod tarafı temiz çıktı. SQL'e giden her değer bağlı parametre; iki yerde
tanımlayıcı (sütun adı) enterpolasyonu var, çünkü Postgres'te
tanımlayıcı için yer tutucu yok — ikisi de kapalı tip arkasında.
Rollerin hiçbirinde `SUPERUSER`, `CREATEDB`, `CREATEROLE`, `BYPASSRLS`
yok; `public` şemasında `CREATE` yok; `pg_authid`, `pg_read_file`,
`COPY TO file` hepsi reddediliyor; `pg_stat_activity` başka rolün sorgu
metnini `<insufficient privilege>` diye gösteriyor. Chunk'lar — hem
mevcut hem sonradan oluşan — hypertable yetkilerini doğru miras alıyor:
`beacon_writer` kendi yazdığı satıra chunk üzerinden de ulaşamıyor.

**Açık, kimsenin vermediği yetkilerdeydi.** Üçü de bir `GRANT`
listesinde görünmüyor, çünkü hiçbiri `GRANT` ile verilmemiş — varsayılan
olarak açık geliyorlar:

| Bulgu | Ölçüm | Karar |
|---|---|---|
| **Arka plan işi zamanlama** | `panel_user` — panel dışında hiçbir yetkisi olmayan rol — `add_job()` çağırıp iş 1000'i aldı, sahibi kendisi, saatlik | Kapatıldı |
| **TimescaleDB telemetrisi** | `telemetry_level = basic`, 24 saatte bir `telemetry.timescale.com`'a rapor | Kapatıldı |
| **Veritabanına `PUBLIC` bağlanabiliyor** | `datacl` içinde `=Tc/postgres` | Kapatıldı |

##### Neden asıl bulgu birincisi

`add_job` yetki yükseltmesi **değil**: iş, sahibi olan rol olarak
çalışır, yani o rolün psql'de yapamayacağı hiçbir şeyi yapamaz. Ama iş,
onu yaratan oturumdan, bağlantı havuzundan ve **uygulamanın yeniden
başlatılmasından** sağ çıkar.

> Bir dakikalığına ele geçirilmiş bir süreç, aylarca çalışmaya devam
> eden, kimsenin bakmadığı bir yerde duran, servisi yeniden başlatmanın
> kaldırmadığı bir şey bırakabilir. Taşıdığı yetki ne olursa olsun,
> **arka kapının şekli budur.**

Ayrıca arka plan işçi yuvaları küme genelinde sabit bir havuz — yani
aynı zamanda hiçbir şeyin sınırlamadığı bir hizmet reddi.

Bu üründe hiçbir şey iş zamanlamıyor. Sıkıştırma ve saklama politikaları
— bir kurulum isterse — kurulumu yapan superuser tarafından uygulanır,
ki kararın ait olduğu yer orasıdır.

##### Kendi betiğim kendi kuralını kanıtladı

`harden.sql`'in başına *"çalışan bir REVOKE ile etkili olan bir REVOKE
farklı olgulardır"* yazmıştım. İlk çalıştırmada döngü `run_job`'da
durdu — o bir **procedure**, `REVOKE ... ON FUNCTION` değil `ON ROUTINE`
ister — ve `DO` bloğu ilk hatada durduğu için listedeki sonraki adlar
sessizce açık kaldı. Betik "çalıştım" dedi. `verify.sql` yakaladı.

##### Kodda iki düzeltme

- **`SaveMailAccount`'ta SQL birleştirmesi.** Üç sabit değişmezden biri
  seçiliyordu; enjekte edilemezdi ama aynı depoda başka yerde kapalı tip
  kullanılan bir tehlike için hiçbir koruma yoktu. `CASE` ifadesine
  çevrildi: tek ifade, üç dal SQL'de görünür, her değer parametre.
- **Aynı desen iki farklı savunulmuş.** `countDistinct` hem kapalı tip
  hem beyaz liste kullanıyor ("belt and braces" diye yorumlanmış);
  `beaconBreakdown` yalnız tip. Bir tehlikeye iki standart, zayıf olanın
  incelemeden geçmesinin yoludur — güçlü olan ikisine de uygulandı, ve
  listenin sabitlerden kaymayacağını garanti eden ayna testi eklendi.

##### Kapatılamayan, raporlanan

**Bağlantı şifrelemesi.** Dört DSN'in hiçbirinde `sslmode` yok.
Hepsi `localhost` gösterdiği için bugün doğru — ama libpq'nun varsayılanı
`prefer`'dır: TLS'i dener, sunucu sunmuyorsa **sessizce şifresiz devam
eder.** Yani DSN'i uzak bir veritabanına çeviren kurulum, parolayı ve her
analitik satırı ağdan açık geçirir ve yapılandırmada bunu gösteren
hiçbir şey olmaz.

Zorunlu kılınmadı: tek makinelik kurulum — KURULUM.md'nin anlattığı ve
kurulumların çoğunun olacağı şey — TLS'siz doğrudur, ve bunu devri bloke
eden bir kontrol yapmak insanlara kırmızı bir satırı görmezden gelmeyi
öğretir. Bunun yerine **ölçülüyor**: `pg_stat_ssl` ile bağlantının
gerçekten şifreli olup olmadığı, `inet_server_addr()` ile sunucunun
gerçekten uzak olup olmadığı sorulup, ikisi birlikte uyarıya çevriliyor.

*(Bu kontrolün ilk hâli her loopback bağlantısına "uzak sunucu" diye
uyarı veriyordu: `inet_server_addr()::text` maskeyi koruyor —
`127.0.0.1/32` — ve hiçbir adres ayrıştırıcısı bunu kabul etmiyor. En
yaygın kurulumda yanlış alarm, yani tam olarak kontrollerin görmezden
gelinmesini öğreten şey. `host()` ile düzeltildi.)*

##### Bitti ölçütü

`release/sql/harden.sql` her kurulumda uygulanıyor; `verify.sql` 22
iddia taşıyor ve dördü yeni; sihirbaza iki kontrol eklendi
(`db.connection_encrypted`, `db.no_background_jobs`); ekilmiş bir işi
yakaladığı gerçek veritabanında ölçüldü.

---

#### H4 — Yapısal güvenlik değişmezleri: "biri unutunca" testi ✅ **yapıldı** (2026-08-30)

**Bu fazın gerekçesi bir tahmin değil, bu grubun kurulduğu gün bulunan
iki deliğin ortak şekli.** İkisi de şuydu: *dört sunucudan üçü doğru
şeyi yapıyor, biri yapmıyor.* `internal/api`, `internal/beacon`,
`internal/panel/web` dört zaman aşımını da kuruyordu; `internal/fullproxy`
hiçbirini. `net/http` üstünde koşan her şey bağlantı başına
`recover`'ı bedavadan alıyordu; `internal/proxy` — tek `http.Server`
kullanmayan — almıyordu.

İkisini de **bir insanın fark etmesi** bulmuş olurdu. Bu grubun tüm
iddiası, o insana bağlı kalmamak.

Yazılacak testler, iddiayı yorumda değil derlemede tutan cinsten
(§3.4'ün "listeye karşı bakılır, hafızaya karşı değil" kuralı):

- Dinleyen her `http.Server`'ın `ReadHeaderTimeout`'u var — sunucu
  listesi testte sayılır, yeni sunucu eklenip listeye girmezse test
  bunu söyler
- Saldırgan baytına dokunan her goroutine kökünde `recover` var
- Yeni bir `net.Listener` kabul döngüsü eklendiğinde ikisini de sorar

**Bitti ölçütü:** zaman aşımını kaldırmak testi kırıyor (bugün elle
ölçüldü: kaldırınca 10 sn sonra bağlantı hâlâ açık); `recover`'ı
kaldırmak testi kırıyor (bugün elle ölçüldü: test ikilisi paniğe
ölüyor); ve **listeye girmemiş yeni bir sunucu** testi kırıyor — bu
sonuncusu asıl olan, çünkü diğer ikisi zaten bugün var.

> **Yapıldı.** `internal/invariants/` — üretim kodu yok, hiçbir yerden
> import edilmiyor; içinde yalnız kaynak ağacını sözdizimi ağacı üstünden
> okuyan dört test var. Üç bitti ölçütünün üçü de mutasyonla doğrulandı:
>
> ```
> zaman aşımını kaldır  -> internal/api/server.go:466 builds an
>                          http.Server with no ReadHeaderTimeout
> recover'ı kaldır      -> internal/proxy/pipe.go:24 starts a goroutine
>                          with no recover
> listeye girmemiş yeni -> TestEveryHTTPServerBoundsItsHeaderRead +
> sunucu                   TestTheListeningPackagesAreTheOnesOnTheList
> ```
>
> Üçüncüsü ikisini birden kırıyor, ki istenen buydu: yeni sunucu hem
> kuralı çiğniyor hem de listede yok.
>
> **Muafiyet mekanizması gerekçeyi zorunlu tutuyor.** `internal/proxy`'de
> `ctx.Done()` kapatıcısı meşru bir istisna ve dosyada
> `// no-recover: <gerekçe>` satırıyla yazılı; çıplak muafiyet kabul
> edilmiyor. (İlk hali yorumun yalnız altı satırını okuyordu ve yedi
> satırlık gerekçe kesiliyordu — bitişik yorum bloğunun tamamını
> okuyacak şekilde düzeltildi.)

---

#### H'nin gereksiz kılmadığı şey

Tarama, denetimin yerine geçmez. AI.3'ün sekiz bulgusunun hiçbirini
`gosec` bulamazdı: yetki modeli, sızdıran hata mesajı, kayıt sırası
gibi şeylerin hepsi **niyet** gerektirir. Bugünkü oran da bunu söylüyor
— araç 28 şey işaret etti, birini doğru işaret etti, ve o birini de
*insan* gerçek olduğuna karar verdiği için düzelttik. Araçlar
okumadığın yeri tarar; okuduğun yeri anlamaz.

---

### F. Sonraya bırakılanlar (karar verildi: bu yayda değil)

Bunlar **kapsam dışı değil, ertelenmiş**. Fark önemli: §7'dekiler
yapılmayacak, buradakiler sonra yapılacak.

**F2 artık tam olarak "ertelenmiş" değil.** G grubunun doğal üçüncü
adımı: G2 kurulacak paketi üretir, F2 onu kurar. Numarası F2 kalıyor
çünkü commit mesajlarında ve risk tablosunda o adla geçiyor — bu
belgenin numaralandırma konusundaki kuralı, düzeltmek yerine
açıklamak.

#### F1 — Yedekleme

**Neden gerekli:** §5 "yedekten dönme"yi SSH gerektiren işler arasında
sayıyor — ama projede **yedek alma hiç planlanmamış.** Geri yüklenecek
bir şey olmadan geri yükleme prosedürü anlamsız. Müşterinin analitik
geçmişi ürünün kendisi; tek diske emanet edilmiş durumda.

**Kapsam:** zamanlanmış `pg_dump`, belgelenmiş geri yükleme adımları,
panelde `ShowLastBackup()` göstergesi, sağlık sayfasında "son başarılı
yedek" satırı.

#### F2 — Kurulum betiği ✅ **yapıldı**

Postgres + TimescaleDB, **dört veritabanı rolü ve GRANT'ları**, systemd
unit dosyaları, TLS. Tek betik. Rol ayrımı bu projenin güvenlik
temelinin yarısı ve şu an elle kuruluyor — yani yanlış kurulabilir.
Betik, 13. müşteriyi yarım günden on dakikaya indirir ve rol ayrımını
kurulumun garantisi hâline getirir.

**Ayrıca buraya ait: IP jeton anahtarının iki serviste aynı olduğunun
kontrolü.** (Kullanıcı kararı: *"kontrolü ekleriz"*.) `devpass -ipkey`
tek değer üretiyor ve o değer collector ile beacon'ın **iki ayrı**
yapılandırma dosyasına elle kopyalanıyor. Preflight anahtarın
*varlığını* doğrulayabiliyor, *aynılığını* doğrulayamıyor — iki servis
birbirinin dosyasını okumaz, okumamalı da. Farklı anahtarlar hataya
değil, **sessiz yanlışa** yol açar: kesişim birleşimi hiçbir satırı
eşleştirmez ve sebebini söyleyen bir mesaj olmaz. Kurulum betiği bu iki
dosyayı zaten birlikte yazan tek yer olduğu için doğru sahip odur:
anahtarı bir kez üretir, ikisine de yazar, sonra ikisinin hash'ini
karşılaştırıp bildirir.

##### Ne oldu: üç sessiz yanlış, üçü de koşarak bulundu

**1. Roller küme geneli, veritabanı başına değil.** "Hiçbiri yoksa
dördünü oluştur" mantığı, kümede zaten `collector` olan bir makinede
diğer üçünü hiç oluşturmuyordu — ve GRANT'lar var olmayan bir rolde
patlıyordu. Kısmi kalmış bir önceki kurulumda da aynısı olurdu. Her rol
tek tek kontrol edilip oluşturuluyor.

**2. `set -o pipefail` + erken çıkan boru tüketicisi.** İki kez, aynı
sebeple: `psql ... | grep -q` eşleşince grep hemen çıkıyor, psql SIGPIPE
alıyor, boru hattı 141 dönüyor — yani betik **başarı** durumunda
duruyordu. Sessizce. `newsecret`'teki `tr | head -c 32` de aynı şey.

**3. Ve asıl olan: tablo sahipliği, hiçbir GRANT'ın göstermediği yetki.**
Bir tablonun sahibi, GRANT'lardan bağımsız olarak o tablo üzerinde her
yetkiye **örtük** olarak sahiptir. Şemalar `collector` olarak
uygulanırsa o rol bütün tabloların sahibi olur ve yalıtım **hiç
yoktur** — üstelik her şey doğru görünür: GRANT'lar doğrudur, `\dp`
doğrudur, panel analitiği okuyabilir.

Bu, bu fazın kendi ilk koşusunda oldu: superuser DSN'i collector'dı,
bütün GRANT'lar doğru uygulandı, ve doğrulama collector'ın
`beacon_events`'e erişebildiğini bildirdi. `verify.sql` artık sahipliği
ve superuser'lığı da kontrol ediyor, KURULUM.md de uyarıyor.

##### Doğrulama, tek kaynak, ve reddetme

**GRANT bloğu artık tek yerde: `release/sql/grants.sql`.** KURULUM.md onu
tekrarlamıyor, işaret ediyor — ve bir test belgede GRANT satırı kalmadığını
tutuyor. Aynı bloğun iki yerde durması ayrışmaya davettir ve ayrışma hep
aynı yönde olur: betik koştuğu için düzeltilir, belge bir sonraki
işletmeciye başka bir şey söylemeye devam eder.

**Doğrulama negatifleri de içeriyor**, çünkü asıl özellik onlar: bir
GRANT bloğunun hatasız koşması ifadelerin kabul edildiğini kanıtlar,
**ne verilmediğini** söylemez. On üç iddia, sekizi negatif. Tek bir `f`
kurulumu bitirmiyor.

**Reddettiği gösterildi, geçtiği değil.** Dört yalıtım özelliği tek tek
bozulup betik yeniden koşuldu: panele analitik okuma verildi, API'ye
yazma verildi, denetim kaydı silinebilir yapıldı, bir servis rolü
superuser yapıldı. Dördünde de sıfırdan farklı çıkış ve hangi iddianın
düştüğünün adı.

##### IP anahtarı: ilk hâli kendi çıktısını kendisiyle karşılaştırıyordu

İlk yazdığım kontrol anahtarı üretiyor, iki dosyaya da yazıyor, sonra
karşılaştırıyordu — yani hiçbir zaman ayrışamazdı. Elle yanlış
kopyalanmış bir `beacon.toml` sessizce geçti. Oysa yakalanacak hata tam
olarak budur.

Şimdi **önce okuyor, sonra yazıyor**: iki dosyada da anahtar varsa ve
farklıysa reddediyor, hash'leriyle (anahtarın kendisiyle değil — ayrışma
bildirilmeye değer, sır değil). Var olan anahtar asla döndürülmüyor:
döndürmek, saklanmış her satırın takma adını kopartırdı.

##### Kapsam dışı, bilerek

**TLS sertifikası alınmıyor.** Sertifika, bu makineye çözümlenen bir alan
adı ve betiğin tek başına tamamlayamayacağı bir ACME sınaması istiyor;
yarısını yapmak, kimsenin hata ayıklayamayacağı yarım yapılandırılmış bir
ters vekil bırakırdı. Betik ne yapılacağını söylüyor, KURULUM.md §8'e
yönlendiriyor.

#### F3 — Filo izleme paneli

Bizim tarafımızda: kurulum listesi, destek token'ları, yoklama
zamanlayıcısı, tek ekranda hepsinin sağlığı. Müşteri sayısı azken elle
de yoklanabilir; sayı artınca gerekli olur.

---

### K grubu — Kanıt ve dağıtım *(planda yoktu, araya girdi)*

Bu üç faz sıradan çıkarak yapıldı, ve neden yapıldığı kadar **neden
sıradan çıkarak** yapıldığı da kayda değer.

Yukarıdaki A–H sırası özellik ekliyor. Bu üçü hiç özellik eklemedi;
**zaten yazılmış olanın çalıştığını** sordu — ve on iki gerçek kusur
buldu, hepsi de o sırada bir sonraki maddeye geçilseydi üstüne inşa
edilecek olan zeminde.

#### K1 — Uçtan uca kanıt ✅ *(2026-08-27)*

**Öncül:** depodaki her test bir parçayı ölçüyordu. Entegrasyon
paketleri bir paketi gerçek veritabanına karşı, `release/` kurulum
betiğinin ürettiği yetki matrisini. Hiçbiri şunu sormamıştı: *gerçek bir
istek panoda bir sayıya dönüşüyor mu?*

İki yarı ayrı ayrı geçiyordu, ve **ayrı ayrı geçen iki yarı çalışan bir
zincir değildir.**

`e2e/e2e_test.go` zinciri koşuyor: tarball'ı derliyor, paketin kendi
`install.sh`'ıyla temiz bir veritabanına kuruyor, dört süreci
başlatıyor, gerçek bir HTTPS isteğini collector üzerinden geçiriyor, bir
pageview atıyor, ve satırı veritabanında, JA4'ü satırda, saklama
politikasını iş kataloğunda, sayıyı okuma API'sinde, rakamı panonun
altı kartında arıyor.

**Bulduğu altı kusur** — beşinin kökü tek: geliştirme veritabanını
`collector` yaratmıştı, yani her tablonun sahibiydi, yani on entegrasyon
paketi hiçbir kurulumun vermediği yetkiyle koşuyordu.

1. Saklama politikası kurulu hiçbir sistemde çalışmamış (TimescaleDB
   yetkiye değil sahipliğe bakıyor; site kırpması için gereken `DELETE`
   de hiç verilmemiş). Üç `SECURITY DEFINER` sarmalayıcıyla çözüldü.
2. Kurulum sihirbazı doğru kurulmuş **her** sistemde "tablo eksik"
   diyordu — `information_schema` yetkisiz tabloları gizler. Zorunlu
   kontrol düşüyor, düşen zorunlu kontrol devir teslimi bloke ediyor:
   kurulum müşteriye hiç verilemezdi.
3. `ip_asn_ranges` ve `ip_country_ranges`'e hiç `GRANT` yoktu; ASN ve
   ülke sütunları her kurulumda kalıcı olarak boştu.
4. `install.sh`, DSN verildiğinde `--db`'yi yok sayıyordu.
5. Örnek yapılandırmalarda collector'ın arka ucu ve okuma API'si aynı
   portta.
6. `beacon.example.toml` geçerli TOML değildi; beacon hiçbir kurulumda
   başlamamıştı.

**Kalıcı hâle gelenler:** E2E zinciri, `release/ports_test.go`,
`release/schemalist_test.go`, `release/examples_test.go`, `--db` testi,
`verify.sql`'de yedi yeni doğrulama, ve `internal/testdb` — on paket
artık üretimde koştuğu rolle bağlanıyor.

#### K2 — Docker: tek imaj, beş giriş noktası ✅ *(2026-08-27)*

**Sıraya girme gerekçesi kullanıcının:** dağıtım modeli müşteri başına
konteyner, ve konteyneri sonraya bırakmak aradaki fazları yanlış
varsayımla yazmak olurdu — yükseltme yolu, F1 yedekleme, F3 filo, B5
destek token'ı, dördü de "dağıtım nasıl" sorusunun cevabına göre
şekilleniyor.

Ölçüldü: üç endişeden ikisi boş çıktı (loglama zaten stdout'a
düşebiliyor, systemd bloğu zaten korumalı). **Asıl mesele başkaydı** —
konteyner, kimsenin elle düzeltmediği bir kurulum olduğu için altı kusur
daha çıkardı, ve altısı da konteyner kusuru değil:

1. `install.sh` GNU sed'e bağımlı; BusyBox sed kullandığı biçimi
   **sessizce yok sayıyor**, yani "anahtarı yazdım" diyen her satır
   yalandı.
2. "`ip_hash_key` iki dosyada eşleşiyor" iki **boş dizenin**
   karşılaştırması olabiliyordu.
3. `ip_hash_key`'in yer tutucusu `[retention]` altındaydı; alan ise
   `privacy.ip_hash_key`.
4. Panelin `secret_key`'i aynı şekilde `[developer_gate]` altındaydı —
   **C7.3'te yazılan posta mekanizması hiçbir kurulumda çalışmamış.**
5. Rolleri önceden var olan bir makinede DSN'in veritabanı adı
   düzeltilmiyordu.
6. Okuma API'sinin jetonu hâlâ elle kopyalanıyordu.

3 ve 4'ü yakalamak için yazılan test, yazıldığı anda **üç tane daha**
buldu.

**Kurulan:** tek imaj + `init`, kalıcı birimde sırlar, `docker/
compose.yml`, ve `e2e/docker_test.go` — aynı zincir, konteynerden.

**Konteynerin tersine çevirdiği tek güvenlik kararı:** konteynerde
`127.0.0.1` o konteynerin kendisi, yani loopback bağlama servisi
korumaz, *kullanılamaz* yapar. Yerini compose ağı alıyor;
`release/ports_test.go` dosyayı okuyup doğruluyor.

#### K3 — Sözlük ✅ *(2026-08-27)*

`SOZLUK.md`: projede geçen her teknik terim, konuya göre on iki bölümde.
Her madde üç şey söylüyor — ne demek, bu projede nerede geçiyor, olmazsa
ne kırılır.

Bir sözlüğün arıza biçimi kırılmak değil **kaymaktır**: paket yeniden
adlandırılır, maddesi kusursuz okunmaya devam eder. O yüzden
`release/glossary_test.go` adı geçen paket, tablo, dosya ve
binary'lerin var olduğunu kontrol ediyor. Metnin *iyi* olup olmadığını
değil — onu bir test soramaz.

---

### L. Yükseltme yolu

**Sıraya girme gerekçesi kullanıcının:** *"müşteri yükseltemez,
güncellemeleri otomatik de yapamayız — o zaman manuel güncelleme
panelden tek tıkla."* Ve modeli veren analoji yine kullanıcının:
**Android işletim sistemi.** Ayarlar → Hakkında sürümü gösterir, bir
düğme güncellemeyi başlatır, ve işi Ayarlar uygulaması değil ayrı ve
yetkili bir bileşen (`update_engine`) yapar.

Analoji üç yerde tutuyor, üç yerde kırılıyor, ve **kırıldığı yerler
kısıtları veriyor**:

| tutuyor | kırılıyor |
|---|---|
| Sürüm Ayarlar'da görünür | Android veriyi değiştirmez, biz **şemayı** değiştiriyoruz |
| Tek düğme, terminal yok | Telefon yeniden başlar; **collector müşterinin sitesinin önünde** |
| Düğmeyi gösteren ≠ işi yapan | Bozulan telefon bir telefon; bozulan VDS **üç müşteri** |

#### Ölçüm: sürüklenme altında ne oluyor *(2026-08-30, gerçek TimescaleDB)*

Faz yazılmadan önce ölçüldü, çünkü restart'ın gerekli olup olmadığı
buna bağlıydı.

**Durum A — şema yeni, binary eski:**

```
yazılan=1  hata=<nil>  written=1  failed=0
```

Eski binary, tanımadığı sütunu olan tabloya sorunsuz yazıyor.
`CopyFrom` sütunları açıkça adlandırıyor ve her `ADD COLUMN`'un
varsayılanı var.

**Durum B — binary yeni, şema eski:**

```
NewWriter başarılı — Ping geçti, açılış SESSİZ
hata=column "asn_org" does not exist (SQLSTATE 42703)
written=0  failed=3   tabloda: 3'ün 0'ı
```

**Bu ölçümün verdiği üç karar:**

1. **Şema göçü restart istemiyor.** Durum A kanıtı. Şema yükseltilirken
   servisler çalışmaya devam eder, müşterinin sitesi kapanmaz.
2. **Sıra şema → binary.** Tersi Durum B'dir: süreç sağlıklı görünür,
   satırlar kaybolur.
3. **Açılış sessiz, ve olmamalı.** `Ping` veritabanının cevap verdiğini
   kanıtlar, şemanın uyduğunu değil.

Bundan iş ikiye bölünüyor, ve **güvenli yarısı restart'sız** — L
grubunun tamamı o yarı. Binary değişimi ve soket devri ayrı, sonra, ve
belki hiç.

---

#### L'nin iç bağları — biri diğerine yük bindirmesin diye

Üç faz sırayla yapılabilir ama **bağımsız değiller**. Dördü de ölçüldü,
ve ikisi L1'in L3'ün kararlarını önceden vermesini gerektiriyor. Bunlar
burada duruyor çünkü sonradan keşfedilirlerse L1 yeniden yazılır.

**Bağ 1 — `schema_version`'ı L1 yaratır, L3 yazar.**
L1 sadece okuyor, ama tabloyu güncelleyecek olan L3'ün uygulayıcısı.
Tablo L1'de "panelin okuduğu bir tablo" diye tasarlanırsa, L3 geldiğinde
sahiplik ve `GRANT`'ler değişir — yani `grants.sql`, `verify.sql` ve
`install.sh` üçü birden ikinci kez elden geçer. **L1, yazma yolunu
tabloyu yaratırken kararlaştırmak zorunda**, o yolu henüz kimse
kullanmayacak olsa bile.

**Bağ 2 — L1'in sayısı ile L2'nin kontrolü aynı soruyu iki yoldan
soruyor.** L1 "şema kaçıncı sürümde?" diye soruyor ve bir sayı alıyor;
L2 "ihtiyacım olan sütunlar duruyor mu?" diye soruyor ve gerçek yapıya
bakıyor. **Sayı yalan söyleyebilir** — biri göç etmeden sürümü
yükseltebilir; sütun kontrolü söyleyemez.

Bu yüzden ikisi iki ayrı doğruluk kaynağı olamaz: **L2'nin kontrolü
yaptırım, L1'in sayısı etiket**, ve *ikisinin uyuştuğunu doğrulayan bir
test* olmak zorunda. Yoksa bu projenin defalarca bulduğu arıza biçimi
geri gelir: sessizce ayrışan iki liste.

**Bağ 3 — DDL'i hangi rol çalıştıracak? Bugün cevap yok.**
Ölçüldü: şemayı `install.sh` **süperkullanıcıyla** uyguluyor
(`psql_db -f`, `SUPERUSER_DSN` üzerinden). Dört rolün hiçbirinde DDL
yetkisi yok, ve `grants.sql`'de tek bir `ALTER`/`CREATE`/`OWNER` geçmiyor.

L3'ün uygulayıcısı ya süperkullanıcı kimlik bilgisini diskte sürekli
taşıyacak — **kabul edilemez**, çalışan bir servis için süresiz
süperkullanıcı demek — ya da tabloların sahibi olan **beşinci bir rol**
gerekecek. İkincisi doğru cevap, ama `install.sh` + `grants.sql` +
`verify.sql` üçünü birden değiştiriyor, yani **F2 yüzeyi**. L1'de
kararlaştırılmazsa L3'te keşfedilir ve L1 yeniden yazılır.

**Bağ 4 — tavuk-yumurta: `schema_version`'ı ilk kez kim yaratacak?**
Göçleri takip eden tablonun kendisi bir göçle yaratılmalı, ama o göçü
çalıştıracak mekanizma henüz yok. **Yani L'nin kendisi bir kez elle
kurulmak zorunda:** geliştirici `install.sh`'ı bir kez çalıştırır,
sonrası panelden yürür.

Bu kaçınılmaz ve kusur değil — ama **yazılı olmazsa ilk müşteride
sürpriz olur**, ve o sürprizin adı "düğme çalışmıyor" olur.

---

#### L1 — Sürüm görünürlüğü ✅ **yapıldı** *(2026-08-30)*

Bugün **hiçbir şey veritabanının kaçıncı sürümde olduğunu bilmiyor.**
`schema_version` diye bir tablo yok; `internal/panel/migrate.go` ayar
göçü (TOML→DB), şema göçü değil. Bu yüzden hiçbir şey *"bu binary
buradaki şemadan yenisini istiyor"* diyemez.

Ekran, senin istediğin yer: **Ayarlar → Hakkında.** Binary sürümü, şema
sürümü, ve ikisi uyuşuyor mu. Salt okunur, risk yok, ve L2 ile L3'ün ön
şartı.

**Bitti ölçütü:** şema sürümü veritabanından okunuyor; uyuşmazlık
ekranda ayırt edilebiliyor; sürümü elle geri alınca ekran bunu söylüyor.

> **Yapıldı.** `internal/schemaver` iki değer tutuyor — sıralanabilir
> `Version` ve yalan söyleyemeyen `Fingerprint` — çünkü tek alan iki
> soruyu birden cevaplayamıyor. Tam sayıyı dürüst tutan şey diskten
> yeniden hashleyen ayna testi.
>
> **Evi ayar sayfası değil sağlık sayfası oldu:** orada zaten dört sürüm
> gösteriliyordu, ve L1'i D4a'ya bağlamak sırayı gereksiz uzatırdı.
> Kullanıcının istediği "ayarlarda sürüm" ihtiyacı bugün karşılanıyor;
> ayar sayfası geldiğinde oraya da bağlanır.
>
> **Bağ 1 çözüldü:** `panel_user` yalnız `SELECT` aldı; L3'ün
> uygulayıcısı için `GRANT UPDATE` bu satırın *yanına* eklenecek. Tablo
> sahipliği yaratılırken kararlaştırıldı, yani `grants.sql`/`verify.sql`/
> `install.sh` ikinci kez elden geçmeyecek.
>
> **Yedi mutasyonun yedisi yakalandı**, ve ikisi test tarafında gerçek
> boşluk buldu: veritabanı durumu mutasyonları Go test önbelleğine
> görünmüyor (`-count=1` şart), ve `Matches()` bozukken sağlık sayfası
> yeşil kalıyordu çünkü `healthSchema` o branch'a hiç uğramıyor. Durum
> makinesi kendi biriminde ayrıca test edildi.

#### L2 — Yazmadan önce dur ✅ **yapıldı** *(2026-08-30)*

Durum B'nin emniyet ağı, **ve müşteri düğmeye hiç basmasa bile
çalışıyor.** Yazıcının `columns` listesi `pg_attribute` ile
karşılaştırılır; uyuşmuyorsa süreç **yazmaya başlamadan** yüksek sesle
ölür.

Sessiz bir süreç artı kaybolan satırlar yerine, açılışta duran bir
servis. İkincisi onarılabilir bir arıza; birincisi fark edilmeyen bir
arıza.

**Bitti ölçütü:** eksik sütunla açılış reddediliyor ve hata sütunu
adıyla söylüyor; fazladan sütunla açılış **kabul ediliyor** (Durum A
bozulmamalı — yükseltme sırasının doğru olanı odur).

> **Yapıldı.** `schemaver.RequireColumns`, iki yazıcının `NewWriter`'ında
> `Ping`'den hemen sonra. L1'in deneyi birebir tekrarlandı: eksik sütunla
> açılış reddediliyor ve sütunu adıyla söylüyor; iki fazladan sütunla
> `yazılan=1 failed=0` — L1'deki Durum A ölçümüyle aynı sayı. Tarball
> E2E zinciri uçtan uca geçiyor.
>
> **Üç mutasyonun üçü yakalanıyor**, ama üçüncüsü ilk turda hayatta
> kaldı: `pg_catalog` yerine `information_schema` koyunca her test geçti,
> çünkü testler süperkullanıcı olarak koşuyordu. Bu oturumda üçüncü kez
> aynı ders. Ölçüm ayrıca yorumun abartılı olduğunu gösterdi — bugünkü
> çağrı noktalarında iki katalog aynı cevabı veriyor; fark **yanlış rolü
> gösteren DSN**'de ortaya çıkıyor, ve testi o durumu kuruyor.

#### L3 — Düğme: istek satırı + yetkili uygulayıcı ✅ **yapıldı** *(2026-08-31)*

Panel DDL çalıştıramaz, ve çalıştıramaması B6/H5'in tesis ettiği şeydir:

```
grants.sql → panel_user: panel_* tablolarında tam yetki
             analitik tablolarında hiçbir yetki
             dosyada tek bir ALTER / CREATE / OWNER yok
```

O yüzden düğme DDL çalıştırmaz, **istek yazar**:

1. Panel `panel_upgrade_requests`'e bir satır yazar *(panel\_ tablosu,
   yetkisi zaten var)*
2. Yetkili bir bileşen o satırı görür, şemayı uygular, sonucu geri yazar
3. Panel sonucu gösterir

DDL panel sürecinden **hiç** erişilebilir olmuyor. Docker'da bu
bileşenin yarısı zaten var — `init` konteyneri.

##### Kilit: yetkiyle değil, parolayla

**Varsayılan kilitsiz** — müşteri tek tıkla yükseltir, ek parola yok,
zaten girerken parolasını yazdı. "İşi bilmeyen normal müşteri de
yapabilmeli" şartı budur.

**Geliştirici isterse kilitler** — güncellemeye güvenmiyorsa ya da risk
görüyorsa. Kilit açıkken **yalnız geliştirici parolası** açar.

Yetenekle değil parolayla, çünkü `RoleOwner`'da `CapManageMembers` var
ve `RoleAdmin`'de `CapUseDeveloperMode` var: **müşteri kendine yetki
vererek yetenek kazanabilir.** Bakmakla biten şeyler için sorun değil;
geliştiriciye iş çıkarabilen şeyler için tek koruma parolanın
yapılandırma dosyasından gelmesidir. *(Bkz. SOZLUK.md §3, "Erişim:
yetkiyle değil, parolayla".)*

Sihirbaza **girmiyor** — gereksiz uzatma. Geliştirici seçeneklerinde
durur, sonradan istenirse açılır.

**Nüans, kullanıcının kendi kuralı:** geliştirici istediği yere parola
koyabilir, ama **geliştirici seçenekleri sayfasının kendisine
koyamaz** — o sayfa zaten geliştirici parolasının arkasında, ve kapıya
ikinci bir kilit takmak döngüsel olur. Kilitlenebilen şey sayfanın
kendisi değil, **içindeki tek tek işlemler**.

##### İnternete sorulmuyor

Panel "yeni sürüm çıkmış mı" diye hiçbir yere bağlanmaz. Yalnız iki
yerel sayıyı karşılaştırır: binary sürümü ve şema sürümü. Ölçtüğümüz
risk zaten tam olarak bu, ve her kurulumun bize bağlanması hem ürünün
duruşuna aykırı hem de F3 (filo) işi.

**Bitti ölçütü:** kilitsizken müşteri yükseltebiliyor; kilitliyken
yetkisi ne olursa olsun yükseltemiyor ve yalnız geliştirici parolası
açıyor; uygulama sırasında hiçbir servis durmuyor *(ölçülecek, iddia
edilmeyecek)*; başarısız göç geri bildiriliyor ve tekrar denenebiliyor.

##### Nasıl bitti

Yetkili bileşen `cmd/upgrader` oldu — altıncı binary, kendi sistem
hesabıyla, kendi yapılandırma dosyasıyla. systemd'de bir zamanlayıcının
koşturduğu tek atışlık birim: makine boştayken DDL yetkili açık bağlantı
tutan bir süreç yok, ve göç ortasında bir çökme "durumu bilinmeyen
servis" değil "düşmüş birim" oluyor.

`upgrader.toml` dağıtımdaki DDL koşabilen tek DSN'i taşıdığı için ikinci
bir hesap gerekti (`crucible-upgrader`): panel `crucible` olarak koşuyor
ve aynı hesap olsaydı, okuması bile yasak olan veritabanını yeniden yazan
kimliği okuyabilirdi.

**"Hiçbir servis durmuyor" ölçüldü.** Dört servis kendi rolüyle kendi
sorgusunu döngüde koşarken gerçek uygulayıcı gerçek şemayı uyguladı;
pencere içindeki en kötü sorgu 2.3–9.9ms, boştaki en kötü 5.0–83.5ms.
Yükseltme sırasında en kötü sorgu, hiçbir şey olmazken görülenden hızlı.
Sebep şema dosyalarında: her `CREATE` `IF NOT EXISTS`, yani yeniden
uygulama ağır kilit almıyor. Özellik SQL'e ait olduğu için teste
bağlandı — bir sonraki şema dosyası bir `ALTER` uzaklıkta.

İki eşik: 2sn mutlak tavan, artı aynı makinenin boş hâlinin 4 katı ve
250ms. İkisi de gerçek `ACCESS EXCLUSIVE` kilidiyle mutasyonla ölçüldü.

Yol üstünde ayrı bir kusur çıktı ve ayrı işlendi: `install.sh`
yapılandırma dosyalarını, kurduğu birimlerin koştuğu hesabın
okuyamayacağı izinlerle bırakıyordu — yani ürettiği kurulumda hiçbir
servis başlamıyordu. Betiğin tek testi `--no-systemd` geçtiği için o
aşamaya hiç girilmemişti.

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

### M. Veri kaynakları ve müşterinin kendi yenilemesi

*(2026-08-31'de eklendi. Kullanıcının isteği: "ASN/IP şeyleri için birden
fazla kaynak ekleyelim, nelerde neyi kullanacağını seçsin", "müşteri
panelden kendisi de yenileyebilsin", "orada logu gözükür — başarısız
kaynak var mı, ne kadar veri çekti, boyutu ne oldu".)*

#### Ölçülen başlangıç noktası

| | bugün |
|---|---|
| Sağlayıcı | **bir**, koda gömülü dört URL (hepsi aynı proje) |
| Kaçış kapısı | `local_csv_path` — kapalı ağ / ayna senaryosu |
| Yenileme | yalnız zamanlayıcı, **elle tetikleme yok** |
| Çekim kaydı | **yok** — hangi kaynak düştü, kaç satır, kaç bayt: hiçbiri kayıtlı değil |

#### Karar: kaynak kütüphanesi kodda, panel seçer

Panele serbest URL kutusu **konmayacak**, ve gerekçe tercih değil ölçüm:

**1. Kaynak URL değil, URL + ayrıştırıcıdır.** `internal/asnlookup/parse.go`
tek bir projenin tam üç sütunlu şekline yazılmış, gerçek indirilmiş
veriye karşı doğrulanarak. Panele konacak bir URL kutusu ancak *zaten
desteklenen biçimdeki* bir şeyi gösterebilir; yeni bir sağlayıcı yeni
ayrıştırıcı, o da yeni sürüm demektir. Yani kutu "yeni kaynak ekleme"
işini yapamaz — yapabileceğini sanmak yanıltıcıdır.

**2. Yapabileceği tek şeyin karşılığı zaten var.** "Aynı biçim, başka
sunucu" ayna demektir, ve o senaryo bugün `local_csv_path` ile
karşılanıyor.

**3. Dolayısıyla bedeli var, karşılığı yok.** Kutu, ya imkânsız olan ya
da zaten mevcut olan için bütün SSRF yüzeyini açardı — B3'ün kalıcı
yasak listesindeki *"parametresi ... bağlanılacak bir alan adı olan
herhangi bir operasyon"* maddesinin tarif ettiği durum.

**Tek doğruluk kaynağı kuralı:** ayarların enum seçenekleri kütüphaneden
`init()`'te üretilir. Kaynak eklemek tek yerde olur ve panel
kendiliğinden görür; ikinci bir liste yok, dolayısıyla sürüklenecek
ikinci liste de yok.

---

#### M1 — Kaynak kütüphanesi ve "neyi nereden"

**Ne:** `internal/asnlookup/sources.go`'da bir kayıt: kimlik, etiket, ne
sağladığı (ülke / ASN / ikisi), URL'ler, hangi ayrıştırıcı, lisans, ve
"bunu neden seçersin" notu. Üç yeni ayar — `sources.country`,
`sources.asn`, `sources.fallback_order` — seçenekleri kütüphaneden.

`local_csv_path` kütüphaneye birinci sınıf bir giriş olarak taşınır, yani
ayna senaryosu ayarlarda görünür hâle gelir.

**Dürüst uyarı:** bugün sağlayıcı bir tane. "Neyi nereden seç" ancak
seçilecek şey varsa anlamlı, o yüzden fazın gerçek işinin bir kısmı
kodda birkaç sağlayıcı daha eklemek. Aynı biçimi paylaşanlar mevcut
ayrıştırıcıyı kullanır; farklı biçim yeni ayrıştırıcı ister. **Hangi
sağlayıcıların gönderileceği — lisans, güncellik, kapsam — fazı yaparken
ölçülecek ayrı bir soru.** Şimdi isim saymak, doğrulanmamış bir iddiayı
plana yazmak olurdu.

**Bitti ölçütü:** hiç dokunmayan kurulum bugünküyle birebir aynı
davranıyor; seçim değiştirildiğinde bir sonraki yenileme yeni kaynaktan
çekiyor; kütüphaneye eklenen bir kaynak panelde ek bir değişiklik
olmadan görünüyor *(mutasyonla ölçülecek: kütüphaneye giriş ekle, panel
testinin onu gördüğünü doğrula)*.

---

#### M2 — Çekim kaydı

**Ne:** her yenileme denemesi bir satır: kaynak, başlangıç ve bitiş,
kaç satır ayrıştırıldı, kaç bayt indirildi, sonuç, ve başarısızlıkta tam
hata zinciri.

Kullanıcının istediği şey birebir bu: *"başarısız olan kaynak var mı, ne
kadar veri çekti, boyutu ne oldu"*.

**Şekli `panel_operations`'ın aynısı** — bir çekim, tanımı gereği bir
operasyondur — ama **çeken collector, panel değil**, yani rol ayrımı
farklı: collector yazar, panel okur. B1+B2'nin kurduğu bölünmenin aynısı,
ve aynı sebeple: yazan ile okuyan aynı rol olursa, kaydın kendisi
uydurulabilir hâle gelir.

**Tuzak, ve şimdiden yazılı olsun:** bu tablo da bir log tablosudur,
yani A4'ün disk sorunu üçüncü bir yoldan gelir. Kendi saklama süresi
olacak ve `internal/panel/housekeeping.go`'ya bağlanacak — o dosyanın var
olma sebebi tam olarak "yazılıp çağrılmayan süpürme".

**Bitti ölçütü:** başarısız bir çekim satırı hangi kaynağın düştüğünü ve
neden düştüğünü söylüyor; başarılı çekim satır ve bayt sayısı taşıyor;
tablo süpürülüyor ve süpürme çağrılıyor.

---

#### M3 — Elle yenileme düğmesi

**Ne:** müşteri panelden "şimdi yenile" diyebiliyor.

**L3'ün deseninin aynısı, ve bedava geliyor:** panel istek satırı yazar,
yetkisi olan bileşen — burada collector, çünkü çeken o — bir sonraki
turunda görür ve yapar. Panel dışarı hiçbir bağlantı açmaz.

**Bağlılık:** M2'den sonra. Düğme sonucu göstermeden yarım kalır, ve
sonucu gösterecek kayıt M2'de geliyor.

**Bitti ölçütü:** düğme kilitsiz varsayılanla müşteride çalışıyor; iki
kez basmak iki çekim başlatmıyor; sonuç ekranda görünüyor.

---

#### C8 — Geliştirici erişim politikası, müşterinin elinde

*(C grubuna ait ama burada duruyor çünkü aynı konuşmada karara bağlandı.)*

**Bugün:** politika örtük ve sabit — sahip yokken otomatik onay ("sorulacak
kimse yok"), sahip varken sahip onayı. Müşteri değiştiremiyor.

**Olacak:** müşteri seçer — *onay bekle* (varsayılan) / *doğrudan reddet*
/ *geçici olarak açık*. Ayrı bir sekmede, ayarların içinde.

**Neden müşterinin, kuralımıza rağmen:** "geliştiriciye iş çıkarabilen
şeyler parolanın arkasında" kuralı, müşterinin *kendine yetki verip iş
çıkarmasını* engellemek için. Burası tersi: müşteri kendini koruyor. Ve
gerçek bir kilitleme değil — kabuğu olan geliştirici zaten girer, kapanan
yalnız panel yolu.

**Şart:** karar denetim kaydına *ve* bağlantı sayfasına yazılır. "Neden
giremiyorum" sorusunun cevabı ekranda durmalı; yoksa kapatılmış bir
politika, kimsenin bağlantı kuramadığı bir arıza gibi görünür.

---

#### D4c — Ayarlar kabuğu: kategoriler ve açılır bölümler

**Ne:** bugün 27 ayar tek düz liste. Kategori kategori, açıldıkça açılan
bir yapı — kullanıcının tarifiyle "Android ayarları gibi".

**Neden sadece güzellik değil:** kullanıcının asıl amacı *"paneli
gereksiz doldurmamak için ayarlara atalım"*. Üst seviye sayfaları
ayarlara taşımak, ancak ayarlar o kalabalığı taşıyabildiğinde mantıklı
olur. Yani bu, kendisinden sonraki her şeyin ön şartı: M1'in üç ayarı,
C8'in sekmesi ve L3'ün düğmesi hep aynı sayfaya biniyor.

**Sıradaki ile bağı:** M1, C8 ve L3'ün yüzeyleri bu kabuğun içine
oturuyor. Kabuk önce gelmezse üçü de düz listeye eklenir ve sonra taşınır.

**Bitti ölçütü:** varsayılan görünüm kısa — açılmamış kategoriler; bir
kategori açıldığında içindekiler erişime göre filtreli; hiçbir ayar
kaybolmuyor *(kaynaktan türetilen bir test: kayıttaki her tanım bir
kategoriye ait, ve kategorisiz tanım kırmızı)*.

---

---

### Ertelenmiş kararlar

Karar verilmedi ve **şimdi verilmeyecek** — burada duruyorlar ki
kaybolmasınlar, gündeme girmesinler diye değil.

#### Konumlanma: "analitik" mi, "trafiğinin ne kadarı gerçek" mi

*(2026-08-31'de konuşuldu, bilerek ertelendi.)*

Aynı teknik iş iki farklı ürün olarak anlatılabilir:

- **Self-hosted analitik** — rakipler Plausible, Matomo, Fathom. Kalabalık
  pazar, zayıf farklılaşma.
- **Trafik kalitesi / bot ayrımı** — JA4 parmak izi, ASN kesişimi, sessiz
  IP'ler, JS-bot listeleri. Farklı alıcı, farklı fiyat.

D3 zaten ikincisinin altyapısını kurdu. Ama **odağı kaydırmak bir ürün
kararıdır ve ürün bitmeden verilmemeli** — kullanıcının kendi sözleriyle:
*"iyice tamamlandıktan sonraya bırakalım, şimdi düşünmeyelim."*

Buraya yazılmasının tek sebebi, o gün geldiğinde sıfırdan
hatırlanmaması. Plandaki hiçbir fazın sırasını ya da kapsamını
değiştirmiyor.

#### Lisans modeli

Aynı konuşmada: Apache-2.0 mı kalır, yoksa kaynağı görünür + ticari
lisans (BSL tarzı) mı olur. Karar verilmedi.

**Geri dönülemez olan tek şey yayınlamaktır**, o yüzden karar
verilene kadar sürüm etiketi atılmıyor. Bunun dışında teknik iş iki
modelde de birebir aynı, yani karar geciktirmenin bedeli yok.

Bugün yapılan tek şey **geri dönülemez olanı önlemek**: `CLA.md`.
Dışarıdan gelen ilk katkı Apache-2.0 altında kabul edildiği an, o kişinin
kodu izni olmadan yeniden lisanslanamaz — yani seçenek o gün kapanır.
Sözleşme bugün bedava, sonra imkânsız.

*`CLA.md` Apache ICLA v2.0'dan uyarlandı ve **avukat incelemesinden
geçmedi**. Ticari yola gerçekten girilirse metin bir hukukçuya
okutulmalı; şu anki hâli seçeneği açık tutmaya yeter, dayanak olmaya
değil.*

---

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
adımı, sahiplenme bağlantısının tüketilmesi — hepsi tek ifade, ve
eşzamanlı testlerle tam olarak bir kazananın olduğu doğrulandı.

### 3.4 Denetim: listeye karşı bakılır, hafızaya karşı değil ⚠️ **AI.3**

Denetimin tamamı `SECURITY.md`'de. Buraya yalnız **kalıcı kural**
düşenler yazılıyor:

- **Bağımlılıklar okumayla denetlenmez.** `govulncheck` her yayından
  önce çalışır. AI.3'ün en yüksek önemli iki bulgusu bağımlılıktaydı ve
  ikisi de *erişilebilirdi*; hiçbir kod incelemesi ikisini de bulamazdı.
  Biri (`pgx`'te yer tutucu karışması) bu belgedeki §3.1 kuralını **bir
  katman altından** deliyordu.
- **Her istek gövdesinin bir tavanı vardır.** Go'nun `ParseForm`'u
  urlencoded gövdeyi sınırsız okur; sınır middleware'de, handler'da
  değil. "Her handler hatırlar" burada bozulan özelliktir.
- **Hata mesajı bir çıktıdır ve çıktının bir muhatabı vardır.** İnsana
  yazılmış doğrulama mesajı ile sarmalanmış sürücü hatası aynı dönüş
  değerinden geliyorsa, ayrımı **sentinel** yapar — konvansiyon değil.
  Konvansiyon, her gelecek çağrı yerinin doğru tahmin etmesi demektir.
- **Yorum, derleyicinin işini yapmaz.** Interpolasyonla SQL'e giren tek
  tanımlayıcı kapalı bir tiptir; kontrolü atlayan yeni bir değer
  derlenmez.
- **Bir test yanlış nedenle geçebilir.** AI.3'te ikisi geçti: biri kendi
  ayırdığı belleği ölçtü, biri de bir *reddi* başarı saydı. Yazarken
  sorulacak soru "kod bozuk olsa bu iddia ne derdi".

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
- **Binary yükseltme** — *(L grubu bu maddeyi böldü: **şema**
  yükseltmesi L3 ile panelden yapılabilir hâle geliyor ve kesinti
  istemiyor; listede kalan **binary** değişimi, çünkü çalışan bir
  sürecin kodu yerinde değiştirilemez)*
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

**A7 + A7.5 fazında kapatıldı:**

| Risk | Nasıl |
|---|---|
| IP tam saklanıyordu, karar beklemedeydi | Hukukçu cevabı geldi: **varsayılan maskeli.** Ayar değil, varsayılan önemliydi — okunmayan ayar üretime giden ayardır, o yüzden her geri düşüş noktası (boş config anahtarı, bozuk ayar satırı, doldurulmamış struct alanı) maskeliye düşüyor |
| Maskeleme coğrafyayı ve ziyaretçi sayımını sessizce bozabilirdi | Sıralama teste bağlandı: çözümleyiciye **hangi adresin sorulduğu** doğrulanıyor, yalnız çıktı değil. İki ziyaretçi aynı /24'te tek satıra düşüyor ama **iki ayrı ziyaretçi kimliği** üretiyor |
| İki yazıcı farklı maskeleyebilirdi | Tek paket (`internal/privacy`). Kesişim birleşiminin karşılaştırdığı iki sütunu yazan taraflar aynı fonksiyonu çağırıyor; bir bitlik anlaşmazlık o birleşimi **hatasız biçimde boş** döndürürdü |
| Hukuki ağırlıklı ayarları panele giren herkes değiştirebiliyordu | Ayrı şifre, yapılandırma dosyasından, hash'li, **her seferinde**. Kural hatırlamaya değil derlemeye bağlı: `SetSetting` korumalı anahtarı reddediyor, yetkinin geçerlilik alanı dışa kapalı |
| Sıfırlama kapıyı atlatabilirdi | `ResetSetting` de korumalı. `campaign.drop_params` varsayılanı boş liste — "varsayılana dön" demek "utm_term'i saklamaya başla" demek |
| Kapının kendisi 19 MiB'lık DoS aracı olabilirdi | Doğrulamalar seri, kuyruk sınırlı, ardışık hatalar pencereli sayaçla durduruluyor |
| Yanlış yazılmış hash sonsuza kadar "yanlış şifre" gibi görünürdü | Açılışta reddediliyor. Düz metin şifre alanı da: yok sayılmıyor, **hata veriyor** |

**Önceki fazlarda kapatılanlar:**

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
| ~~Collector tarafı canlı ayarlar (geo, skor eşiği, flush)~~ | ✅ **A5.2'de kapandı** | Geo listeleri, bot ASN listesi ve `apply_to_scoring` canlı; flush aralığı bilerek dışarıda (performans ayarı, destek çağrısının ihtiyacı değil). **"Kalanı mekanik" tahmini yanlıştı** — alanlar okununca ikiye ayrıldılar ve biri sessizce uygulanmıyordu |
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

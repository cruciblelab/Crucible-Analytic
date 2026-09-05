# Değişiklik günlüğü

Sürüm şeması ve ne zaman sürüm çıkarıldığı: `VERSIONING.md`.

Her girdi üç şeyi söyler: ne değişti, şema sürümü kaç, ve **kuran
kişinin bir şey yapması gerekiyor mu**. Üçüncüsü en önemlisi ve genelde
yazılmayanı: bir sürüm notunu okuyan kişinin asıl sorusu "ben ne
yapacağım".

---

## Yayımlanmamış

Etiketlenmemiş çalışma. Bir sonraki sürüm bunu taşıyacak.

**Şema sürümü: 11.** v0.21.0 ile aynı, yani o sürümü kurup şemayı
yükselttiyseniz burada **yapmanız gereken bir şey yok.**

### Yedek düğmesi: Sağlık → Yedek

Yedek alma yolunun ikinci yarısı. Artık uçtan uca çalışıyor: **Sağlık →
Yedek**, neyin dahil olacağını seçin, düğmeye basın. Yükseltici otuz
saniye içinde alıyor ve bölüm kendini yeniliyor.

**Kim basabilir:** sitenin yöneticisi. Sürüm güncellemesinin aksine
geliştirici parolası istemiyor, ve fark iki düğmenin başkasına ne
yapabildiğinde: sürüm güncellemesi sitenizin önündeki programı
değiştirir, yedek ise bir dosya yazar. Yapabileceği tek zarar diski
doldurmak, o da tek bayt yazılmadan sayılarla reddediliyor.

**Sığmıyorsa hiç başlamıyor.** Yükseltici tabloların gerçek boyutunu
ölçüyor, dosyanın ne kadar tutacağını kötümser bir oranla tahmin ediyor,
ve diskte kalacak payı hesaba katıyor. Yetmiyorsa satıra ne kadar eksik
olduğu yazılıyor. Dolan disk collector'ı durdurur, collector da sitenin
önündedir — yani bu özelliğin tek başına yaratabileceği kesinti, tam da
çalışırken yarattığı olurdu.

**Yedekler listede,** tarih, içerik, boyut ve o sırada çalışan sürümle.
Toplam da yazıyor. Biri kabuktan bir yedeği silerse satır kaldırılmıyor,
"dosya yok" diye işaretleniyor: "burada bir yedek vardı ve gitti"
okunması gereken bir cümle.

**Ve listenin altında bir uyarı var:** bir yedek, saklama politikanızın o
tarihten sonra sildiği veriyi tutar. Sildiğinizi sandığınız satırlar
orada duruyor olabilir.

`upgrader.toml` içine `[backup] dir` yazılmadıkça bu kurulum yedek
almaz. Düğmeye basılırsa istek satırına "yedek dizini yapılandırılmamış"
yazılıp bitiriliyor — sessizce beklemiyor.

### İç yapı: her bölüm yalnız kullandığı veritabanı metotlarını görüyor

**Davranış değişmedi, yapmanız gereken bir şey yok.** Panelin sağlık
sayfasındaki beş bölüm — disk, yedek, sürüm, şema, IP kaynakları — artık
veritabanının tamamına değil, yalnız kendi kullandığı üç-beş metoda
erişiyor. Derleyici gerisini reddediyor.

Görünür tek etkisi ileriye dönük: bu bölümlerin mantığı artık
veritabanı olmadan sınanabiliyor, yani "okunamayan bir boyut sıfır bayt
olarak gösterilmesin" gibi kurallar gerçek bir arıza kurmadan test
edilebiliyor.

## v0.21.0 — 2026-09-04

Panelden **güncelleme** yolunun tamamı: imzalı sürüm paketi, istek
kuyruğu, indirme ve doğrulama, kurulum, geri dönüş ve isteğe bağlı
otomatik yeniden başlatma. Yanına disk görünümü ve yedeklemenin ilk
dilimi.

**Şema sürümü: 11.** **Kuran kişinin yapması gereken:** panelde
**Sağlık → Şema yükseltmesi**. Dört tablo ekleniyor ve üç tablonun satır
güvenliği sıkılaştırılıyor; veri değişmiyor, servis durmuyor.

Yükseltmeden önce her şey çalışmaya devam eder. **Ama satır güvenliği
düzeltmesi yükseltilene kadar uygulanmaz**, ve aşağıda ne olduğu
yazıyor.

### Yedekleme: ilk dilim, ve pg_dump'ın sessizce boş dosya üretmesi

Yedek alma yolunun ilk yarısı: şema, küme tanımları, ve **dosyayı
üreten** taraf. Panelde henüz düğme yok, sonraki dilimde geliyor.

**pg_dump kullanılmıyor, ve sebebi ölçüldü.** Bariz yol
`pg_dump --table=traffic_snapshots`. Dosya üretiyor, dosya geri
yükleniyor, ve içinde **sıfır satır** var — çünkü bir hypertable'ın
satırları o isimde değil, chunk'larda duruyor ve `--table` süzgeci
chunk'ları takip etmiyor. Gerçek veriyle ölçüldü:

| Ne | Değer |
|---|---|
| traffic_snapshots'taki satır | 8.050 |
| --table dökümünün boyutu | 3.957 bayt |
| Geri yükleyince gelen satır | 0 |

Hata yok, uyarı yok, makul boyutta bir dosya, ve içi boş. Bir yedek
özelliğinin alabileceği en kötü şekil bu: yalnız birinin ona ihtiyacı
olduğu anda başarısız oluyor.

Onun yerine veri **COPY** ile çıkıyor, tablo tablo. COPY sıradan bir
sorgu olduğu için hypertable chunk'larıyla cevap veriyor. Aynı veri geri
yüklendiğinde 8.050 satır ve 6 chunk oluştu.

**Şema dosyanın içinde değil.** Zaten binary'nin içinde: geri yükleme
tabloları kurulumun kullandığı aynı baytlardan kuruyor. Dosyanın kendi
DDL'ini taşıması, şemanın ikinci bir tanımı olurdu.

Dosya `tar.gz`, standart kütüphaneyle. Yani yükselticinin
`postgresql-client`'a veya başka hiçbir harici programa ihtiyacı yok.

**Korumalar dosyanın ilk yazıldığı anda var:** mod 0600, dizin 0700,
önce geçici ada yazılıp fsync'ten sonra yeniden adlandırılıyor. Yarıda
kesilen bir yedek **asla** son adını almıyor — böyle bir dosya
açılabilir, boyutu makuldür ve son tablosu eksiktir.

**Kuyruk tablosunda yol sütunu yok, katalogda var ama panele
verilmemiş.** Sütun düzeyinde GRANT: `SELECT path FROM panel_backups`
panelin rolü için veritabanı tarafından reddediliyor. Panel boyutları,
tarihleri ve içerikleri görüyor; baytların nerede olduğunu göremiyor.

**Şema sürümü 11'e çıktı.** İki tablo daha ekleniyor.

### Depolama bölümü hypertable'ları 400 kat küçük gösteriyormuş

`traffic_snapshots` için **40 KB** yazıyordu. Gerçek boyutu **16 MB**.
`beacon_events` için 48 KB yazıyordu, gerçeği 18 MB.

Sebebi: bir hypertable'ın satırları ana tabloda durmaz, chunk'larda
durur. `pg_total_relation_size` ana tabloyu ölçer, ve bir hypertable'ın
ana tablosu her zaman neredeyse boştur — kaç satır girerse girsin.

Yani sayfanın raporladığı iki tablo, yani saklama politikasının ilgili
olduğu iki tablo, yani büyüyen iki tablo, gerçek boyutlarının binde
dördüyle gösteriliyordu. Hiçbir şey hata vermedi: sayı oradaydı, küçüktü,
ve depolama sayfasında küçük sayılar hata gibi görünmez.

B4'ten beri böyleydi. Yedek tahminleri için sıkıştırma oranı ölçülürken
bulundu: bir tablonun **sıkıştırılmış dökümü**, tablonun kendisinden
büyük çıktı — insanı baktıran türden bir imkânsızlık.

Artık hypertable'lar `hypertable_detailed_size` ile ölçülüyor, sıradan
tablolar eskisi gibi. Ve bir test yirmi bin satır yazıp sayının
kıpırdadığını kontrol ediyor; eski sorguda hiç kıpırdamıyordu.

**Diskiniz sandığınızdan doluysa panelde bir şey değişmedi, sadece doğru
sayı yazmaya başladı.**

### Sağlık → Disk

Bu üründe hiçbir sayfa **diski** göstermiyordu. Depolama bölümü dört
tablonun boyutunu yazıyor, hepsi veritabanından geliyor, ve "bu tablo
4,2 GB" cümlesi bir sonraki yazmanın yer bulup bulamayacağını söylemiyor.

Artık ayrı bir **Disk** bölümü var: yapılandırdığınız her dizinin
bulunduğu dosya sistemi, toplam / dolu / kullanılabilir, ve bir çubuk.
Aynı diskteki iki dizin tek satırda birleşiyor — iki kez yazılsalardı
toplanmaya davet olurdu, ki o da gerçeğin tam iki katı.

**Kullanılabilir alan, "boş" alan değil.** Dosya sistemi bir kısmını
yalnız root'a ayırır; bu ürünün hiçbir servisi root olarak çalışmaz. Bu
makinede ölçüldü: çekirdek 222 GB boş diyor, gerçekten yazılabilir olan
7 GB. Çubuk da ayrılmış payı ayrı çiziyor, çünkü çubuğun boş kısmı
"kalan yer" diye okunur ve o okuma yanlış olurdu.

**Konteynerde bir uyarı var:** dizin bir volume üzerinde değilse, oraya
yazılan her şey bir sonraki imaj güncellemesinde silinir. Uyarı yalnız
ikisi birden doğruyken çıkıyor; sıradan bir sunucuda `/var/lib`'in kök
dosya sisteminde olması normaldir ve hakkında hiçbir şey denmiyor.

**Veritabanının diski görünmüyor, ve bu yazıyor.** Panel veritabanının
boyutunu okuyabiliyor ama veri dizinini soramıyor: o, panelin rolünde
bilerek bulunmayan bir yetki ister. Sayfa tahmin etmek yerine
söylüyor — veritabanı bu makinedeyse "büyük ihtimalle yukarıdakilerden
biri, hangisi olduğunu göremiyorum", başka makinedeyse "buradaki
sayıların onunla ilgisi yok".

Kurulum kontrollerindeki disk ölçümü de artık aynı koda bakıyor. İki
kopyalı bir ölçüm ayrışır, ve hep bakılmayan kopya yanlış kalır.

### Kırılım tablolarında oran çubuğu

Her kırılım satırının yüzdesinin yanında artık bir **oran çubuğu** var.
Ölçek mutlak: tam genişlik trafiğin tamamı demek, "buradaki en büyük
satır" değil — çubuk hücrenin içindeki sayıyla aynı şeyi söylüyor.

Payı bilinmeyen satırda çubuk **hiç çizilmiyor**. Sıfır genişlikte bir
çubuk "%0" demektir, ve o "bilinmiyor"dan farklı bir iddiadır.

Dar ekranlarda çubuk gizleniyor, sayı kalıyor.

### Sürüm güncellemesi artık gerçekten yapılıyor

Düğmeye basmak bir istek satırı yazıyordu ve **o satırı hiçbir süreç
okumuyordu**: sayfa "Sırada" diyor ve öyle kalıyordu. Yükseltici artık
sırayı da boşaltıyor — paketi indiriyor, imzasını doğruluyor, kuruyor,
çalışmazsa eskisini geri koyuyor, ve sonucu satıra yazıyor.

Sırası önemli: **önce binary, sonra şema.** Yeni bir binary yeni bir şema
bekliyorsa başlamayı reddeder, yani gürültülü durur; tersi sessizdir.

**Yeniden başlatma sizde** — ta ki aşağıdaki *Yeniden başlatma ve
otomatik geri dönüş* bölümünü açana kadar. Dosyalar değişse de çalışan
süreçler eski binary'yi çalıştırmaya devam eder. Kurulum bittikten
sonra:

```
sudo systemctl restart crucible-collector crucible-beacon \
    crucible-analytics-api crucible-panel
```

Servisleri yeniden başlatabilen bir sürecin onları durdurabilen bir süreç
olması sebebiyle bu yetki yükselticiye bilerek verilmedi.

`upgrader.toml` içinde `[release] prefix` ile kurulum kökü
değiştirilebilir; varsayılan `/opt/crucible-analytic`.

### Güncellemeleri kontrol et

Sağlık → Sürüm güncellemesi artık **en yeni sürümü de yazıyor**, ve yeni
bir sürüm varsa kurulacak sürüm alanını kendisi dolduruyor. Sürüm
numarasını ezbere bilmeniz gerekmiyor.

Soruyu **yükseltici** soruyor, panel değil: adres ve imza anahtarı onun
yapılandırmasında, ve gösterilen sürüm panelin değil imzanın sözü.
Altı saatte bir bakılıyor; başarısız olursa on beş dakikada bir yeniden
deneniyor.

Dört ayrı durum var ve dördü ayrı cümle: hiç bakılmadı, günceliz, yeni
sürüm var, ulaşılamadı. Son bakışı başarısız olan bir kuruluma asla
"güncelsiniz" denmiyor — bilinen son cevap, tarihiyle birlikte yazılıyor.

**Yayıncı için:** `release/manifest.sh v0.21.0 [notlar-adresi]`, paketler
yüklendikten sonra, aynı imza anahtarıyla. Ürettiği iki dosya sürüm
adresinin köküne konur.

### Yeniden başlatma ve otomatik geri dönüş

**İsteğe bağlı, ve açmak sizin kararınız.** Açmazsanız hiçbir şey
değişmez: güncelleme dosyaları değiştirir, panel "yeniden başlatın"
der, siz başlatırsınız.

Açarsanız güncelleme kendini tamamlar. Yükseltici `/run/crucible-analytic`
içine **boş bir dosya** koyar; bir systemd birimi bunu görüp dört
servisi yeniden başlatır.

Yükselticiye `systemctl` yetkisi **verilmedi**, ve bu tasarımın
tamamıdır. Dosyanın içi hiç okunmuyor: hangi birim, hangi yol, hangi
sürüm — hiçbiri yazmıyor. Bir zil, bir emir değil. Yani ağdan paket
indiren o programı ele geçiren biri, ancak bu yeniden başlatmayı
yaptırabilir; makineye erişimi olan herkesin zaten yapabildiği şeyi.

*Bir isteğin taşıdığı her alan, onu yazana verilmiş bir yetkidir.*

**Kaçış mekanizması.** Yeni sürüm geri gelmezse eskisi otomatik geri
konur. "Geri geldi"nin ölçüsü sürecin ayakta olması değil — veritabanına
**yeniden başlatmadan sonra** kalp atışı yazmış olmasıdır. Bağlanamayan
bir servis bu sınavı geçemez; ayakta durup hiçbir şey yapmayan bir
servis de.

Dördü de otuz saniye içinde yazmazsa yükseltici önceki binary'leri geri
koyar, tekrar başlatır, tekrar bakar ve satıra ne olduğunu yazar. Hepsi
yazarsa kontrol noktası silinir — o silme, tek "kanıt yok ediliyor"
anıdır ve yalnızca servisler konuştuktan sonra olur.

**Limitler.** systemd birimi beş dakikada üç başlatmayla sınırlı. Bir
servisin çöküp yeniden başlatma isteyip yine çöktüğü bir döngü, aksi
hâlde müşterinin sitesini sonsuza kadar yeniden başlatırdı; üçüncüden
sonra systemd reddediyor ve arıza görünür kalıyor. Betikte `stop`,
`disable`, `mask` yok: hiçbir yol servisi kapalı bırakmıyor.

**Açmak için** (üçü de gerekli, bu sırayla):

```
sudo install -m 0644 /opt/crucible-analytic/tmpfiles/crucible-analytic.conf \
     /etc/tmpfiles.d/
sudo systemd-tmpfiles --create /etc/tmpfiles.d/crucible-analytic.conf
sudo systemctl enable --now crucible-restart.path
```

İlk satır `/run/crucible-analytic` dizinini oluşturur. Yükseltici "bir
yeniden başlatıcı dinliyor mu" sorusunu **o dizinin varlığıyla**
cevaplar, yani onsuz birim çalışır, doğrudur ve hiçbir zaman tetiklenmez.

**Yapamadığı şey:** yeni binary makinenin ağını veya diskini bozarsa
buradaki hiçbir şey yardım edemez. Geri dönüş veritabanına yazabilen bir
makine varsayar. O durumda iş yine kuran kişide.

### Panelden sürüm güncellemesi: düğme

**Sağlık → Sürüm güncellemesi.** Çalışan sürüm yazıyor, kurulacak sürümü
yazıp gönderiyorsunuz; yükseltici otuz saniye içinde alıyor ve bölüm
kendini yenileyerek sonucu gösteriyor.

**Varsayılan kilitli.** Şema yükseltmesinin aksine — ve bilerek: şema
yükseltmesi çalışan bir veritabanına tablo ekler, sürüm güncellemesi
sitenizin önündeki programı değiştirir. Geliştirici, `Sürüm
güncellemesini geliştirici parolasına kilitle` ayarını kapatarak müşteriye
açabilir.

Panel hangi sürümlerin yayımlandığını bilmez ve bilmemelidir: adres ve
imza anahtarı yükseltici programın yapılandırma dosyasındadır,
veritabanında değil.

### Kilitli şema yükseltmesi doğru parolayı da reddediyormuş

Sağlık sayfasındaki parola alanı, kapının okuduğundan **başka bir ad**
gönderiyordu. Yani kilitli bir kurulumda şema yükseltmesi hiçbir zaman
açılamıyordu: doğru parolayı yazana sayfa "parolayı yazarak
başlatabilirsiniz" diyordu.

Dört şablonda tek isim var artık, ve bir değişmez her parola alanının
kapının okuduğu adı taşımasını kontrol ediyor.

### Panoda zaman grafiği

Pano artık dönem boyunca hareketi **çiziyor**: sayfa görüntüleme ve
ziyaretçi, kartların üstünde, seçilen döneme göre saatlik / altı saatlik
/ günlük kovalarla.

Sunucuda çizilen düz SVG. Grafik kütüphanesi yok, CDN yok, ek bir istek
yok — panelin "tarayıcının indirdiği her şey binary'nin içindedir"
kuralı grafikte de geçerli. JavaScript kapalıyken de çiziliyor.

Üç şey bilerek şöyle:

- **Boş kova, boş zaman değil.** Analitik servisi satırı olmayan
  kovaları hiç göndermiyor. Grafik aralığı kendisi dolduruyor, yani
  sessiz bir gece çizgiyi tabana indiriyor — atlanıp yokmuş gibi
  yapılmıyor.
- **Gelecek çizilmiyor.** Dönem tam yerel gün olduğu için "son yedi
  gün" bu gecenin saatlerini de kapsıyor; henüz olmamış kovalar
  çizilmiyor. Devam eden kova kalıyor: o gerçek ölçüm.
- **Kapatılabiliyor.** Grafik, çizdiği kartlardan (Ziyaretçi / Sayfa
  görüntüleme) en az biri açıkken çiziliyor. İkisi de kapalıysa
  çizilmiyor **ve sorgusu hiç koşmuyor.**

### Panoda "Son «»7 gün" yazıyordu

Müşterinin panosundaki dönem düğmeleri, panonun yazıldığı ilk günden beri
`Son «»1 gün` / `Son «»7 gün` diye okunuyordu. Sebep tek satır: dönem
etiketi kurulurken **boş bir mesaj anahtarı** aranıyordu, ve hiçbir dil
paketi boş anahtarı tanımlamadığı için "eksik mesaj" işareti basılıyordu.

İşaret tam da fark edilsin diye var. Ama boş anahtarda `«»`'ye çöküyor,
ve bir sayının yanındaki iki tırnak noktalama gibi okunuyor: on altı gün,
iki ekran görüntüsü turu ve bütün test takımı boyunca kimse kusur olarak
görmedi.

Üç şey değişti. Satır düzeltildi. Boş anahtar artık kendi adını basıyor
(`«anahtarsiz-cagri»`). Ve işarete bir **okuyucu** eklendi: müşterinin ve
geliştiricinin ulaştığı her sayfa gerçek veritabanıyla yükleniyor ve
işaret aranıyor; yönlendiriciyi okuyan bir ayna testi de yeni bir
sayfanın sessizce atlanmasını engelliyor.

### Üç kuyrukta rol ayrımı aslında uygulanmıyormuş

**Bu bir güvenlik düzeltmesi ve testin kendisi bulmadı — testi
sıkılaştırınca bulundu.**

Üç istek kuyruğu (şema yükseltmesi, IP veri kümesi yenilemesi, ve yeni
sürüm güncellemesi) aynı ayrımı iddia ediyor: **bir rol ister, başka bir
rol cevaplar.** Panel isteği yazar ve sonucu uyduramaz.

Ölçüldü: uygulayıcı rolüyle bağlanıp düz bir `INSERT` çalıştırmak
**üçünde de başarılı oluyordu.** Sebep: bütün tablolar `schema_admin`'e
ait, ve PostgreSQL bir tablonun sahibini satır güvenliğinden muaf
tutuyor — tabloya `FORCE` denmedikçe. Yani politikalar ve GRANT'lar,
tabloyu sahiplenen tek rol hakkında hiçbir şey söylemiyordu.

Bunu yakalaması gereken test **geçiyordu, ve yanlış sebeple**: uçuşta bir
istek varken ekleme yapıyordu, tekil indeks reddediyordu, ve iddia
yalnız "bir şey reddetti" diye bakıyordu. Test artık **hangi şeyin**
reddettiğini kontrol ediyor, ve üç tabloda da `FORCE ROW LEVEL SECURITY`
var.

*Bir testin geçmesi, test ettiğini sandığınız şeyin doğru olduğu
anlamına gelmiyor.*

### Panelden güncelleme: istek kuyruğu

`panel_release_requests` tablosu eklendi. Panel yüzeyi henüz yok; bu
altyapı.

Kuyruk **adres taşımıyor, sürüm taşıyor.** Paketlerin nereden geldiği
`upgrader.toml`'daki `[release] base_url`, ve orada kalmalı: tablodaki
bir adres, ele geçirilmiş bir panelin seçebileceği bir adres olurdu.
Sürüm dizgisi de sıkı kontrol ediliyor, çünkü bir URL'nin parçası
oluyor — eğik çizgi taşıyabilen bir sürüm, yol taşıyabilen bir sürümdür.

### Sürüm paketleri artık imzalanabiliyor

**Panelden güncelleme yolunun ilk parçası.** Müşteri paneldeki bir
düğmeyle güncelleme yapabilsin istiyoruz; onun önündeki engel buydu.

`SHA256SUMS` derleme sırasında üretiliyor ve paketin **içinde** geliyor.
Yani dosyanın bozulmadığını kanıtlıyor, **kimin ürettiğini
kanıtlamıyor** — paketi verebilen herkes yanına uyan bir liste de
verebilir. İnsan bir arşivi açmayı seçerken bu katlanılır; panel
güncelleme isteyebildiği anda değil.

Artık Ed25519 imzası var:

```bash
go run ./cmd/releasesign -keygen            # iki yarıyı basar
CA_RELEASE_KEY=... ./release/build.sh       # imzalı paket üretir
CA_RELEASE_PUBKEY=... ./release/verify.sh dist/...   # kontrol eder
```

**Açık anahtar `upgrader.toml`'a gidiyor, veritabanına değil.** O dosyayı
servislerin çalıştığı `crucible` hesabı okuyamıyor. Yani ele geçirilmiş
bir panel güncelleme *isteyebilir*, ama ne kurulacağını *etkileyemez*.

`verify.sh` üç durumu ayırıyor: imzasız, imzalı ama kontrol edilmedi,
imzalı ve doğrulandı. **Kontrol edilmemiş bir imzayı başarı diye
raporlamak, imzayı hiç görmemekten kötü.**

Anahtarsız derleme hâlâ çalışıyor — baytların tekrar ürediğini kontrol
eden herkes için gerekli — ama paketin imzasız olduğunu yüksek sesle
söylüyor.

### `install.sh` artık binary'leri de kuruyor

**Bu bir düzeltme, ve etkisi total.** Beş systemd biriminin hepsi
`/opt/crucible-analytic/bin/<ad>` çalıştırıyor. Betik rolleri,
veritabanını, şemayı, yapılandırmayı, servis hesaplarını ve birimlerin
kendisini kuruyordu — ve o dizine hiçbir şey koymuyordu.

Yani kılavuzu harfiyen izleyen biri `systemctl enable --now` diyor ve
dört servisten de `status=203/EXEC` alıyordu. Tek ipucu, okuması
söylenmemiş bir birim dosyasının içindeki yol.

Dosya yanına yazılıp **taşınıyor** (`mv`), çünkü Linux çalışan bir
çalıştırılabilir dosyaya yazmayı reddeder ve asıl önemli koşu ikincisi:
yeni sürüme geçen koşu. Servisler yeniden başlatılmıyor — binary'yi
değiştirmek ile onu yeniden başlatmak iki ayrı karar.

`--bin-dir` de eklendi: başka makinede derleyip kopyalayanlar için.

### Yapılandırma dizini tek bir yazıma indirildi

`/etc/crucible` ve `/etc/crucible-analytic` yan yana yaşıyordu.
**Kurulum kılavuzunun 6. bölümü dosyaları hiçbir servisin okumadığı bir
dizine kopyalatıyordu**; 7. bölüm, yedi bölüm sonra, doğru yazımı
kullanıyordu. Kurulum öneki de ikiye ayrılmıştı, ve kılavuzun haftalık
bot verisi cron satırı var olmayan bir binary'yi gösteriyordu — haftada
bir sessizce başarısız olan bir iş.

Dördü de tek isme indirildi ve dizin ailesi testine eklendi.

### KURULUM.md §13.5 — "Yeni sürüme geçme"

Kılavuz sıfırdan kurmayı ve şemayı yükseltmeyi anlatıyor, ikisinin
arasındaki adımı anlatmıyordu. Yeni bölüm neyin **kaybolmadığını** tablo
hâlinde söylüyor (yapılandırma, veritabanı, sırlar, kullanıcılar — hiçbiri
ellenmiyor), ve hiçbir şeyin GitHub'dan indirilmediğini yazıyor.

**İki kurulum yolunun ikisi de var:**

| yol | güncelleme |
|---|---|
| Konteyner | Yeni imaj, `docker/.env` içinde `CA_IMAGE`, `compose up -d`. Üç kalıcı birim dokunulmadan kalır |
| Elle | Paketi doğrula, `install.sh`'ı tekrar koştur, servisleri yeniden başlat, sürümü doğrula |

Sonrasında ikisinde de aynı: panelde **Sağlık → Şema yükseltmesi**, ve
şema değişmediyse düğme "yapacak bir şey yok" der.

### §16 "Bilinen eksikler" denetlendi

Dört maddesi çoktan yapılmış işleri eksik diye anlatıyordu. Var olmayan
bir eksiği anlatan bir belge, okuyana gerçek eksikler hakkındakilere de
inanmamayı öğretir.

### Yükseltme istendiğinde çıkan cümle düzeltildi

Eskiden şöyle diyordu: "Uygulayıcı birkaç dakika içinde başlayacak; bu
sayfayı yenileyerek sonucu görebilirsiniz."

İkisi de yanlıştı. Uygulayıcı otuz saniyede bir bakıyor, ve bu bölüm
artık kendini yeniliyor. Yani kendini yenilemeyi yeni öğrenmiş bir sayfa,
size elle yenilemenizi söylüyordu.

---

## v0.20.0 — 2026-09-03

Kurulumda **kaynak profili seçilebiliyor**, ve seçilmediğinde hangisinde
olunduğu artık ekranda yazıyor. Panelde uzun süren işlemler **kendini
tazeliyor**, ve kurulum kontrolleri kurulumdan sonra da **görünür
kalıyor**.

**Şema sürümü: 8** — değişmedi. **Kuran kişinin yapması gereken:**
**hiçbir şey.** Veritabanına dokunulmuyor, servis durmuyor.

### `install.sh --profile hafif|dengeli|tam`

Seçileni hem `collector.toml`'a hem `beacon.toml`'a yazıyor. İkisi birden,
çünkü kardeşinin artık doldurmadığı bir sütun için ASN veri kümelerini
yükleyen bir beacon 136 MB'ı boşa harcar.

| profil | ne yükler | ölçülen taban | en az konteyner |
|---|---|---|---|
| `hafif` | hiçbiri | 32 MB | ~96 MB |
| `dengeli` | yalnız ülke | 160 MB | ~256 MB |
| `tam` | ülke + ASN | 320 MB | ~512 MB |

### Söylenmeyen varsayılan

Bayrağı yazarken asıl kusur çıktı: örnek yapılandırmalar
`asn_lookup.enabled = false` ile geliyor, yani **hiç kimsenin seçmediği
bir kurulum `hafif` profiline düşüyordu** — ülke kırılımı yok, ASN
kırılımı yok — ve betik bunu söylemiyordu. Müşteri haftalar sonra boş bir
grafiğe bakarak öğreniyordu.

Artık her kurulumun sonunda hangi profilde olunduğu yazıyor, ve `hafif`
ise ne kaybedildiği ve nasıl açılacağı da.

**Bayrak bir varsayılan dayatmıyor:** `--profile` verilmezse dosyalar
olduğu gibi kalır. Seçmediğiniz bir şeyi sizin adınıza seçmiyoruz; yalnız
seçili olanı söylüyoruz.

### Uzun işlemler artık kendini tazeliyor

Sağlık sayfasındaki **yükseltme** ve **veri kümesi yenileme** bölümleri,
uçuşta bir istek varken beş saniyede bir kendini yeniliyor. Eskiden
durumu görmenin tek yolu sayfayı elle yenilemekti — ve yükseltme
mesajının kendisi bunu söylüyordu.

Yoklama, iş bitince **kendiliğinden** duruyor: tetikleyici yalnız çalışan
bir istek varken basılıyor, yani gelen yeni kopya tetikleyicisiz geliyor.
Kapatmayı hatırlaması gereken bir şey yok.

Bunun bir yükleme animasyonu olmamasının sebebi ölçüm: panelin sayfaları
2-38 ms sürüyor ve 50.000 satırda 5.000 ile aynı. Gerçek bekleme
sayfalarda değil, dakikalar süren veri kümesi yenilemesindeydi.

### Kurulum kontrolleri kurulumdan sonra da görünüyor

Sağlık sayfasına **Kurulum kontrolleri** bölümü eklendi: yalnız
karşılanmamış kontroller listeleniyor, altında geçen kontrol sayısı.

Bunun düzelttiği kusur şu: ön koşul kontrolleri yalnız `/kurulum/`
altındaki iki sayfada gösteriliyordu, yani kurulumu bitirmiş bir sistemde
görünmez oluyorlardı. Sonraki bir sürümde eklenen bir kontrol, mevcut
kurulumların hiçbir zaman görmeyeceği bir kontrol demekti.

**Saklanan bir durum yok.** Yeni bir kontrol geldiğinde kendiliğinden
beliriyor, düzeltildiğinde kendiliğinden kayboluyor. "Okundu" işareti
taşıyan bir bildirim kutusu değil, çünkü karşılanmamış bir kontrol
düzeltilene kadar zaten doğrudur.

Sihirbazın koştuğu kontrollerden biri burada bilerek yok: servislerin
`/healthz` adreslerini HTTP ile yoklayan kontrol. Ölçüldü — yoklamasız 17
ms, cevapsız tek servisle 5.007 ms, çünkü yoklamalar sırayla ve her
birine beş saniye zaman aşımıyla koşuyor. Sihirbaz bunu bir kez, kurulum
anında karşılayabilir; beş saniyede bir kendini yenileyen bir sayfa
karşılayamaz. Zaten bu sayfa aynı soruyu kalp atışı satırlarından daha
iyi cevaplıyor: `/healthz` "süreç şu an ayakta" der, kalp atışı satırı
"son yazma başarılı oldu, saat 14:02" der.

Kalan kontroller beş saniyeyle sınırlı. Tıkanmış bir veritabanında sayfa
beklemek yerine o bölümün okunamadığını yazıyor, diğer bölümler
görünmeye devam ediyor.

---

## v0.19.0 — 2026-09-02

Panelin **Sağlık** sayfası artık collector'ın hangi kaynak profilini
çalıştırdığını gösteriyor.

**Şema sürümü: 8.** **Kuran kişinin yapması gereken:** panelde
**Sağlık → Yükselt**. Tek bir sütun ekleniyor, veri değişmiyor, servis
durmuyor.

### Ne değişti

`service_heartbeat` tablosuna `profile` sütunu eklendi. Collector her
kalp atışında hangi profilde çalıştığını yazıyor; panel onu servis
tablosunda yeni bir sütunda gösteriyor.

**Neden dosyadan değil de buradan:** panelin veritabanı rolü
`collector.toml`'u okuyamaz ve okumayı öğrenmemeli — o dosya collector'ın
veritabanı parolasını taşıyor, ve beş ayrı rol tam da bunun için var.
Ayrıca daha doğru: dosya birinin ne yazdığını söyler, kalp atışı satırı
**çalışan sürecin gerçekten ne yüklediğini.** İkisi ayrıldığında —
dosya değişmiş, servis yeniden başlatılmamış — panel eskisini gösterir,
ve çalışan da eskisidir.

### Yükseltmeden önce ve sonra

**Bu sürümü kurup şemayı henüz yükseltmediyseniz her şey çalışmaya
devam eder.** Kalp atışı, sütunun olmadığını fark edip onsuz yazıyor;
panel de onsuz okuyor. Görünmeyen tek şey profil sütunu.

Bu bilinçli bir istisna: bu depodaki her yazıcı eksik sütunla başlamayı
**reddeder**, çünkü eksik sütunla yazmak veri kaybettirir. Kalp atışı
veri yazmıyor, **durum** yazıyor — ve o durumu gösteren sayfa, tam da
yükseltmenin ortasında bakılan sayfa. Yükseltme sırasında izlemeyi
körleştirmek, bir etiketi kaybetmekten kötü.

### İlk kurulum deneyimi: üç kusur, üçü de ilk beş dakikada

**Yeni kuranları ilgilendiriyor. Kurulu sistemlerde hiçbir şey
değişmiyor.**

### Kurulum betiği artık veritabanını önceden kontrol ediyor

**Yeni kuranları ilgilendiriyor. Kurulu sistemlerde hiçbir şey
değişmiyor.**

Ölçüldü: veritabanı olmayan bir makinede `install.sh`'ın gösterdiği tek
şey `psql`'in kendi bağlantı hatasıydı — iki kez basılmış, ve bizden tek
cümle yok. Bu, konteyner istemeyen her müşterinin ilk dakikası.

Artık `preflight` üç şeyi **hiçbir şey oluşturmadan önce** soruyor ve her
biri kendi çözümünü söylüyor:

1. PostgreSQL'e ulaşılabiliyor mu — ulaşılamıyorsa dört olası sebep,
   uzaktaki bir veritabanına yönlendirme komutu, ve konteyner yolunun
   veritabanını kendi getirdiği notu.
2. TimescaleDB kurulu mu (`pg_available_extensions`).
3. Önyüklenmiş mi (`shared_preload_libraries`) — değilse eklenecek satır.

Üçü de yarım kurulum bırakmıyor: kontrol, rol ve şema oluşturulmadan
önce. `KURULUM.md` Bölüm 2'ye de "veritabanını nereden bulacaksınız"
başlığı eklendi.

**`--dry-run` yalan söylüyormuş.** Veritabanı olmayan bir makinede bütün
aşamaları yazıp `== done` diyor ve sıfırla çıkıyordu — oysa o mod "bu
makine hazır mı" sorusunun cevabı. Kontroller artık kuru koşuda da
çalışıyor; hepsi okuma olduğu için kuru koşunun koruyacağı bir şey yok.

### Kurulum bittiğinde ne yapılacağı ekranda yazıyor

Dört adım, yapılması gereken sırayla: ters vekil ve TLS, servisleri
başlatma, geliştirici bağlantısı ve sihirbaz, snippet.

**Ve bir sıfırıncı adım:** `install.sh` `site_id`'yi yazmıyor, örnekler
`example-site` ile geliyor. Bu karar **geri alınamaz** — her satır o
kimliğe göre anahtarlanıyor, sonradan değiştirmek o günden başlayan
ikinci bir site açıyor. Betik artık dosyadan okuyup hâlâ örnek değerse
bunu ilk sırada söylüyor.

### `docker/setup.sh` — konteyner yolunda `.env`'i yazan betik

```bash
./docker/setup.sh --site musteri --backend site:443
```

Site kimliğini ve arka ucu bayrakla verirsiniz ya da sorar; veritabanı
parolasını **üretir**. Compose'u çalıştırmıyor, imajı derlemiyor. Mevcut
bir `.env`'in üstüne yazmayı reddediyor — o dosya veritabanı parolasını
taşıyor.

---

## v0.18.0 — 2026-09-02

**KIRICI (dar).** Collector, konteyner bellek sınırının altına sığmayan
bir IP zekâsı ayarıyla artık **başlamıyor.** Sığanlar için hiçbir şey
değişmiyor.

**Şema sürümü: 7** — değişmedi. **Kuran kişinin yapması gereken:**
konteynerle kurduysanız ve `mem_limit` verdiyseniz, aşağıdaki tabloya bir
bakın. systemd/tarball kurulumunda ve sınırsız konteynerde **hiçbir şey.**

### Önlediği kusur

`asn_lookup` veri kümeleri belleğe yükleniyor ve yenileme sırasında
tepe yapıyor. Sığmayan bir kurulum saatlerce sorunsuz çalışıyor, sonra
**yenileme sırasında çekirdek tarafından öldürülüyor.** Collector
müşterinin sitesinin önünde durduğu için site de onunla gidiyor — günde
bir kez, sürecin başladığı saatte, hiçbir uyarı olmadan.

Ölçülen tabanlar (her boyutta beş koşu, hepsinin yaşadığı en küçük boyut):

| profil | `asn_lookup` | taban | en az konteyner |
|---|---|---|---|
| Hafif | `enabled = false` | 32 MB | ~96 MB |
| Dengeli | `country_only = true` | 160 MB | ~256 MB |
| Tam | ikisi de açık | 320 MB | ~512 MB |

Rakamlara hız deposu da ekleniyor: varsayılan 500 istek/sn ve 300 sn TTL
ile ~23 MB.

### Ne reddediliyor, ne reddedilmiyor

**Yalnız zorunlu bir sınırın altında reddediliyor** — yani cgroup, yani
`docker run --memory` ya da compose'daki `mem_limit`. Çekirdek o sayı
için süreci öldürür ve sayı oynamaz.

**Boş belleğe bakarak reddedilmiyor.** Makinenin o anki boş belleği bir
tahmindir ve TimescaleDB'nin çalıştığı bir kutuda iki okuma arasında
yüzlerce megabayt oynar. Orada yalnız **uyarı** var, servis başlıyor.
Tavan hiç okunamıyorsa da başlıyor.

Hem ret hem uyarı, **o bellekte çalışacak en büyük profilin adını**
söylüyor.

### Ne yapmalısınız

Konteyner reddederse iki seçenek var, ikisi de sizin:

- `mem_limit`'i tablodaki değere çıkarın, ya da
- `asn_lookup` ayarını bir küçüğe alın (`country_only = true`, ya da
  `enabled = false`).

Log satırı da her açılışta ne istendiğini ve ne bulunduğunu yazıyor:

```
INFO resource profile profile=tam needs_mb=390 ceiling_mb=15395
     ceiling_from="free memory"
```

---

## v0.17.0 — 2026-09-02

**KIRICI (küçük).** Beacon, daha önce sessizce kabul ettiği iki
yapılandırmayı artık reddediyor. İkisi de zaten bozuk kurulumlar; beacon
bunu söylemiyordu, collector söylüyordu.

**Şema sürümü: 7.** Tek bir sütun, tablo ya da indeks değişmedi.
**Kuran kişinin yapması gereken:** panelde **Sağlık → Yükselt**'i bir
kez çalıştırmak. Aşağıda neden.

### Beacon iki bozuk yapılandırmayı kabul ediyormuş

**1. `privacy.ip_storage` bilinmeyen bir değerse.** Collector
başlamayı reddediyor ve iki geçerli değeri söylüyor; beacon başlıyor,
sessizce `masked`'a düşüyordu. Aynı dosyadan, aynı niyetle kurulmuş bir
çiftin bir yarısı çalışıp öbürünün ayağa kalkmaması demek.

Bu değerin en olası hâli `"hashed"`: beacon'ın kendi belge yorumu ve
şema dosyası, artık var olmayan o modu anlatıyordu. Belgeler de
düzeltildi.

**2. `ip_storage = "full"` ama anahtar yok ya da 32 bayttan kısaysa.**
Daha sessiz olanı. Beacon "full" modda çalışır, **hiç jeton yazmaz** —
zayıf anahtarla jeton üretilmiyor, çünkü üretilseydi mikrosaniyede geri
çevrilirdi — ve hem yapılandırma dosyası hem panel "full" der. Bir /24
içindeki iki ziyaretçiyi ayırmaya dayanan her sayı yanlış olur.

**Sizi ilgilendiriyor mu:** yalnız `[privacy]` bölümünde bu iki alandan
birine dokunduysanız. Hiç dokunmadıysanız varsayılan `masked` ve
hiçbir şey değişmiyor.

### Şema sürümü neden 7 oldu

Şema değişmedi; **parmak izinin hesaplanma kuralı** değişti.

Parmak izi şema dosyalarının ham baytlarından alınıyordu, yani bir
yorumun düzeltilmesi de onu oynatıyordu. Parmak izi ise panelin
"şemanız uyuşmuyor" uyarısını ve yükseltme düğmesini süren değer. Yani
bir cümlenin düzeltilmesi, veritabanının hiç görmediği bir metin için,
her kuruluma geliştirici parolası isteyen bir yükseltme çıkarıyordu.

Artık parmak izi **veritabanına giden DDL'den** alınıyor: yorumlar
ayıklanıyor, boşluklar sadeleştiriliyor. Bundan sonraki yorum
düzeltmeleri hiçbir şeye mal olmuyor. Kuralın kendisinin değişmesi ise
bir kereye mahsus bütün özetleri oynatıyor — bu sürümün sizden bir kez
yükseltme istemesinin sebebi bu.

**Yükseltme ne yapıyor:** aynı şema dosyalarını yeniden uyguluyor
(hepsi `IF NOT EXISTS`) ve kaydı 7'ye çekiyor. Veri değişmiyor, servis
durmuyor.

---

## v0.16.0+A3 — 2026-09-02

Küçük bir VDS için: `asn_lookup` artık yalnız ülke verisini yükleyebiliyor.
**136 MB yerine 59 MB.** Kimse için bir şey değişmiyor; bu yeni bir düğme.

**Şema sürümü: 6** — değişmedi. **Kuran kişinin yapması gereken:**
**hiçbir şey.** Varsayılan bugünküyle aynı: `country_only = false`, yani
her iki veri kümesi de yükleniyor.

### Ne eklendi

`[asn_lookup]` bölümüne `country_only`. Açıldığında ASN veri kümesi
**hiç indirilmiyor, okunmuyor, ayrıştırılmıyor** — boşaltılmıyor,
en baştan alınmıyor. Tasarrufun büyük kısmı zaten burada: her iki
ayrıştırıcı da dosyanın tamamını okuyup içindeki her aralığın listesini
kuruyor, ve o tepe tablonun kendisinden büyük.

Gerçek veri kümeleriyle ölçüldü:

| mod | tutulan |
|---|---|
| tam | 136,2 MB |
| yalnız-ülke | 59,1 MB |

**Ne kaybediliyor:** satırlardaki `asn` / `asn_org` sütunları, panelin
ASN kırılımı, ve ASN'e dayanan üç ayar. Ülke engelleme
(`blocked_countries`) etkilenmiyor — zaten bu modu seçmenin başlıca
sebebi o.

**Ne reddediliyor:** `country_only` ile `apply_to_scoring`,
`blocked_asns` ya da `known_bot_asns` bir arada verilirse collector
**açılmıyor.** Üçü de eşleşecek bir ASN isteyen açık taleplerdir ve bu
modda sessizce hiçbir şey yapmazlardı. Hata mesajı hangi ayarın
çakıştığını söylüyor; hangisinden vazgeçeceğiniz sizin kararınız.

Beacon'da da aynı anahtar var. **İkisinde birden açın:** aynı makinede
koşan bir beacon, kardeşinin artık doldurmadığı bir sütun için ASN
dosyalarını yüklemeye devam eder.

---

## v0.15.0+N6 — 2026-09-01

Bot verisi hiçbir systemd kurulumunda yazılamıyormuş, ve collector bunu
"hiç çekilmedi" diye bildiriyormuş. **Herkesi ilgilendiriyor.**

**Şema sürümü: 6** — değişmedi. **Kuran kişinin yapması gereken:**

- **systemd / tarball ile kurduysanız:** `./release/install.sh`'ı yeniden
  koşturun. Betik `collector.toml`'daki bot verisi yolunu `STATE_DIR`'e
  taşıyor. Yeniden koşmak güvenli — betik zaten öyle tasarlandı,
  yapılandırma dosyalarınızın içindeki sırlara dokunmuyor.
- **Docker ile kurduysanız:** imajı yenileyin (`docker compose pull` ya
  da yeniden `build`) ve yığını yeniden başlatın. **Veri kaybı yok:**
  `state` adlandırılmış bir birim, içeriği aynı kalıp yeni yola
  bağlanıyor.

### Ne bozuktu

Depoda `/var/lib` için iki yazım yan yana yaşıyordu:

| yazım | nerede |
|---|---|
| `/var/lib/crucible-analytic` | `install.sh`'ın `STATE_DIR`'i, collector biriminin `ReadWritePaths`'i |
| `/var/lib/crucible` | `config.example.toml`'un **açık** satırı, Dockerfile, compose, KURULUM.md |

collector birimi `ProtectSystem=strict` ile koşuyor ve `ReadWritePaths`
yalnız birinci yazımı listeliyor. Yani **her systemd kurulumunda**
collector bot verisi dosyasını yazamıyordu.

Sessizdi, çünkü bot verisi bir önbellek: collector bir uyarı yazıp devam
ediyor. Ama uyarı yanlış şeyi söylüyordu —

    "bot data has never been fetched; the known-bot signal is off"
    how=run: collector -config <dosya> -update-bot-data

— "hiç çekilmedi" diyordu, doğrusu "çekemiyorum, oraya yazamam"dı. Yani
önerilen komut çalışmayacaktı, koşturan kişi yine "hiç çekilmedi"
görecekti, ve **bilinen-bot sinyali sessizce kapalı kalacaktı.**

### Ne değişti

- Tek yazım: `/var/lib/crucible-analytic`. On iki yer taşındı.
- collector artık ikisini ayırıyor. Yazamadığında **UYARI** veriyor ve
  çalışmayacak komutu **önermiyor**; gerçekten çekilmemişse eskisi gibi
  bilgi verip komutu söylüyor. Ayrım tahminle değil, yazmayı deneyerek
  yapılıyor — `ProtectSystem=strict` altında dizinin kipi ve sahibi
  doğru görünür, salt-okunur olan altındaki bağlama noktasıdır.
- `install.sh` `STATE_DIR`'i artık yapılandırmaya **yazıyor**. Kabul
  edip yalnız dizin yaratıyordu; yani `STATE_DIR` veren biri, hiçbir
  servisin kullanmadığı bir dizin yaratıyordu.

**Bir uyarı:** `install.sh` artık `collector.toml`'daki `bot_data.path`
satırını `STATE_DIR` ile yeniden yazıyor. Bot verisini bilerek başka bir
dizine koyduysanız, betiği o dizini `STATE_DIR` olarak vererek koşturun.
Dosya adınız korunuyor; taşınan dizin.

---

## v0.14.2 — 2026-09-01

Konteyner kurulumları dört tablo eksik geliyordu. Docker kullanmıyorsanız
bu sürüm sizi ilgilendirmiyor.

**Şema sürümü: 6** — değişmedi. **Kuran kişinin yapması gereken:**
Docker'daysanız **imajı yenileyin** (`docker compose pull` ya da yeniden
`build`) ve yığını yeniden başlatın. Tarball ya da `install.sh` ile
kurduysanız **yapacak bir şey yok.**

### Ne düzeldi

`Dockerfile` şema dosyalarını tek tek `COPY` ediyor — yani
`internal/schemafiles`'daki listenin elle yazılmış bir kopyasını
taşıyor. Kopya **altıda kalmıştı**, şema ona çıkmıştı.

Eksik dördü:

| tablo | ne yapar |
|---|---|
| `panel_logs` | panelin okuyabildiği kayıt sinki |
| `panel_upgrade_requests` | yükseltme düğmesinin kuyruğu |
| `ip_range_refresh_requests` | "şimdi yenile" düğmesinin kuyruğu |
| `schema_version` | veritabanının hangi şekilde olduğu |

Yüzeye çıkışı: init konteyneri 3 ile çıkıyor ve
`relation "schema_version" does not exist` diyor — `install.sh`'ın son
adımı, kimsenin yaratmadığı bir tabloya. Yani yığın hiç ayağa
kalkmıyordu.

**Neden bu kadar uzun sürdü:** bu dosyalar yalnız gecelik hatta
koşuyor, ve gecelik kendi ilk işinde takılıyordu — `e2e`, `install.sh`'ı
`--no-systemd` olmadan çağırıyordu ve betik haklı olarak reddediyordu.
Yani bunu bildirecek olan hat, boşluk açılmadan önce başka bir sebeple
düşüyordu. Hiç koşmayan bir koruma, zayıf bir koruma değil; korumanın
yokluğu.

Liste artık `internal/schemafiles` ile aynada tutuluyor, sıra dâhil.

---

## v0.14.1 — 2026-09-01

Panel, hiçbir kurulumda kendi günlük dizinini açamıyormuş. Açılışta
ölüyordu; diğer üç servis çalışıyordu.

**Şema sürümü: 6** — değişmedi. **Kuran kişinin yapması gereken:
`install.sh`'ı yeniden koşturun** *(zaten kurulu bir sistemde)*. Ya da
elle: `panel.toml` içindeki `[logging] dir` satırını
`/var/log/crucible-analytic` yapın.

### Ne düzeldi

Depoda iki yol ailesi yan yana yaşıyordu. `install.sh` ve **beş systemd
biriminin hepsi** `/var/log/crucible-analytic` diyordu; `panel.toml`
`/var/log/crucible` diyordu.

Panel, örneği yorumsuz `dir` taşıyan tek servis — yani açılışta günlük
ağacı açmayı deneyen tek servis, ve açamayan tek servis:

    panel: logging setup failed: mkdir /var/log/crucible: permission denied

**systemd ile de düşerdi**, başka sebeple: `ProtectSystem=strict` bütün
dosya sistemini salt-okunur yapıyor ve birim yalnız öteki yazımı yazılabilir
kılıyor. Yani bu bir `--no-systemd` sorunu değil, bir isim sorunuydu.

Ayrıca **`LOG_DIR` kabul ediliyor ama hiçbir yapılandırmaya
yazılmıyordu** — onu veren biri, hiçbir servisin açmadığı bir dizin
yaratıyordu. Artık yazılıyor, ve dizin systemd'li systemd'siz iki yolda da
yaratılıyor.

**Yanlış yol talimatlarda da vardı:** `KURULUM.md` ve panelin kendi ön
kontrolü — kopyalanmak üzere hazırlanmış bir komutun içinde — aynı
yanlış dizini söylüyordu. Kodu düzeltip belgeyi bırakmak, kusuru
kullanıcının eline vermek olurdu.

Dosyaların her biri kendi içinde tutarlıydı; tutarsız olan kümeydi. Bir
kabuk betiği, beş birim dosyası, beş TOML örneği ve bir kılavuz arasında
okuyan hiçbir araç yok — o yüzden artık bir test okuyor.

---

## v0.14.0+C8 — 2026-09-01

Geliştirici erişimi artık sizin kararınız: *onay bekle* (varsayılan),
*doğrudan reddet*, ya da *geçici olarak açık*.

**Şema sürümü: 6** — değişmedi. **Kuran kişinin yapması gereken: yok.**
Hiçbir şey değiştirmezseniz sistem bugüne kadarki gibi davranır —
varsayılan zaten "onay bekle".

### Ne var

**Ayarlar → Erişim.** Yeni bir bölüm, tek ayarla. Geliştirici panele
girmek istediğinde ne olacağını siz seçiyorsunuz:

| seçenek | ne olur |
|---|---|
| `ask` (varsayılan) | İstek sizin onayınızı bekler. Bugünkü davranış. |
| `deny` | İstek geldiği anda reddedilir, sebebi geliştiricinin ekranında yazar. |
| `open` | Onay sorulmadan kabul edilir — **yalnız** verdiğiniz bitiş zamanına kadar. |

**`open` kalıcı olamıyor, ve bu kasıtlı.** Bitiş zamanı boşsa ya da
geçmişse politika kendiliğinden `ask`'e döner. Destek çağrısı bitince
kapatmayı unutmak, bu ayarın var olma sebebinin kendisi.

**Süresi geçen pencere reddetmeye değil sormaya düşüyor.** Diğer türlüsü
sizi kendi geliştiricinizden, ona en çok ihtiyaç duyduğunuz anda ayırırdı
— ve siz bunu hiç istememiştiniz.

**Geliştirici parolası istemiyor.** Kuralımız "geliştiriciye iş
çıkarabilen şeyler parolanın arkasında"; burası tersi, siz kendinizi
koruyorsunuz. Ulaşamadığınız bir koruma koruma değildir.

**Gerçek bir kilit olmadığını açıkça söylüyoruz.** Sunucuya kabuk
erişimi olan biri zaten içeride; burada kapanan yalnız panelin kapısı.
Ayarın kendi açıklaması da bunu yazıyor.

**Karar iki yere yazılıyor.** Denetim kaydına (kim, ne zaman, hangi
politika, hangi bitiş zamanı) *ve* reddedilen geliştiricinin gördüğü
sayfaya. İkincisi olmasaydı kapatılmış bir politika, kimsenin bağlantı
kuramadığı bir arıza gibi görünürdü.

O sayfa politikayı **her ziyaretçiye** yazıyor, bağlantısı geçerli olsun
olmasın. Bilerek: cümle jetonu değil kurulumu anlatıyor, yani bilinmeyen
bir jetonla süresi dolmuş bir jeton hâlâ birebir aynı sayfayı görüyor.
Aksi hâlde sayfa, tahmin eden birine jetonunun bir zamanlar gerçek
olduğunu doğrulardı.

---

## v0.13.2 — 2026-09-01

Aynı anda çalışan iki yükseltici birbirini eziyordu. Şema dosyalarının
metni değişti; veritabanının şekli değişmedi.

**Şema sürümü: 6** *(5'ten yükseldi)*. **Kuran kişinin yapması gereken:
panelden yükseltme düğmesine basmak.** Acele değil: hiçbir sütun, hiçbir
yetki, hiçbir tablo değişmedi — eski şemayla çalışan bir kurulum
çalışmaya devam eder, panel yalnız "yükseltme var" der.

*(Faz kodu yok; gerekçesi `VERSIONING.md`'de — bir düzeltme hiçbir fazı
tamamlamıyor.)*

### Ne düzeldi

Bir şema dosyasını üst üste uygulamak güvenliydi; **aynı anda** uygulamak
değildi, ve bu ikisinin ayrı şeyler olduğu hiç ölçülmemişti. Kilitsiz üç
oturum bütün dosyaları on ikişer kez uygularken 360 denemenin 17'si
düştü: `tuple concurrently updated` (XX000) ve `deadlock detected`
(40P01). İkisi de kimsenin yeniden denemediği hatalar.

İki ayrı sebep çıktı:

- **Değişmeyen bir GRANT da yazıyor.** `GRANT`, hedefin ACL satırını
  içeriği değişmese de yeniden yazıyor. Üç oturum aynı, zaten verilmiş
  yetkiyi 300'er kez verdiğinde 900 denemenin 93'ü çakıştı. Artık her
  GRANT önce `has_table_privilege` / `has_function_privilege` ile
  gerekli olup olmadığını soruyor — yetki başına ayrı ayrı, çünkü virgüllü
  biçim "bunlardan herhangi biri" demek ve yarım yetkili bir rolü tam
  sayardı.
- **`CREATE OR REPLACE FUNCTION` ve `DROP POLICY` + `CREATE POLICY`**
  ifade ifade koşullu hâle getirilemiyor. İkincisini "politika zaten
  varsa atla" diye sarmak, ileride değişen her politikanın sessizce hiç
  uygulanmaması demek olurdu — düzeltilenden daha kötü bir hata. Bunun
  için yükseltici artık bütün uygulama boyunca tek bir danışma kilidi
  tutuyor (`internal/dblock`).

Kilidin ne yaptığı ölçüldü, ve beklenen şey değildi. Yükseltici zaten 250
ms'lik `lock_timeout` ile çalıştığı için XX000'e hiç varmıyordu; kilit
kaldırılmış hâlde beş koşuda tek bir çakışma çıkmadı. Kilit, **boşuna
sıraya dönmeyi** engelliyor:

| | uygulandı | yol verdi |
|---|---|---|
| kilitle | 24 | 0 |
| kilitsiz | 8 | 16 |

Kilitsiz hâlde işin üçte ikisi kuyruğa geri dönüyor — her biri, müşterinin
yükseltmesinin ekranda görünmeyen bir sebeple bir tur daha beklemesi.

Aynı anahtar `internal/testdb.SchemaApplyLock` olarak testlere de açıldı:
şema dosyasını elle uygulayan bir paketin `lock_timeout`'u yok, ve
yukarıdaki çakışmalara gerçekten varıyor. Düz bir
`go test -tags integration ./...` ikinci koşusunda tam olarak bundan
kırmızıya döndü — hiç yanlış olmamış bir GRANT'i suçlayarak.

---

## v0.13.1 — 2026-09-01

Tek bir düzeltme, ve kuran kişiyi ilgilendirmiyor: kapıdaki bir eşik,
ölçüldüğü makineye aitmiş.

**Şema sürümü: 5** — değişmedi. **Kuran kişinin yapması gereken: yok.**

*(Faz kodu yok; gerekçesi `VERSIONING.md`'de — bir düzeltme hiçbir fazı
tamamlamıyor.)*

### Ne düzeldi

"Uygulama sırasında hiçbir servis durmuyor" ölçümünün duyarlı yarısı,
sabit 250 ms'lik bir tabana dayanıyordu. O sayı, yükseltmenin 35–86 ms
sürdüğü bir makinede seçilmişti. Yükseltmenin 461 ms sürdüğü bir koşuda
yazıcılar 393 ms bekliyor — ve bu bekleyiş **pencerenin içinde**, yani
bir şema dosyasının ShareLock'unun arkasında sıraya girmekten ibaret,
zaten bilinen ve kabul edilen davranış.

Taban artık `max(250 ms, yükseltmenin süresi)`: bir sorgu, yükseltmenin
sürdüğünden uzun süre yükseltme yüzünden beklemiş olamaz. Mutlak 2
saniyelik tavan yerinde.

Bu yalnız geliştirme hattını ilgilendiriyor — ürünün davranışı
değişmedi.

---

## v0.13.0+M3 — 2026-09-01

Elle yenileme düğmesi: *Sağlık* sayfasında "şimdi yenile", ve altında son
çekimlerin sonucu.

**Şema sürümü: 5** *(4'ten yükseldi)*. **Kuran kişinin yapması gereken:
yükseltmeyi uygulayın** — panelde *Sağlık → Yükselt* düğmesi, ya da
`install.sh`'ı yeniden koşturun. Uygulamazsanız hiçbir şey durmaz; yalnız
yenileme düğmesi çalışmaz.

### Ne var

**Panel dışarı hiçbir bağlantı açmıyor, ve açmayacak.** Düğme bir satır
yazıyor; çeken servis (collector ya da beacon, hangisinde `asn_lookup`
açıksa) otuz saniyede bir bakıyor, alıyor, zaten yapmayı bildiği
yenilemeyi yapıyor ve sonucu geri yazıyor. L3'ün deseninin aynısı, bir
tablo ötede — sebebi de aynı: müşterinin tarayıcısının ulaştığı sürecin
dışarı istek atması, tam olarak atmaması gereken yerde bir SSRF yüzeyi
olurdu.

**İki kez basmak iki çekim başlatmıyor.** Kısmi tekil indeks, uygulama
mantığı değil: engellenmek istenen şey iki isteğin *aynı anda* "hiçbir
şey koşmuyor" diye karar vermesi, ve önce bakıp sonra yazan bir kontrol
bunu hiç engellemez.

**Sonuç ekranda.** Düğmenin altında her veri kümesi dosyasının son
denemesi: ne zaman, kaç satır, kaç bayt, tamam mı düştü mü, ve düştüyse
hata zinciri. M2'nin kaydı, nihayet bir sayfada.

**Geliştirici parolası istemiyor, ve bu bilinçli.** Kuralımız "geliştiriciye
iş çıkarabilen şeyler parolanın arkasında" — bu hiç kimseye iş
çıkarmıyor: müşterinin kendi sunucusuna, kendi hattından, iki kamuya açık
dosyayı yeniden indiriyor. Ayarları değiştirebilen basabilir.

**"Kimse dinlemiyorsa" durumu bir cümleyle söyleniyor.** `asn_lookup`
varsayılan olarak kapalı, yani çoğu kurulumda bu kuyruğu yoklayan hiçbir
şey yok. İstek birkaç dakika sonra kendiliğinden düşüyor ve sayfa bunun
sebebini yazıyor — yoksa hiç kımıldamayan bir satır, panelin bozuk
olduğu anlamına gelirdi.

---

## v0.12.0+M2 — 2026-09-01

Çekim kaydı: hangi veri kümesi ne zaman çekildi, kaç satır, kaç bayt, ve
başarısızsa neden.

**Şema sürümü: 4** *(3'ten yükseldi)*. **Kuran kişinin yapması gereken:
yükseltmeyi uygulayın** — panelde *Sağlık → Yükselt* düğmesi, ya da
`install.sh`'ı yeniden koşturun.

Uygulamazsanız hiçbir şey durmaz: servisler çalışmaya devam eder, adres
çözümlemesi çalışır, sayfalar çizilir. Yalnız çekim kaydı yazılamaz ve
her yenilemede günlüğe bir uyarı düşer — yani bu sürümün getirdiği tek
şey çalışmaz.

`install.sh`'ı yeniden koşturmak ayrıca aşağıdaki gereksiz yetkileri de
kaldırır.

### Ne var

**Her dosya için bir satır.** Bir yenileme, bir veri kümesinin IPv4 ve
IPv6 dosyalarını ayrı ayrı çekiyor ve bunlar **ayrı ayrı** başarısız
oluyor — kod da zaten öyle davranıyor, düşen ailenin eski tablosunu
koruyup çalışanı değiştiriyor. Yenileme başına tek satır, "IPv6 güncel,
IPv4 bir aylık" durumunu tek bir sonuca sıkıştırmak zorunda kalırdı ve
onun dürüst bir değeri yok.

Yedek sıralaması da bundan bedava çıkıyor: seçilen kaynak düşüp sıradaki
çalıştığında ikisi de kayıtta, sırasıyla.

**Kesilmiş dosyayı yakalayan şey bayt sayısı.** Her iki ayrıştırıcı da
bozuk bir satırda durup okuduğunu saklıyor, yani yarıda kesilmiş bir
dosya hatasız ayrışıyor ve geriye internetin yarısı eksik bir tablo
bırakıyor. Tek fark bayt sayısında görünüyor — ve `Content-Length`'ten
değil, gerçekten okunandan sayılıyor.

**Yazan ile okuyan ayrı.** Çeken servisler (collector, beacon) yazıyor;
panel **yalnız okuyor**. Kimsede `UPDATE` yok: bir çekim satırı yazıldığı
anda bitmiş oluyor, sonradan değiştirme yetkisi yalnızca bir
başarısızlığı başarı gibi göstermeye yarardı.

**Saklama: 90 gün**, ve süpürmeyi yazan servis yapıyor. Planı burada bir
adım değiştirdik; gerekçesi `PurgeOldFetches`'te ve `PLAN.md`'de.

**Yetkiler tablonun şema dosyasında.** Bir kurulum bu şemaya iki yoldan
ulaşıyor ve ikisi aynı işi yapmıyor: `install.sh` şemaları *ve*
`grants.sql`'i uyguluyor, yükseltme düğmesi yalnız şemaları. Bu, L1–L3
yazıldığından beri eklenen ilk tablo olduğu için ilk kez ortaya çıktı —
düzeltilmeseydi düğmeyle yükselten her kurulum, hiçbir servisin
yazamadığı bir tablo alacaktı ve bu sürümün getirdiği tek şey sessizce
çalışmayacaktı.

### Kaldırılan yetkiler

Üç `GRANT ... ON SEQUENCE` gereksizdi: ilgili sütunlar
`GENERATED ALWAYS AS IDENTITY` ve PostgreSQL identity sekansını
sütununun parçası sayıyor — tabloya `INSERT` yetkisi tek başına yetiyor.
Ölçüldü. Dosyadan silmek kurulu veritabanından silmiyor, o yüzden açık
`REVOKE` de eklendi; `install.sh` yeniden koşturulduğunda uygulanır.
`BIGSERIAL` sekansları farklı ve yetkilerine gerçekten ihtiyaç
duyuyorlar.

---

## v0.11.1 — 2026-09-01

Tek bir düzeltme, ve ciddi olanı: **yükseltme, çalışan bir yazmayı
öldürebiliyordu.**

**Şema sürümü: 3** — değişmedi. Kuran kişinin yapması gereken: **yok.**

*(Faz kodu yok, ve bu kasıtlı: faz kodu "hangi fazın tamamlanmasıyla
çıktı" demek, bir düzeltme ise hiçbir fazı tamamlamıyor. `VERSIONING.md`
gerekçeyi yazıyor.)*

### Ne düzeldi

Şema dosyası tek bir işlem olarak koştuğu için aldığı her kilidi dosya
bitene kadar tutuyor. `panel_operations`'a yazan bir panel isteği,
yabancı anahtarı yüzünden aynı iki tabloyu **ters sırada** kilitliyor, ve
PostgreSQL çevrimi birini öldürerek çözüyor — kurban müşterinin yazması
olabiliyordu. Oysa yükseltme düğmesi ona "siteniz trafik alırken basmak
güvenli" demişti.

Uygulayıcı artık kendi bağlantısında `lock_timeout = 250ms` ile koşuyor.
`deadlock_timeout`'un (1 sn) altında olması seçim değil şart: altında
kalırsa hiçbir dedektör koşmaz, çevrimi **yükseltmenin** geri çekilmesi
kırar, ve seçilecek bir kurban olmaz.

İkinci yarısı: kilit zaman aşımı artık **başarısızlık değil**. İstek
sıraya geri konuyor, sebep satıra yazılıyor, sonraki tik tekrar deniyor.
Daha önce böyle bir an "Yükseltme başarısız" diye görünüyordu ve düğmeye
tekrar basmak gerekiyordu; artık "Sırada" görünüyor ve son denemenin
sebebi yanında yazıyor.

*(Yanlış olan yalnız davranış değildi: `IF NOT EXISTS`'in "ağır kilit
almaz" diye yazılı açıklaması da yanlıştı. Ölçüldü — işi atlıyor, kilidi
değil. Ayrıntısı `NOTES.md`'de.)*

---

## v0.11.0+M1 — 2026-09-01

Kaynak kütüphanesi: hangi IP aralığı veri kümesinin kullanılacağı artık
panelden seçiliyor. Ayrıca `v0.10.0+D4c` ile bu sürüm arasına giren iki
düzeltme burada.

**Şema sürümü: 3** — değişmedi. Kuran kişinin yapması gereken: **iki
şey, ikisi de yalnız belirli kurulumlar için** (aşağıda).

### Ne var

**Beş veri kümesi, ikisi varsayılan.** Ülke tarafında `user-country`
(varsayılan — ziyaretçinin ülkesi), `server-country` (adresin
*barındırıldığı* ülke), `iptoasn-country`; ASN tarafında `origin-asn`
(varsayılan) ve `iptoasn-asn`. Beşi de PDDL 1.0 — kamu malı, atıf
gerekmiyor, kurulumun üstünde hiçbir yükümlülük yok. **Hiç dokunmayan
kurulum bugüne kadarki iki veri kümesini indirmeye devam ediyor;
davranış birebir aynı.**

**Üç yeni ayar** — *Ayarlar → Toplama* altında: ülke kaynağı, ASN
kaynağı, ve biri erişilemezse denenecek yedek sıralaması. Üçü de
yeniden başlatma istemiyor; bir sonraki zamanlanmış yenilemede etkili
oluyor.

**Panelde serbest URL kutusu yok, ve olmayacak.** Bir kaynak URL değil,
URL *ve ayrıştırıcı*dır: kutu ancak zaten desteklenen biçimdeki bir şeyi
gösterebilirdi, yani "yeni sağlayıcı ekle" işini yapamazdı ama
yapabildiğini sandırırdı. Yapabileceği tek şeyin (aynı biçim, başka
sunucu) karşılığı zaten `asn_lookup.local_csv_path`.

**Ayna dizini seçimi izliyor.** `local_csv_path` kullanan kurulumda
okunan dosya adları seçilen veri kümesinden geliyor. Varsayılanlarda
değişen bir şey yok.

### Düzeltmeler

**`schema_admin` parolası hiçbir yere gitmiyordu.** `install.sh` beş
rolün beşi için de parola üretiyordu ama yalnız dördünü bildiriyordu:
beşincisi ne dosyaya yazılıyordu ne ekrana. Yapılandırma dosyası önceden
varsa yükseltici `change-me` ile kalıyor ve şema uygulayamıyordu, ve
ekranda bunu söyleyen hiçbir şey yoktu. **Kuran kişinin yapması gereken:
`install.sh`'ı daha önce koşturduysanız `/etc/crucible-analytic/upgrader.toml`
içindeki parolanın gerçek olduğunu doğrulayın** (`change-me` ise
yükseltici çalışmaz). Betik artık beş rolü tek bir tablodan okuyor ve
sayıyı sayarak yazıyor.

**CI haftalardır kırmızıydı.** İki sebep: iş akışının rol döngüsü beşten
dördünü oluşturuyordu (`schema_admin` yoktu), ve `actions/checkout`
varsayılan sığ klonu CLA testine deponun değil klonun bir olgusunu
rapor ettiriyordu. İkisi de kapatıldı. **Kuran kişiyi ilgilendirmez** —
yalnız geliştirme hattı.

*(Yükseltmenin çalışan bir yazmayı öldürebilmesi de bu sürümün
etiketlendiği gün bulundu, ama **bu sürümde değil** — düzeltmesi bir
commit sonra geldi ve `v0.11.1`'de. Bu ağaç hâlâ o kusuru taşıyor.)*

---

## v0.10.0+D4c — 2026-08-31

Ayarlar sayfası: 28 ayar yedi açılır kategoriye.

**Şema sürümü: 3** — değişmedi. Kuran kişinin yapması gereken: yok.

### Ne var

Görünüm, toplama, bot, gizlilik, sınırlar, tanılama, bakım. Varsayılan
görünüm kısa: kategoriler kapalı geliyor. Akordeon `<details>` ile —
betik yok, dolayısıyla CSP'de gevşetilen hiçbir şey yok.

Reddedilen bir kaydın bulunduğu bölüm **açık** geliyor. Olmasaydı,
sayfanın tepesindeki kırmızı bant sebebini kapalı bir başlığın arkasında
bırakırdı.

---

## v0.9.0+L3 — 2026-08-31

İlk etiketli sürüm. On altı faz, 130 commit, altı binary.

**Şema sürümü: 3.** Kuran kişinin yapması gereken: yok — bu ilk
etiketli sürüm, karşılaştırılacak öncesi yok.

### Ne var

**Altı süreç, beş veritabanı rolü, tek veritabanı.** Toplayıcı, beacon,
salt okunur API, panel, geliştirici parolası aracı, ve şema
yükselticisi. Panelin rolünün analitik tablolara **hiçbir** yetkisi yok
— trafik sayılarını HTTP üzerinden okuma API'sinden alıyor, tıpkı dış
bir panel gibi. Bu iki olumsuz olgu (panel analitiği okuyamaz, API
yazamaz) açılışta canlı veritabanına karşı doğrulanıyor, varsayılmıyor.

**Toplama.** JA4 TLS parmak izi + istek hızı ile bot/insan ayrımı,
hiçbir şeyi engellemeden veya geciktirmeden. 0-100 skor ve gerekçesi.
Tam modda TCP bağlantısı başına değil **HTTP isteği başına** kayıt.

**Beacon.** İstemci tarafı sayfa analitiği — sayfalar, yönlendirenler,
kampanyalar, tarayıcılar, cihazlar, özel olaylar; çerezsiz ziyaretçi
kimliği.

**Panel.** Site panosu, kırılımlar, üye yönetimi, kurulum sihirbazı,
sağlık sayfası, ayarlar, işlem kaydı ve günlükler.

**Yükseltme yolu (L1–L3).** Veritabanı hangi şemada olduğunu söylüyor;
eksik sütunla açılışta duruyor; ve paneldeki düğme bir istek satırı
yazıyor, yetkili altıncı binary uyguluyor. *Uygulama sırasında hiçbir
servis durmuyor* — ölçüldü: pencere içindeki en kötü sorgu 2.3–9.9 ms,
boştaki en kötü 5.0–83.5 ms.

**Yayın.** Tekrarlanabilir paket (aynı commit'ten iki yapı aynı
byte'lar), systemd birimleri ve zamanlayıcı, kurulum betiği, Docker
imajı ve compose dosyası.

### Bilinen eksikler

`README.md` → *Explicitly out of scope for this phase* ve
`SECURITY.md` → *Open, and stated on the pages that are affected*.
Kısaca: parola değişikliği diğer cihazlardaki oturumları kapatmıyor,
kurtarma kodu yok, panelde genel eşzamanlılık sınırı yok.

`PLAN.md`'de tamamlanmamış gruplar: D4c, M, A2/A3, B3, E, F1, F3.

### Lisans

Apache-2.0. Bu ve bundan sonraki her 0.x sürümü Apache-2.0 altında
kalır — geri alınamaz. Ticari lisansa geçiş 1.0.0'da; gerekçesi
`VERSIONING.md`'de.

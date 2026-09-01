# Değişiklik günlüğü

Sürüm şeması ve ne zaman sürüm çıkarıldığı: `VERSIONING.md`.

Her girdi üç şeyi söyler: ne değişti, şema sürümü kaç, ve **kuran
kişinin bir şey yapması gerekiyor mu**. Üçüncüsü en önemlisi ve genelde
yazılmayanı: bir sürüm notunu okuyan kişinin asıl sorusu "ben ne
yapacağım".

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

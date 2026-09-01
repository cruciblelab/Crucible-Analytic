# Değişiklik günlüğü

Sürüm şeması ve ne zaman sürüm çıkarıldığı: `VERSIONING.md`.

Her girdi üç şeyi söyler: ne değişti, şema sürümü kaç, ve **kuran
kişinin bir şey yapması gerekiyor mu**. Üçüncüsü en önemlisi ve genelde
yazılmayanı: bir sürüm notunu okuyan kişinin asıl sorusu "ben ne
yapacağım".

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

**Yükseltme, çalışan bir yazmayı öldürebiliyordu.** Şema dosyası tek bir
işlem olarak koştuğu için aldığı her kilidi dosya bitene kadar tutuyor;
`panel_operations`'a yazan bir panel isteği aynı iki tabloyu ters sırada
kilitliyor, ve PostgreSQL çevrimi birini öldürerek çözüyor — kurban
müşterinin yazması olabiliyordu. Uygulayıcı artık `lock_timeout = 250ms`
ile koşuyor: `deadlock_timeout`'un (1 sn) altında kaldığı için çevrimi her
zaman **yükseltme** geri çekilerek kırıyor, trafik değil.

Bunun ikinci yarısı: kilit zaman aşımı artık **başarısızlık değil**.
İstek sıraya geri konuyor, sebep satıra yazılıyor, sonraki tik tekrar
deniyor. **Kuran kişinin yapması gereken: yok.** Daha önce böyle bir an
"Yükseltme başarısız" diye görünüyordu ve düğmeye tekrar basmak
gerekiyordu; artık "Sırada" görünüyor ve son denemenin sebebi yanında
yazıyor.

*(Yanlış olan yalnız davranış değildi: `IF NOT EXISTS`'in "ağır kilit
almaz" diye yazılı açıklaması da yanlıştı. Ölçüldü — işi atlıyor, kilidi
değil. Ayrıntısı `NOTES.md`'de.)*

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

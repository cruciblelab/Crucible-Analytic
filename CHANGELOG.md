# Değişiklik günlüğü

Sürüm şeması ve ne zaman sürüm çıkarıldığı: `VERSIONING.md`.

Her girdi üç şeyi söyler: ne değişti, şema sürümü kaç, ve **kuran
kişinin bir şey yapması gerekiyor mu**. Üçüncüsü en önemlisi ve genelde
yazılmayanı: bir sürüm notunu okuyan kişinin asıl sorusu "ben ne
yapacağım".

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

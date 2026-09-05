# Sürümleme

Bu dosya iki soruya cevap veriyor: bir sürüm numarası ne söz veriyor, ve
ne zaman yeni bir tane veriliyor.

---

## Kısa cevap

```
v0.9.0+L3
 │ │ │  └── hangi fazın tamamlanmasıyla çıktı
 │ │ └───── düzeltme
 │ └─────── yeni yetenek, şema uyumlu
 └───────── kırıcı değişiklik
```

**Her commit'e sürüm verilmiyor.** Sürüm, bir *faz tamamlandığında*
veriliyor. Bugüne kadar 130 commit var; bunlara 130 sürüm vermek sürümü
anlamsızlaştırırdı — bir sürüm, insanların kurduğu bir yapı hakkında
verilmiş sözdür, geçmişteki bir satır değil.

---

## Neden v0.9.0'dan başlıyor

**v0.1 değil**, çünkü doğru değil. On altı faz bitti; sistem kuruluyor,
çalışıyor, uçtan uca kanıtı var, altı binary'si ve tekrarlanabilir
paketi var. 0.1 diyen bir sürüm numarası okuyucuya yalan söyler.

**v1.0 da değil, ve bu daha önemli.** SemVer'de 1.0.0 tek bir şey vaat
eder: *bundan sonra kırmayacağım*. Bu projenin okuma API'si README'de
açıkça "public contract" diye geçiyor, ve PLAN'da hâlâ tamamlanmamış
gruplar var (D4c, M, A, B3, E, F1, F3). 1.0 demek ya o sözü tutmak için
o grupları kısıtlı yapmak, ya da sözü ilk fırsatta bozmak olurdu.

**0.9.0, bitmediğini söyleyen ama nerede olduğunu da söyleyen sayı.**

### 1.0.0 ne zaman

Faz tablosu tamamlandığında. Ve bu tesadüfen değil, kasten, **lisans
değişikliğiyle aynı noktada**:

```
0.x         Apache-2.0        kurulur, çatallanabilir, ticari kullanılabilir
1.0.0 →     kaynağı görünür   derlenmiş sürüm lisans anahtarıyla
            ticari lisans
```

Üçü tek bir an: fazlar bitti, ürün tamam, lisans değişti, 1.0 çıktı.

---

## Faz kodu neden `+` ile

`v0.9.0+L3` içindeki `+L3` SemVer'in **build metadata** alanı. Standart
onu sıralamada *yok sayar* — yani `v0.9.0+L3` ile `v0.9.0+M1`
karşılaştırıldığında ikisi de aynı sürümdür ve olması gereken budur:
faz kodu bir etiket, bir mertebe değil.

Alternatifleri denendi ve ikisi de yanlıştı:

- `v0.9.0-L3` — SemVer'de tire **ön sürüm** demektir, yani `0.9.0`'dan
  *küçüktür*. Faz kodu bir ön sürüm değil, o yüzden bu sıralamayı bozar.
- `v0.9.0L3` — SemVer olarak ayrıştırılamaz. Bunu okuyan her araç
  (Go modülleri, paket yöneticileri, `sort -V`) kırılır.

**Bilinen kısıt:** Docker imaj etiketleri `+` karakterini kabul etmez
(izin verilen küme `[A-Za-z0-9_.-]`). Bu depo bugün otomatik imaj
yayımlamıyor, ama yayımladığı gün dönüşüm şu olacak ve tek yönlü
olacak:

```
git etiketi   v0.9.0+L3
docker        0.9.0_L3
```

`+` → `_`. Yazılı olmasının sebebi, o gün birinin iki ayrı sürüm şeması
uydurmasını engellemek.

### Düzeltme sürümlerinde faz kodu yok

`v0.11.1`'in faz kodu yok, ve bu eksiklik değil. Faz kodu tek bir şey
söylüyor: *bu sürüm hangi fazın tamamlanmasıyla çıktı*. Bir düzeltme
hiçbir fazı tamamlamıyor, dolayısıyla söyleyecek bir şeyi de yok.

Denenmiş ve reddedilmiş alternatif, `v0.11.1+M1` yazmaktı: okuyan kişiye
"M1 sürümü" diye görünür, ve o adı taşıyan iki farklı ağaç olurdu.

**Nereden çıktı:** `v0.11.0+M1` etiketlendiği gün, notunun içinde ondan
*bir commit sonra* gelmiş bir düzeltme anlatılıyordu. Yani sürüm notu,
etiketli ağacın içermediği bir şeyi iddia ediyordu — kuran kişinin
düzeltmeyi aldığını sanacağı, kimsenin de fark etmeyeceği türden. Not
bölündü, düzeltme kendi sürümünü aldı.

**Kural:** bir girdi yalnız kendi etiketinin gösterdiği ağaçtaki şeyleri
anlatır. Sonradan gelen bir şeyi anlatmak istiyorsa, o bir sonraki
sürümdür.

---

## İki ayrı sürüm var, ve karıştırılmamalı

Bu, projenin en kolay yapılacak hatası:

| | nedir | nerede |
|---|---|---|
| **Yapı sürümü** | `v0.9.0+L3` — insanların konuştuğu sürüm | `git describe`, `-X main.version`, panelin Hakkında bölümü |
| **Şema sürümü** | `3` — veritabanının hangi şekilde olduğu | `internal/schemaver.Version` |

**Şema sürümü, SemVer'in MINOR'ı değildir.** L1/L2/L3'ün bütün yükseltme
makinesi o tam sayının üstünde duruyor: açılış kontrolü, yükseltme
düğmesi, parmak izi reddi. Onu yapı sürümüne bağlamak, hiçbir şema
değişikliği içermeyen bir düzeltme sürümünün her kurulumdan yükseltme
istemesi demek olurdu.

İkisi bağımsız artar. Bir sürüm notu ikisini de yazar.

---

## Ne zaman hangi hane artar

**MAJOR** — kuran kişinin bir şey *yapması gereken* değişiklik:

- Eski binary'nin çalışamayacağı bir şema değişikliği
- Kaldırılan ya da anlamı değişen bir yapılandırma alanı
- Okuma API'sinde kırıcı bir değişiklik
- Bir rolün yetkisinin değişmesi

**MINOR** — yeni yetenek, şema uyumlu. Bir faz genellikle budur.

**PATCH** — düzeltme. Yeni yüzey yok.

0.x'te MAJOR artmıyor: SemVer 0.x'i "her şey değişebilir" olarak
tanımlar, ve bu doğru — 1.0'a kadar kırıcı değişiklik MINOR'a yazılıyor
ve sürüm notunda **KIRICI** diye işaretleniyor.

---

## Bir sürüm nasıl çıkarılır

1. Faz `PLAN.md`'de ✅, kapı yeşil (`CONTRIBUTING.md` → *The gate*).
2. `CHANGELOG.md`'ye giriş: ne değişti, şema sürümü kaç, kuran kişinin
   yapması gereken bir şey var mı.
3. Açıklamalı etiket:
   ```
   git tag -a v0.9.0+L3 -m "L3: yükseltme yolu"
   git push origin v0.9.0+L3
   ```

   **2 ile 3 arasında kapı kırmızıdır, ve bu bilerek böyle.**
   `TestEveryReleaseNoteHasATagAndEveryTagHasANote` en yeni bir nota
   etiketsiz olma izni verir — yukarıdaki sıra tam olarak bunu üretiyor —
   ama **ikinciye vermez.** Etiketsiz bir not "çıkarıldı" diye okunur ve
   `git checkout` edilemez; iki tanesi, birinin unutulduğu anlamına
   gelir.

   Yani 2 ve 3 birlikte gider. Etiketi başkası kesecekse (bu depoda
   olduğu gibi: bazı oturumların kimlik bilgileri `refs/tags/` yazamıyor,
   HTTP 403 alır) not ile etiket arasında geçen süre boyunca kapı
   kırmızı kalır — bir sonraki notu yazmadan önce kesilmiş olmalı.

   Ölçüldü: `v0.14.2`'nin notu yazıldı, etiketi kesilmedi, ve sonraki not
   (`v0.15.0+N6`) eklenince kapı `unit (-race)` işinde düştü. Tek
   kontrol, doğru sebep.

   **Etiketin nereye kesildiği de kontrol ediliyor.** `v0.22.0` bir kez
   203 commit geriye, `main` üzerinde olmayan bir commit'e kesildi. Not
   doğruydu, numara doğruydu, ve `git checkout v0.22.0` kendi notunun
   anlattığı özelliği taşımayan bir ağaç veriyordu — çünkü not ile
   etiket *ad* üzerinde anlaşıyor, ikisi de nereyi gösterdiğine
   bakmadan. `TestEveryTagPointsAtSomethingOnTheDefaultBranch` artık
   soruyor.

   Yanlış yere kesilmiş bir etiketi düzeltmenin yolu silip yeniden
   kesmektir:

   ```
   git tag -d v0.22.0
   git push origin :refs/tags/v0.22.0
   git tag -a v0.22.0 <dogru-commit> -m "..."
   git push origin v0.22.0
   ```
4. `release/build.sh` gerisini yapıyor: `git describe --tags` etiketi
   bulur, altı binary'ye `-X main.version` ile basar, paketi
   `crucible-analytic-v0.9.0+L3` diye adlandırır ve SHA256SUMS üretir.

Dördüncü adım için hiçbir şey yazılmadı — altyapı G2'den beri hazırdı ve
etiket olmadığı için `58e2acd-dirty` gibi isimler üretiyordu.

---

## Apache-2.0 geri alınamaz — ve bu, ne zaman etiketleneceğini etkiler

Yayımlanan **her sürüm**, yayımlandığı lisans altında sonsuza kadar
kalır. `v0.9.0` Apache-2.0 ile etiketlendiğinde:

- O ağacı isteyen herkes çatallayabilir, değiştirebilir, ticari
  satabilir. Sonsuza kadar.
- Sonradan lisans değiştirmek **geçmiş sürümleri geri almaz**, sadece
  gelecek sürümleri bağlar.

Bu bir engel değil, bir zamanlama sorusu. Apache-2.0 altında ne kadar
çok sürüm çıkarsa, birinin devam ettirebileceği "ücretsiz taban" o kadar
büyür. Bunun karşılığında güven, kullanıcı ve inandırıcılık geliyor —
kimsenin görmediği bir kapalı kaynak MVP'nin de değeri yok.

Şu anki karar: **0.x Apache-2.0, 1.0.0 ticari.** Yani çatallanabilir
taban, fazları eksik bir 0.9 olur. Eksik bir 0.9 ürün değildir, ve
değerin çoğu 1.0'da olur.

`CLA.md` bu geçişi mümkün kılan şey; §2'deki alt lisanslama hakkı ve §8
devir maddesi olmadan bu karar hiç verilemezdi.

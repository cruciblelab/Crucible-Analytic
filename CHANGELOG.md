# Değişiklik günlüğü

Sürüm şeması ve ne zaman sürüm çıkarıldığı: `VERSIONING.md`.

Her girdi üç şeyi söyler: ne değişti, şema sürümü kaç, ve **kuran
kişinin bir şey yapması gerekiyor mu**. Üçüncüsü en önemlisi ve genelde
yazılmayanı: bir sürüm notunu okuyan kişinin asıl sorusu "ben ne
yapacağım".

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

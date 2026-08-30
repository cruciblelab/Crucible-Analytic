-- Hangi şemanın kurulu olduğu, veritabanının kendi ağzından.
--
-- # Neden bu tablo var
--
-- Bugüne kadar hiçbir şey veritabanının kaçıncı sürümde olduğunu
-- bilmiyordu. `internal/panel/migrate.go` ayar göçüdür (TOML→DB), şema
-- göçü değil; binary'ler bilerek DDL çalıştırmaz; ve açılışta yapılan
-- tek kontrol `Ping`'dir.
--
-- Bunun bedeli ölçüldü (2026-08-30, gerçek TimescaleDB). Yeni binary
-- eski şemayla karşılaşınca:
--
--     NewWriter başarılı — Ping geçti, açılış sessiz
--     ilk yazma: column "asn_org" does not exist (SQLSTATE 42703)
--     written=0  failed=3   tabloda: 3'ün 0'ı
--
-- Süreç sağlıklı görünerek ayağa kalkıyor, ilk yazmada düşüyor, ve
-- satırlar kayboluyor. Bu tablo o sessizliği bitirmek için var.
--
-- # Neden iki alan, bir tane değil
--
-- İki farklı soru var ve bir alan ikisini birden cevaplayamıyor:
--
--   version      "binary veritabanından yeni mi?"  — sıralanabilir olmalı
--   fingerprint  "uygulanan şey gerçekten bu mu?"  — yalan söyleyememeli
--
-- Bir tam sayı sıralanabilir ama **yalan söyleyebilir**: biri şemayı
-- değiştirip sürümü yükseltmeyi unutabilir, ya da tersine. Bir özet
-- (hash) yalan söyleyemez ama **sıralanamaz**: iki farklı özetten
-- hangisinin yeni olduğu anlaşılmaz.
--
-- O yüzden ikisi de var, ve tam sayının dürüst kalmasını sağlayan şey
-- `internal/schemaver`'daki iki yönlü ayna testi: şema dosyaları
-- değiştiğinde özet değişir, özet sabitle uyuşmazsa test düşer ve
-- sürümün de yükseltilmesini ister.
--
-- # Ne olmamalı
--
-- Göç geçmişi değil. Bu tablo **tek satırdır** ve şu anki durumu söyler.
-- Satır başına bir göç tutan bir tasarım, göçlerin sırayla ve tam olarak
-- uygulandığını varsayar; bu projede şemalar idempotenttir ve toptan
-- yeniden uygulanır (18 `CREATE TABLE`'ın 18'i, 16 `ADD COLUMN`'un
-- 16'sı `IF NOT EXISTS`). Sorulan soru "hangi adımlar çalıştı" değil,
-- "şu an ne kurulu".

CREATE TABLE IF NOT EXISTS schema_version (
    -- Tek satır, ve bunu veritabanı zorluyor.
    --
    -- İki satırlı bir sürüm tablosu, hangisinin doğru olduğunu soran
    -- bir tablodur — ve o soruyu okuyan kod yanlış cevaplayabilir.
    -- CHECK, o durumu mümkün olmaktan çıkarıyor.
    id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- Sıralanabilir olan. "Binary 3 istiyor, veritabanı 2'de" cümlesi
    -- bundan kuruluyor, ve yönü söyleyen tek alan bu.
    version INTEGER NOT NULL,

    -- Yalan söyleyemeyen olan. Şema dosyalarının içeriğinden türetilir
    -- (`schemaver.FingerprintOf`), yani birinin elle tutmasına bağlı
    -- değil.
    fingerprint TEXT NOT NULL,

    -- Ne zaman ve ne tarafından. `applied_by` bugün `install.sh`, L3
    -- geldiğinde yükseltme uygulayıcısı olacak — ve o gün "bu şemayı
    -- kim uyguladı" sorusu, panelden cevaplanabilir bir soru olacak.
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_by TEXT NOT NULL DEFAULT ''
);

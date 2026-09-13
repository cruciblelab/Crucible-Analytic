#!/usr/bin/env python3
"""Ornek yapilandirma degismezinin mutasyonlari.

Her mutasyon: uygula -> dosyada gercekten durdugunu dogrula -> testi
kostur -> KIRMIZI bekle -> geri al. Go disi dosyalara dokunuldugu icin
`go test` her seferinde -count=1 ile cagriliyor.
"""
import subprocess, sys, shutil, re, os

ROOT = "/home/user/Crucible-Analytic"
os.chdir(ROOT)
TEST = ["go", "test", "-count=1", "-run",
        "TestEveryTopLevelSettingComesBeforeTheFirstTable|TestTheRegisteredConfigsCoverEveryExampleFile",
        "./internal/invariants/"]


def kos(etiket, dosya, degistir, bekle_metin):
    yedek = dosya + ".mut.bak"
    shutil.copy(dosya, yedek)
    try:
        s = open(dosya, encoding="utf-8").read()
        yeni = degistir(s)
        if yeni == s:
            print(f"{etiket}: MUTASYON UYGULANMADI (dosya degismedi)")
            return
        open(dosya, "w", encoding="utf-8").write(yeni)
        if bekle_metin not in open(dosya, encoding="utf-8").read():
            print(f"{etiket}: MUTASYON DOSYADA YOK")
            return
        r = subprocess.run(TEST, capture_output=True, text=True)
        if r.returncode == 0:
            print(f"{etiket}: SAG KALDI  <-- test tutmuyor")
        else:
            ilk = next((l.strip() for l in (r.stdout + r.stderr).splitlines()
                        if ".go:" in l and ":" in l), "")
            print(f"{etiket}: yakalandi  ({ilk[:100]})")
    finally:
        shutil.move(yedek, dosya)


def tasi_asagi(anahtar_satir, baslik):
    """Bir ayar satirini ilk tablo basliginin altina tasir."""
    def f(s):
        lines = s.split("\n")
        try:
            i = next(j for j, l in enumerate(lines) if l.strip() == anahtar_satir)
        except StopIteration:
            return s
        satir = lines.pop(i)
        k = next(j for j, l in enumerate(lines) if l.strip() == baslik)
        lines.insert(k + 1, satir)
        return "\n".join(lines)
    return f


kos("M5 bot_data_path yeniden [[tokens]] altina",
    "analytics-api.example.toml",
    tasi_asagi('# bot_data_path = "/var/lib/crucible-analytic/known_bots.json"', "[[tokens]]"),
    "[[tokens]]")

kos("M6 language yeniden [roles] altina",
    "panel.example.toml",
    tasi_asagi('language = "tr"', "[roles]"),
    "[roles]")

kos("M7 kayitli listeden panel dusuruldu",
    "internal/invariants/exampleconfig_test.go",
    lambda s: re.sub(r'\t"panel\.example\.toml": \{web\.Config\{\},\n[^\n]*\n', "", s, count=1),
    "exampleConfigs")

kos("M8 listede olmayan dosya kayitli",
    "internal/invariants/exampleconfig_test.go",
    lambda s: s.replace('"upgrader.example.toml": {applier.Config{},',
                        '"hayalet.example.toml": {applier.Config{},', 1),
    "hayalet.example.toml")

kos("M9 her alan tablo sayiliyor",
    "internal/invariants/exampleconfig_test.go",
    lambda s: s.replace("func isTable(t reflect.Type) bool {",
                        "func isTable(t reflect.Type) bool {\n\treturn true", 1),
    "\treturn true")

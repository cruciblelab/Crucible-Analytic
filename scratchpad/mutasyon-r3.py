#!/usr/bin/env python3
"""R2 mutasyonlari: temsilci parmak izi ve sayisi."""
import subprocess, shutil, os

ROOT = "/home/user/Crucible-Analytic"
os.chdir(ROOT)
os.environ.setdefault("CA_SUPERUSER_DSN",
                      "postgres://postgres@127.0.0.1:5432/analytics?sslmode=disable")


def kos(etiket, dosya, eski, yeni, desen, paket, tags=""):
    s = open(dosya, encoding="utf-8").read()
    if s.count(eski) != 1:
        print(f"{etiket}: MUTASYON UYGULANMADI ({s.count(eski)} eslesme)")
        return
    shutil.copy(dosya, dosya + ".mut.bak")
    try:
        open(dosya, "w", encoding="utf-8").write(s.replace(eski, yeni))
        if yeni not in open(dosya, encoding="utf-8").read():
            print(f"{etiket}: MUTASYON DOSYADA YOK")
            return
        if subprocess.run(["go", "build", "./..."], capture_output=True).returncode != 0:
            print(f"{etiket}: DERLENMEDI")
            return
        cmd = ["go", "test", "-count=1"]
        if tags:
            cmd += ["-tags", tags]
        cmd += ["-run", desen, paket]
        r = subprocess.run(cmd, capture_output=True, text=True)
        print(f"{etiket}: " + ("SAG KALDI  <-- test tutmuyor" if r.returncode == 0 else "yakalandi"))
    finally:
        shutil.move(dosya + ".mut.bak", dosya)


S = "internal/api/store.go"
P = "internal/panel/analytics/technical.go"
W = "internal/panel/web/technical.go"

kos("M10 bayrak tercihi kaldirildi", S,
    "ORDER BY is_known_bot_ja4 DESC, time DESC", "ORDER BY time DESC",
    "TheReportedFingerprintIsTheOneThatWasFlagged", "./internal/api/", "integration")

kos("M11 parmak izi sayisi hep 1", S,
    "count(DISTINCT ja4) FILTER (WHERE ja4 <> '')", "1",
    "TheReportedFingerprintIsTheOneThatWasFlagged", "./internal/api/", "integration")

kos("M12 panel cozucusu ja4_count okumuyor", P,
    'JA4Count        int       `json:"ja4_count"`', 'JA4Count        int       `json:"ja4_adet"`',
    "TestTheAddressRowsSurviveTheApisOwnJson", "./internal/panel/analytics/")

kos("M13 panel satiri sayiyi tasimayi biraktiv", P,
    "JA4: r.JA4, JA4Label: r.JA4Label, JA4Count: r.JA4Count,",
    "JA4: r.JA4, JA4Label: r.JA4Label,",
    "TestTheAddressRowsSurviveTheApisOwnJson", "./internal/panel/analytics/")

kos("M14 gorunum tipi sayiyi tasimiyor", W,
    "JA4Count: row.JA4Count,", "",
    "TestOneAddressWithTwoFingerprintsNamesTheFlaggedOne", "./internal/panel/web/", "integration")

kos("M15 sablon sayiyi hic cizmiyor", "internal/panel/ui/templates/pages/adres.html",
    '{{- if gt .JA4Count 1}}', '{{- if gt .JA4Count 99}}',
    "TestOneAddressWithTwoFingerprintsNamesTheFlaggedOne", "./internal/panel/web/", "integration")

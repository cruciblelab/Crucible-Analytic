#!/bin/sh
# R grubu mutasyonlari. Her biri: uygula -> dosyada gercekten durdugunu
# ve derlendigini dogrula -> testi kostur -> KIRMIZI bekle -> geri al.
#
# -count=1 go test'in *icinde*: onbellek bir mutasyonu yesil gosterebilir.
set -e
cd /home/user/Crucible-Analytic

uygula() { # dosya eski yeni
  cp "$1" "$1.mut.bak"
  python3 - "$1" "$2" "$3" <<'PY'
import sys
p, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(p).read()
if s.count(old) != 1:
    print(f"MUTASYON UYGULANMADI: {old!r} dosyada {s.count(old)} kez geciyor"); sys.exit(2)
open(p, 'w').write(s.replace(old, new))
PY
  grep -qF "$3" "$1" || { echo "MUTASYON DOSYADA YOK: $3"; exit 2; }
}
gerial() { mv "$1.mut.bak" "$1"; }

kos() { # etiket dosya eski yeni paket koşu-deseni [etiketler]
  etiket=$1; dosya=$2; eski=$3; yeni=$4; paket=$5; desen=$6; tags=$7
  uygula "$dosya" "$eski" "$yeni"
  if ! go build ./... >/dev/null 2>&1; then echo "$etiket: DERLENMEDI"; gerial "$dosya"; exit 2; fi
  if [ -n "$tags" ]; then
    out=$(go test -tags "$tags" -count=1 -run "$desen" "$paket" 2>&1) || true
  else
    out=$(go test -count=1 -run "$desen" "$paket" 2>&1) || true
  fi
  if printf '%s' "$out" | grep -q "^ok\|^PASS"; then
    echo "$etiket: SAG KALDI  <-- test tutmuyor"
    printf '%s\n' "$out" | tail -3
  else
    echo "$etiket: yakalandi"
  fi
  gerial "$dosya"
}

S=internal/scoring/scoring.go
A=internal/api/store.go

kos "M1 maxJA4Score 50->30 (birim)"  "$S" "maxJA4Score = 50" "maxJA4Score = 30" \
    ./internal/api/ "TestAKnownBotFingerprintAloneReachesTheDefaultCutoff" ""
kos "M2 maxASNScore 20->50 (birim)"  "$S" "maxASNScore = 20" "maxASNScore = 50" \
    ./internal/api/ "TestAKnownBotASNAloneStaysBelowTheDefaultCutoff" ""
kos "M3 DefaultBotScoreMin 50->60"   "$A" "DefaultBotScoreMin = 50" "DefaultBotScoreMin = 60" \
    ./internal/api/ "TestAKnownBotFingerprintAloneReachesTheDefaultCutoff" ""
kos "M4 maxJA4Score 50->30 (gercek veritabani)" "$S" "maxJA4Score = 50" "maxJA4Score = 30" \
    ./internal/api/ "TestStore_RealTimescaleDB_AKnownBotFingerprintAloneCountsAsABot" "integration"

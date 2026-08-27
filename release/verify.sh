#!/usr/bin/env bash
# Checks a staged or unpacked package for two things: that its checksums
# match, and that it carries nothing it must never carry.
#
#	release/verify.sh dist/crucible-analytic-v0.4.1
#
# Separate from build.sh on purpose, and the separation is not tidiness.
#
# A check that runs only inside the build can only ever see the files the
# build itself copied - so it verifies the script's own cp lines rather
# than the package. Pointed at a directory instead, it verifies whatever
# is actually there: a package somebody else built, a tarball unpacked
# from a download, or a stage that a later edit to build.sh has started
# sweeping something extra into.
#
# It is also the half a person can run without a Go toolchain, which is
# who receives a package rather than makes one.
set -euo pipefail

STAGE="${1:-}"
if [ -z "${STAGE}" ] || [ ! -d "${STAGE}" ]; then
  echo "usage: $0 <package directory>" >&2
  exit 2
fi

fail=0

# What must never be in a package. Patterns rather than a list of known
# files, because the risk is the file nobody thought to exclude.
while IFS= read -r -d '' f; do
  rel="${f#"${STAGE}/"}"
  case "${rel}" in
    ornek-yapilandirma/*.example.toml)
      # The examples are the point: they are what an operator copies.
      ;;
    *.log|logs/*|*/logs/*)
      echo "   collected data or logs: ${rel}" >&2; fail=1 ;;
    bot-data*|*/bot-data*|*botdata*.json)
      echo "   bot data (A10: ship the mechanism, not the data): ${rel}" >&2; fail=1 ;;
    *.toml)
      echo "   a real configuration file: ${rel}" >&2; fail=1 ;;
    *.key|*.pem|*.crt|*.p12|.env|*/.env)
      echo "   a credential or certificate: ${rel}" >&2; fail=1 ;;
    *.sql.gz|*dump*.sql)
      echo "   a database dump: ${rel}" >&2; fail=1 ;;
  esac
done < <(find "${STAGE}" -type f -print0)

if [ "${fail}" -ne 0 ]; then
  echo "   refusing to package" >&2
  exit 1
fi

# And the checksums, when the package carries them. Verified here rather
# than only written in build.sh, so the person who downloaded a package
# runs the same check the person who built it did.
if [ -f "${STAGE}/SHA256SUMS" ]; then
  ( cd "${STAGE}" && sha256sum --quiet -c SHA256SUMS ) || {
    echo "   checksums do not match the files" >&2
    exit 1
  }
fi

echo "   nothing forbidden, checksums match"

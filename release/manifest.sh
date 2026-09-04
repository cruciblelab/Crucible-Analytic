#!/bin/sh
# Publish the "which version is current" document.
#
# Run this once per release, after the packages are uploaded, with the
# same signing key that signed them. What it produces goes at the *base*
# of the release URL - beside the version directories, not inside one:
#
#   https://example.invalid/crucible/latest.txt
#   https://example.invalid/crucible/latest.txt.sig
#   https://example.invalid/crucible/v0.21.0/crucible-analytic-...tar.gz
#
# # Why this is a separate script and not part of build.sh
#
# build.sh makes one package for one architecture, and a release is
# several of those. The manifest says which version is current, which is
# a statement about the release as a whole and is only true once every
# package is actually uploaded. Publishing it from build.sh would name a
# version whose files might still be halfway through an upload, and the
# customers who noticed would be the ones whose install failed.
#
# # What reads it
#
# The upgrader, every six hours, verified against the public key in
# upgrader.toml. Nothing else: the panel never fetches this, because the
# panel does not hold the key. See internal/relupdate/manifest.go.
#
# Usage:
#   CA_RELEASE_KEY=... release/manifest.sh v0.21.0 [notes-url]
set -eu

VERSION="${1:-}"
NOTES="${2:-}"

if [ -z "${VERSION}" ]; then
  echo "usage: CA_RELEASE_KEY=... $0 <version> [notes-url]" >&2
  echo "  e.g. $0 v0.21.0 https://github.com/cruciblelab/Crucible-Analytic/blob/main/CHANGELOG.md" >&2
  exit 2
fi

if [ -z "${CA_RELEASE_KEY:-}" ]; then
  # Refused rather than producing an unsigned file. An unsigned manifest
  # is not a weaker manifest, it is one every deployment rejects - and a
  # publisher who uploaded it would have replaced a working document
  # with one that silently stops the check working.
  echo "CA_RELEASE_KEY is not set." >&2
  echo "The manifest has to be signed with the same key as the packages;" >&2
  echo "an unsigned one is refused by every deployment that fetches it." >&2
  exit 1
fi

OUT="${OUT:-dist}"
mkdir -p "${OUT}"

# RFC3339 in UTC, which is what internal/relupdate parses. date -u -Is
# differs between GNU and BSD in whether it prints an offset, so the
# format is written out rather than relied on.
RELEASED="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"

{
  echo "version: ${VERSION}"
  echo "released: ${RELEASED}"
  if [ -n "${NOTES}" ]; then
    echo "notes: ${NOTES}"
  fi
} > "${OUT}/latest.txt"

# The tool parses before it signs, so a version this project could never
# install is refused here rather than by every customer's upgrader.
go run ./cmd/releasesign -sign-manifest "${OUT}/latest.txt"

echo
echo "== upload both files to the base of your release URL:"
echo "   ${OUT}/latest.txt"
echo "   ${OUT}/latest.txt.sig"
echo
echo "   The base is the 'base_url' in upgrader.toml - the directory the"
echo "   version folders sit in, not one of the version folders."

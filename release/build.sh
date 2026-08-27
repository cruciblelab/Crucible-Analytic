#!/usr/bin/env bash
# Builds the release package: the thing a customer installs.
#
# Two properties matter more than convenience here.
#
# Reproducible. The same commit produces the same bytes, on any machine.
# That is what lets somebody verify that the binary they downloaded is the
# one this source builds - a claim nobody can check is a claim not worth
# making. -trimpath removes the build machine's paths from the binary,
# CGO_ENABLED=0 removes the host's C toolchain from the result, and the Go
# version is read from go.mod rather than from whatever is on PATH.
#
# Nothing in it that should not be. The package carries binaries, schemas,
# example configs, licences and the installation guide. It must never
# carry collected analytics, logs, real configuration, or the bot data
# (A10's rule: ship the mechanism, not the data). That list is checked by
# a machine at the end of this script, because "we were careful" is not a
# check.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo unknown)}"
OUT="${OUT:-dist}"
PKG="crucible-analytic-${VERSION}"
STAGE="${OUT}/${PKG}"

# GOOS/GOARCH default to this machine's, so a maintainer can build a
# package for their own server without arguments. Both are overridable
# for cross-building, which is what the release itself does.
GOOS_="${GOOS:-$(go env GOOS)}"
GOARCH_="${GOARCH:-$(go env GOARCH)}"

echo "== ${PKG} (${GOOS_}/${GOARCH_})"

# The toolchain from go.mod, not from PATH. A package built with a
# different Go than the one the project pins is not the same package, and
# the difference would only show as a behaviour change nobody could trace
# to its cause.
WANT_GO="$(awk '/^go /{print $2}' go.mod)"
HAVE_GO="$(go env GOVERSION | sed 's/^go//')"
if [ "${WANT_GO}" != "${HAVE_GO}" ]; then
  echo "   go.mod pins go${WANT_GO}, this is go${HAVE_GO}" >&2
  echo "   Go's own toolchain switching should have handled that; refusing rather than" >&2
  echo "   producing a package whose provenance nobody can reconstruct." >&2
  exit 1
fi

rm -rf "${STAGE}"
mkdir -p "${STAGE}/bin" "${STAGE}/schema" "${STAGE}/systemd" "${STAGE}/ornek-yapilandirma"

echo "== binaries"
# -buildvcs=false, and it is not an optimisation.
#
# Go embeds the git revision and a dirty flag into anything built from a
# working tree. That is useful on a development machine - internal/
# buildinfo falls back to it so an unstamped binary still knows where it
# came from - and it is fatal here.
#
# The point of a reproducible release is that somebody who downloaded the
# source can rebuild it and get the same bytes. That person has a
# tarball, not a repository, so their build embeds no VCS block and a
# maintainer's build embeds one. The binaries then differ for a reason
# that has nothing to do with the source, and the claim becomes
# untestable by precisely the person it exists for.
#
# Measured rather than reasoned: with VCS embedding on, a build from an
# exported copy of the same commit produced five different checksums,
# while -trimpath was doing its job in both. The release version comes
# from -X above, which needs no repository.
for b in collector beacon analytics-api panel devpass; do
  CGO_ENABLED=0 GOOS="${GOOS_}" GOARCH="${GOARCH_}" \
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${VERSION}" \
      -o "${STAGE}/bin/${b}" "./cmd/${b}"
done

echo "== schemas"
# Named individually rather than globbed: a schema that moves should
# break this script, not silently drop out of the package and fail at
# somebody's install.
cp internal/panel/schema.sql      "${STAGE}/schema/01-panel.sql"
cp internal/storage/schema.sql    "${STAGE}/schema/02-storage.sql"
cp internal/beacon/schema.sql     "${STAGE}/schema/03-beacon.sql"
cp internal/asnlookup/schema.sql  "${STAGE}/schema/04-asnlookup.sql"

echo "== example configuration"
for f in config.example.toml beacon.example.toml analytics-api.example.toml panel.example.toml; do
  cp "${f}" "${STAGE}/ornek-yapilandirma/"
done

echo "== systemd units"
cp release/systemd/*.service "${STAGE}/systemd/"

echo "== install and verify scripts"
# The package carries its own installer and its own verifier. A release
# whose install script lived only in the source repository would tell the
# operator who downloaded a tarball to go and clone something, which is
# the manual work F2 exists to remove.
mkdir -p "${STAGE}/release/sql"
install -m 0755 release/install.sh release/verify.sh "${STAGE}/release/"
install -m 0644 release/sql/*.sql "${STAGE}/release/sql/"

echo "== documents"
cp LICENSE NOTICE THIRD-PARTY.md KURULUM.md README.md "${STAGE}/"

echo "== checksums"
( cd "${STAGE}" && find . -type f ! -name SHA256SUMS -print0 \
    | sort -z | xargs -0 sha256sum > SHA256SUMS )

echo "== what must not be here"
# The check the scope note promises, in its own script so it can be run
# against a package nobody here built - see release/verify.sh.
./release/verify.sh "${STAGE}"

tar -C "${OUT}" -czf "${OUT}/${PKG}-${GOOS_}-${GOARCH_}.tar.gz" "${PKG}"
echo "== ${OUT}/${PKG}-${GOOS_}-${GOARCH_}.tar.gz"

#!/usr/bin/env sh
# Writes docker/.env, so the container path is two commands instead of an
# editor session.
#
#   ./docker/setup.sh --site musteri --backend site:443
#   ./docker/setup.sh                 # asks, when there is a terminal
#
# # Why this exists
#
# The container path was already short - `.env`, then `compose up` - and
# the `.env` step was the whole remaining friction: six values, three of
# which a person has to decide, one of which is a password nobody should
# type. Somebody who has never seen this repository has to read six
# comment blocks to fill in three fields.
#
# # What it will not do
#
# It does not run compose, and it does not build the image. Both are one
# command that the reader can see and understand; wrapping them would
# hide the two things they most need to recognise when something goes
# wrong.
#
# It also refuses to overwrite an existing .env. That file carries the
# database superuser's password, and a second run that quietly generated
# a new one would leave a stack whose database no longer accepts its own
# services - with no error until the next restart.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
EXAMPLE="${HERE}/.env.example"
TARGET="${CA_ENV_FILE:-${HERE}/.env}"

SITE=""
BACKEND=""
IMAGE=""
COLLECTOR_PORT=""
BEACON_PORT=""
FORCE=0

die() { printf 'setup: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --site) SITE="$2"; shift 2 ;;
    --backend) BACKEND="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
    --collector-port) COLLECTOR_PORT="$2"; shift 2 ;;
    --beacon-port) BEACON_PORT="$2"; shift 2 ;;
    --force) FORCE=1; shift ;;
    -h|--help) sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ -f "${EXAMPLE}" ] || die "${EXAMPLE} is missing; run this from a checkout"

if [ -f "${TARGET}" ] && [ "${FORCE}" -eq 0 ]; then
  die "${TARGET} already exists.

    It carries the database superuser's password. Writing a new one would
    leave a stack whose database no longer accepts its own services, and
    nothing would say so until the next restart.

    Edit it by hand, or pass --force if you mean to start over - which
    means the existing database volume is no longer usable either."
fi

# ask prints a prompt and reads an answer, defaulting when the reader
# presses enter. Only reached for values not given as flags, and only
# when a terminal is attached: a script run from another script must
# fail with a message naming the flag rather than block forever on a
# read nobody will answer.
ask() {
  _prompt="$1"
  _default="$2"
  _flag="$3"
  if [ ! -t 0 ]; then
    die "no ${_prompt} given and nothing to ask on (not a terminal). Pass ${_flag}."
  fi
  printf '%s [%s]: ' "${_prompt}" "${_default}" >&2
  read -r _answer || _answer=""
  if [ -z "${_answer}" ]; then
    printf '%s' "${_default}"
  else
    printf '%s' "${_answer}"
  fi
}

[ -n "${SITE}" ] || SITE="$(ask 'site id' 'musteri' '--site')"
[ -n "${BACKEND}" ] || BACKEND="$(ask 'the site this proxies to (host:port)' 'site:443' '--backend')"
[ -n "${IMAGE}" ] || IMAGE="crucible-analytic:dev"
[ -n "${COLLECTOR_PORT}" ] || COLLECTOR_PORT="443"
[ -n "${BEACON_PORT}" ] || BEACON_PORT="8081"

# The same character set install.sh enforces on a site id, and for the
# same reason: it reaches SQL as a value and a path segment, and it is
# irreversible once rows carry it.
case "${SITE}" in
  *[!A-Za-z0-9_-]*|"") die "site id ${SITE:-(empty)} may only carry letters, digits, _ and -" ;;
esac

# The password is generated, never asked. A password a person types is a
# password they reuse, and this one is never typed again by anybody: the
# services get their own roles and their own generated passwords from the
# init step.
if command -v openssl >/dev/null 2>&1; then
  PASSWORD="$(openssl rand -hex 24)"
elif [ -r /dev/urandom ]; then
  PASSWORD="$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')"
else
  die "no openssl and no readable /dev/urandom; cannot generate a password"
fi

umask 077
cat > "${TARGET}" <<ENV
# Written by docker/setup.sh. Every value here is documented in
# .env.example, which stays the place the reasoning lives.

POSTGRES_PASSWORD=${PASSWORD}
SITE_ID=${SITE}
SITE_BACKEND=${BACKEND}
COLLECTOR_PORT=${COLLECTOR_PORT}
BEACON_PORT=${BEACON_PORT}
CA_IMAGE=${IMAGE}
ENV

printf 'setup: wrote %s (mode 0600)\n' "${TARGET}"
cat <<NEXT

  site id       ${SITE}
  proxying to   ${BACKEND}
  image         ${IMAGE}
  ports         collector ${COLLECTOR_PORT}, beacon ${BEACON_PORT}

  The database password was generated and written. It is not printed:
  a password on a terminal is a password in a scrollback.

NEXT STEPS

  1. Build the image, if you have not:
       docker build -t ${IMAGE} .

  2. Bring the stack up:
       cd docker && docker compose up -d

  3. Create the one-time developer link and open it:
       docker compose run --rm panel-cli panel -dev-link -base-url https://panel.example.com

  KURULUM.md section 1.5 is the container path in full.

NEXT

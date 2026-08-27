#!/usr/bin/env bash
# Installs Crucible Analytic: database, four roles, privileges, secrets,
# configuration files and systemd units.
#
# KURULUM.md describes seventeen sections of manual work, and it is honest
# about it. The trouble is that the role separation - the panel cannot
# read analytics, the API cannot write - is half this system's security
# foundation and is currently typed by hand. Anything typed by hand can be
# typed wrong, and a wrong GRANT does not fail: it produces an
# installation that works, serves customers, and quietly does not have the
# property the design depends on.
#
# So the point of this script is not the ten minutes. It is that the
# privilege matrix is applied from one file and then verified by the
# database, and an installation that fails the verification does not
# finish.
#
#   sudo ./install.sh --domain analitik.example.com
#
# Idempotent where it can be: roles and databases that exist are kept,
# configuration files that exist are never overwritten (they hold secrets
# and site ids that cannot be regenerated). Re-running it re-applies the
# schemas and the grants, both of which are safe to repeat, and re-runs
# the verification.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "${HERE}")"

DB_NAME="${DB_NAME:-analytics}"
PREFIX="${PREFIX:-/opt/crucible-analytic}"
CONF_DIR="${CONF_DIR:-/etc/crucible-analytic}"
LOG_DIR="${LOG_DIR:-/var/log/crucible-analytic}"
STATE_DIR="${STATE_DIR:-/var/lib/crucible-analytic}"
RUN_AS="${RUN_AS:-crucible}"
DOMAIN=""
DRY_RUN=0
# PSQL is the superuser connection used to create roles and apply
# schemas. Overridable so the tests can point it at a database that is
# not on a unix socket as postgres.
PSQL="${PSQL:-psql -v ON_ERROR_STOP=1 -q}"
SUPERUSER_DSN="${SUPERUSER_DSN:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --domain) DOMAIN="$2"; shift 2 ;;
    --db) DB_NAME="$2"; shift 2 ;;
    --prefix) PREFIX="$2"; shift 2 ;;
    --conf) CONF_DIR="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help)
      sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

say() { printf '== %s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

psql_super() {
  if [ -n "${SUPERUSER_DSN}" ]; then
    ${PSQL} "${SUPERUSER_DSN}" "$@"
  else
    sudo -u postgres ${PSQL} "$@"
  fi
}

psql_db() {
  if [ -n "${SUPERUSER_DSN}" ]; then
    ${PSQL} "${SUPERUSER_DSN}" "$@"
  else
    sudo -u postgres ${PSQL} -d "${DB_NAME}" "$@"
  fi
}

# A password with no shell metacharacters, so it can be substituted into
# SQL and written into a TOML file without quoting surprises.
#
# od with -N rather than `tr -dc ... | head -c 32`, and this is the same
# trap as the role check above rather than a style preference: `head`
# exits as soon as it has its 32 bytes, `tr` dies of SIGPIPE, and under
# `set -o pipefail` the whole substitution reports 141. With `set -e`
# that aborts the script - which it did, silently, in the middle of
# creating the second role.
#
# Twice in one script from the same cause. Any pipe whose reader stops
# early is a failed pipeline here.
newsecret() {
  od -An -tx1 -N24 /dev/urandom | tr -d " \n"
}

# ---------------------------------------------------------------- checks

say "preflight"
command -v psql >/dev/null || die "psql is not installed"
[ -f "${HERE}/sql/grants.sql" ] || die "release/sql/grants.sql is missing; run this from the package"

if [ "${DRY_RUN}" -eq 1 ]; then
  say "dry run: nothing will be changed"
fi

# ------------------------------------------------------------- database

say "database ${DB_NAME}"
if [ "${DRY_RUN}" -eq 0 ]; then
  # Tested with [ -n "$(...)" ] rather than `| grep -q`.
  #
  # Under `set -o pipefail`, a `grep -q` that matches exits immediately,
  # closes the pipe, and psql dies of SIGPIPE - so the pipeline reports
  # 141 and the script stops on the *success* case. It did exactly that,
  # silently, right after printing that a role already existed.
  if [ -z "$(psql_super -tAc "SELECT 1 FROM pg_database WHERE datname = '${DB_NAME}'")" ]; then
    psql_super -c "CREATE DATABASE ${DB_NAME}"
  fi
  psql_db -c "CREATE EXTENSION IF NOT EXISTS timescaledb"
fi

# ---------------------------------------------------------------- roles

say "four roles"
#
# Each role checked and created on its own, not "if none exist, create
# all four".
#
# PostgreSQL roles are cluster-wide rather than per-database, so a
# machine that already runs something with a role called `collector` -
# or a previous install that got half way - has some of these and not
# others. The all-or-nothing version skipped creating the missing three
# because one existed, and the grants then failed on a role that was
# never made. Found by running it against exactly that: a development
# cluster where `collector` was already there.
#
# Existing roles keep their passwords. Rotating them here would break a
# running installation's configuration files, which this script does not
# read and must not invalidate.
declare -A ROLE_PW=()
if [ "${DRY_RUN}" -eq 0 ]; then
  for role in collector beacon_writer analytics_reader panel_user; do
    if [ -n "$(psql_db -tAc "SELECT 1 FROM pg_roles WHERE rolname = '${role}'")" ]; then
      say "   ${role} already exists, keeping its password"
      continue
    fi
    pw="$(newsecret)"
    ROLE_PW["${role}"]="${pw}"
    # Quoted through psql's own :'name' substitution rather than pasted
    # into the statement, so a generated password can never be read as
    # SQL. The alphabet above already excludes quotes; this is the belt
    # that does not depend on that staying true.
    #
    # Fed on stdin rather than with -c: psql does not interpolate
    # variables into a -c string, it sends it literally, and the server
    # answered `syntax error at or near ":"`. The substitution only
    # happens for -f and for standard input.
    psql_db -v "pw=${pw}" <<SQL
CREATE ROLE ${role} LOGIN PASSWORD :'pw';
SQL
  done

  # Connect and schema usage for all four, whether they were made now or
  # were already here: an existing role from somewhere else has no reason
  # to already hold these.
  psql_db -c "GRANT CONNECT ON DATABASE ${DB_NAME}
      TO collector, beacon_writer, analytics_reader, panel_user"
  psql_db -c "GRANT USAGE ON SCHEMA public
      TO collector, beacon_writer, analytics_reader, panel_user"
fi

# --------------------------------------------------------------- schemas

say "schemas"
if [ "${DRY_RUN}" -eq 0 ]; then
  # From the package when there is one, from the source tree otherwise, so
  # this script works in both places without a second copy of the list.
  if [ -d "${HERE}/../schema" ]; then
    for f in "${HERE}"/../schema/*.sql; do psql_db -f "$f"; done
  else
    for f in internal/panel/schema.sql internal/storage/schema.sql \
             internal/beacon/schema.sql internal/asnlookup/schema.sql; do
      psql_db -f "${ROOT}/${f}"
    done
  fi
fi

# ------------------------------------------------------------ privileges

say "privileges"
if [ "${DRY_RUN}" -eq 0 ]; then
  psql_db -f "${HERE}/sql/grants.sql"
fi

# The half that matters. A grant block that ran without error proves the
# statements were accepted; it says nothing about what was *not* granted,
# and "the panel cannot read analytics" is the property the design rests
# on. Asked of the database, and a single f stops the install.
say "verifying the privilege matrix"
if [ "${DRY_RUN}" -eq 0 ]; then
  failed="$(psql_db -tAF'|' -f "${HERE}/sql/verify.sql" | awk -F'|' '$2 != "t" {print $1}')"
  if [ -n "${failed}" ]; then
    printf 'install: the privilege matrix is wrong:\n' >&2
    # One line per assertion. `printf '%s\n' ${failed}` unquoted splits on
    # every space, so the first failure printed as six lines of one word
    # each - which is how a clear message becomes an unreadable one at
    # exactly the moment somebody needs to read it.
    while IFS= read -r line; do
      printf '   - %s\n' "${line}" >&2
    done <<< "${failed}"
    die "refusing to finish an installation whose role separation is not what it claims"
  fi
  say "   every assertion holds, including the negative ones"
fi

# ---------------------------------------------------------------- secrets

say "configuration and the shared key"
if [ "${DRY_RUN}" -eq 0 ]; then
  mkdir -p "${CONF_DIR}"
  chmod 750 "${CONF_DIR}"

  # Configuration files, copied from the examples and never overwritten.
  #
  # A config file that exists holds secrets and a site id that cannot be
  # regenerated: the site id in particular is the thing KURULUM.md calls
  # irreversible, because every stored row is keyed by it. Re-running this
  # script must not touch one.
  copy_example() {
    src="$1"; dst="${CONF_DIR}/$2"
    [ -f "${dst}" ] && return 0
    for candidate in "${ROOT}/$1" "${HERE}/../ornek-yapilandirma/$1"; do
      if [ -f "${candidate}" ]; then
        install -m 0640 "${candidate}" "${dst}"
        say "   wrote ${dst}"
        return 0
      fi
    done
    say "   ${1} not found; skipping ${dst}"
  }
  copy_example config.example.toml        collector.toml
  copy_example beacon.example.toml        beacon.toml
  copy_example analytics-api.example.toml analytics-api.toml
  copy_example panel.example.toml         panel.toml

  # ---- the IP token key ----
  #
  # This is the piece of F2 the plan singles out, and the reason this
  # script owns it rather than preflight.
  #
  # The key goes into two separate configuration files, and the two
  # services never read each other's - they must not. Preflight can check
  # the key is *present*; it cannot check the two are the *same*, because
  # it only ever sees one of them.
  #
  # Different keys do not fail. They produce an installation where the
  # crossover join matches nothing, silently, with no message saying why -
  # so the one view that proves this product's whole claim reports zero
  # and looks like a quiet week.
  #
  # Read first, written second. The first version of this generated a key,
  # wrote it to both files, and then compared them - which compares the
  # script's own output with itself and can never disagree. A hand-edited
  # beacon.toml with a mistyped key passed it without a word. The failure
  # this exists to catch is exactly a key copied by hand into the second
  # file, so the existing values have to be read before anything
  # overwrites them.
  read_key() {
    f="${CONF_DIR}/$1"
    [ -f "${f}" ] || return 0
    sed -nE 's|^[[:space:]]*ip_hash_key[[:space:]]*=[[:space:]]*"(.+)"|\1|p' "${f}" | head -n1
  }
  digest() { printf '%s' "$1" | sha256sum | cut -c1-16; }

  COLLECTOR_KEY="$(read_key collector.toml)"
  BEACON_KEY="$(read_key beacon.toml)"

  if [ -n "${COLLECTOR_KEY}" ] && [ -n "${BEACON_KEY}" ] \
     && [ "${COLLECTOR_KEY}" != "${BEACON_KEY}" ]; then
    printf 'install: the collector and the beacon carry different ip_hash_key values\n' >&2
    printf '   collector.toml  %s\n   beacon.toml     %s\n' \
      "$(digest "${COLLECTOR_KEY}")" "$(digest "${BEACON_KEY}")" >&2
    printf '   (hashes, not the keys - a mismatch is worth reporting, the secret is not)\n' >&2
    printf '   The crossover join would match nothing, silently, and the view that proves\n' >&2
    printf '   this product works would read zero and look like a quiet week.\n' >&2
    die "refusing to finish; make them the same, or delete one and re-run"
  fi

  # Whichever exists wins, so a re-run never rotates a key that is
  # already in use: doing that would break the pseudonyms of every row
  # already stored.
  IP_KEY="${COLLECTOR_KEY:-${BEACON_KEY}}"
  if [ -z "${IP_KEY}" ] && [ -f "${CONF_DIR}/ip_hash_key" ]; then
    IP_KEY="$(cat "${CONF_DIR}/ip_hash_key")"
  fi
  if [ -z "${IP_KEY}" ]; then
    IP_KEY="$(newsecret)$(newsecret)"
    say "   generated an ip_hash_key"
  else
    say "   reusing the ip_hash_key already in place"
  fi
  printf '%s' "${IP_KEY}" > "${CONF_DIR}/ip_hash_key"
  chmod 600 "${CONF_DIR}/ip_hash_key"

  # Written only where it is missing.
  write_key() {
    f="${CONF_DIR}/$1"
    [ -f "${f}" ] || return 0
    [ -n "$(read_key "$1")" ] && return 0
    if grep -qE '^[[:space:]]*#[[:space:]]*ip_hash_key[[:space:]]*=' "${f}"; then
      sed -i -E "0,/^[[:space:]]*#[[:space:]]*ip_hash_key[[:space:]]*=.*/s||ip_hash_key = \"${IP_KEY}\"|" "${f}"
    else
      printf '\n[privacy]\nip_hash_key = "%s"\n' "${IP_KEY}" >> "${f}"
    fi
    say "   wrote the key into $1"
  }
  write_key collector.toml
  write_key beacon.toml

  # Read back from the files, not from the variable: the question is what
  # the two files ended up holding, and only the files can answer it.
  a="$(digest "$(read_key collector.toml)")"
  b="$(digest "$(read_key beacon.toml)")"
  if [ "${a}" != "${b}" ]; then
    die "the two files still disagree after writing (${a} vs ${b})"
  fi
  say "   ip_hash_key matches in both files (${a})"
fi

# ----------------------------------------------------------------- units

say "systemd units"
if [ "${DRY_RUN}" -eq 0 ] && [ -d /etc/systemd/system ]; then
  for unit in "${HERE}"/systemd/*.service; do
    [ -f "${unit}" ] || continue
    install -m 0644 "${unit}" /etc/systemd/system/
  done
  id -u "${RUN_AS}" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "${RUN_AS}"
  mkdir -p "${LOG_DIR}" "${STATE_DIR}"
  chown "${RUN_AS}:${RUN_AS}" "${LOG_DIR}" "${STATE_DIR}"
  systemctl daemon-reload 2>/dev/null || true
fi

# ------------------------------------------------------------------- TLS

say "TLS"
cat <<TLS
   Not obtained here, deliberately. A certificate needs a domain that
   already resolves to this machine and an ACME challenge this script
   cannot complete on its own, so doing half of it would leave a
   half-configured reverse proxy nobody could debug.

   ${DOMAIN:+For ${DOMAIN}: }See KURULUM.md section 8, which has the
   reverse-proxy configuration and the certbot command.
TLS

say "done"
if [ "${#ROLE_PW[@]}" -gt 0 ]; then
  cat <<'SECRETS'

Role passwords, generated once and shown once. Put them in the matching
configuration files before starting anything. Roles that already existed
are not listed: their passwords were left alone, because this script does
not read the configuration files that hold them.

SECRETS
  for role in collector beacon_writer analytics_reader panel_user; do
    [ -n "${ROLE_PW[${role}]:-}" ] && printf '  %-18s %s\n' "${role}" "${ROLE_PW[${role}]}"
  done
  echo
fi

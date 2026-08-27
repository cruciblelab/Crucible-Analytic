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

# dsn_for_db returns a DSN pointed at a named database.
#
# This exists because psql_db used to pass SUPERUSER_DSN through
# untouched, which meant --db was silently ignored whenever a DSN was
# given. Measured, with SUPERUSER_DSN naming `postgres` and --db
# analytics: the script created an empty `analytics` and then applied
# every schema, every grant and every REVOKE to the maintenance
# database. The install reported success, because verify.sql asked
# `current_database()` - and so it was checking the database it had
# just hardened by accident.
#
# It survived a suite that runs install.sh a dozen times because every
# one of those runs passed a DSN that already named the target
# database: the single arrangement in which the two disagree about
# nothing. The `sudo -u postgres` path below was always correct, which
# is why the bug never appeared on a real server - only on the path a
# container uses, where the database is reached over TCP.
#
# The override is a trailing one in both DSN forms, measured against
# libpq rather than assumed. A URI takes ?dbname= - or &dbname= when it
# already carries a query - and libpq lets that beat the path. A
# keyword string takes a second dbname=, where the last wins.
dsn_for_db() {
  case "$1" in
    postgres://*|postgresql://*)
      case "$1" in
        *\?*) printf '%s&dbname=%s' "$1" "$2" ;;
        *)    printf '%s?dbname=%s' "$1" "$2" ;;
      esac ;;
    *) printf '%s dbname=%s' "$1" "$2" ;;
  esac
}

psql_db() {
  if [ -n "${SUPERUSER_DSN}" ]; then
    ${PSQL} "$(dsn_for_db "${SUPERUSER_DSN}" "${DB_NAME}")" "$@"
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
[ -f "${HERE}/sql/harden.sql" ] || die "release/sql/harden.sql is missing; run this from the package"

# The database name reaches SQL as a bare identifier - CREATE DATABASE
# takes no bound parameter - so it is checked here rather than quoted at
# each of its uses. Whoever runs this script is already root, so this is
# not a privilege boundary; it is the difference between a typo that
# stops the install and a typo that runs as SQL. The same check keeps
# the name safe to append to a DSN, which dsn_for_db does.
case "${DB_NAME}" in
  *[!A-Za-z0-9_]*|"") die "database name ${DB_NAME:-(empty)} is not a bare identifier (letters, digits and _)" ;;
esac

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
             internal/beacon/schema.sql internal/asnlookup/schema.sql \
             internal/heartbeat/schema.sql internal/retention/schema.sql; do
      psql_db -f "${ROOT}/${f}"
    done
  fi
fi

# ------------------------------------------------------------ privileges

say "privileges"
if [ "${DRY_RUN}" -eq 0 ]; then
  psql_db -f "${HERE}/sql/grants.sql"
fi

# The privileges nobody granted.
#
# grants.sql says what each role may do. harden.sql closes what
# PostgreSQL and TimescaleDB switched on without being asked - telemetry,
# CONNECT for every role on the cluster, and the ability for any role at
# all to schedule a background job that outlives the process which made
# it. None of those shows up as a missing GRANT, so none of them is
# visible in a privilege listing that reads perfectly.
#
# Applied on every run: each statement is idempotent, and an upgrade that
# reinstalls the extension puts the defaults back.
say "closing the defaults nobody chose"
if [ "${DRY_RUN}" -eq 0 ]; then
  [ -f "${HERE}/sql/harden.sql" ] || die "release/sql/harden.sql is missing; run this from the package"
  psql_db -v dbname="${DB_NAME}" -f "${HERE}/sql/harden.sql"
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
  declare -A FRESH_CONF=()
  copy_example() {
    src="$1"; dst="${CONF_DIR}/$2"
    [ -f "${dst}" ] && return 0
    for candidate in "${ROOT}/$1" "${HERE}/../ornek-yapilandirma/$1"; do
      if [ -f "${candidate}" ]; then
        install -m 0640 "${candidate}" "${dst}"
        # Recorded so the password writer below can tell a file this run
        # created - every value in it is still an example - from one an
        # operator has been editing.
        FRESH_CONF["$2"]=1
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

  # ---- the four database passwords ----
  #
  # The script generated these a hundred lines ago and used to print
  # them with "put them in the matching configuration files", leaving
  # four passwords to be retyped into four files by hand - in a script
  # that goes to considerable trouble, immediately below, to stop
  # exactly that happening with one key.
  #
  # It is not merely tedious. An unattended install - a container image
  # built per customer, which is how this product is meant to be
  # deployed - has nobody to read the screen, so it produced four
  # configuration files that could not connect and four services that
  # would not start. Found by installing the package and trying to run
  # it.
  #
  # Written only into a file this run created, and only for a role this
  # run created. Both halves matter. A role that already existed keeps
  # its password, which this script does not know, so there is nothing
  # to write; and a configuration file that was already here may hold a
  # working password that this script must not overwrite.
  #
  # Only the password and the database name are replaced. The host is
  # left as the example wrote it, because an operator pointing at a
  # database on another machine edits that line and nothing here should
  # undo it.
  write_role_password() {
    role="$1"; file="$2"; key="$3"
    [ -n "${ROLE_PW[${role}]:-}" ] || return 0
    [ -n "${FRESH_CONF[${file}]:-}" ] || return 0
    f="${CONF_DIR}/${file}"
    [ -f "${f}" ] || return 0

    # newsecret produces hex, so the password cannot carry a character
    # sed would read as syntax. That is a property of the generator
    # rather than of this line, which is why it is said here: a generator
    # that grew a wider alphabet would break this quietly.
    sed -i -E "s|^([[:space:]]*${key}[[:space:]]*=[[:space:]]*\"postgres://${role}:)[^@]*(@[^/\"]*/)[^\"]*(\")|\1${ROLE_PW[${role}]}\2${DB_NAME}\3|" "${f}"
    say "   wrote ${role}'s password into ${file}"
  }
  write_role_password collector        collector.toml     timescale_dsn
  write_role_password beacon_writer    beacon.toml        timescale_dsn
  write_role_password analytics_reader analytics-api.toml timescale_dsn
  write_role_password panel_user       panel.toml         panel_dsn

  # And the check that makes the writing worth anything: use what the
  # file now says, as the service will.
  #
  # Read back out of the file rather than taken from the variable - the
  # question is what the service is going to use, and only the file can
  # answer it.
  #
  # Three questions, and the third is deliberately not "did the password
  # work". Measured on the development cluster: pg_hba.conf there says
  # `trust`, so psql connects with any password at all and a check that
  # only tried to connect would have reported four correct passwords
  # while the files held four wrong ones. So the password is compared
  # against what was generated - a text question, true on every cluster -
  # and the connection is used for the thing it can actually answer:
  # that the DSN lands as the right role in the right database, which is
  # what catches a mangled host or a database name left at the example's.
  check_role_dsn() {
    role="$1"; file="$2"; key="$3"
    [ -n "${ROLE_PW[${role}]:-}" ] || return 0
    [ -n "${FRESH_CONF[${file}]:-}" ] || return 0
    dsn="$(sed -nE "s|^[[:space:]]*${key}[[:space:]]*=[[:space:]]*\"(.+)\"|\1|p" "${CONF_DIR}/${file}" | head -n1)"
    [ -n "${dsn}" ] || die "${file} has no ${key} after writing one"

    case "${dsn}" in
      *":${ROLE_PW[${role}]}@"*) ;;
      *) die "${file}'s ${key} does not carry the password this install generated for ${role}" ;;
    esac

    where="$(${PSQL} "${dsn}" -tAc "SELECT current_user || '@' || current_database()" 2>/dev/null || true)"
    [ "${where}" = "${role}@${DB_NAME}" ] \
      || die "${file}'s ${key} reaches ${where:-nothing}, not ${role}@${DB_NAME}"
  }
  check_role_dsn collector        collector.toml     timescale_dsn
  check_role_dsn beacon_writer    beacon.toml        timescale_dsn
  check_role_dsn analytics_reader analytics-api.toml timescale_dsn
  check_role_dsn panel_user       panel.toml         panel_dsn

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

  # ---- the panel's secret_key ----
  #
  # A different problem from the one above, and a simpler one: this key
  # lives in one file, so there is nothing to match. What it needs
  # instead is never to change.
  #
  # It encrypts the stored SMTP password. Rotating it on a re-run would
  # not break the panel and would not lose analytics - it would make the
  # saved mail password stop opening, so invitations quietly stop being
  # delivered while the panel reports itself healthy. Written only when
  # absent, for that reason and no other.
  read_secret_key() {
    f="${CONF_DIR}/panel.toml"
    [ -f "${f}" ] || return 0
    sed -nE 's|^[[:space:]]*secret_key[[:space:]]*=[[:space:]]*"(.+)"|\1|p' "${f}" | head -n1
  }

  if [ -f "${CONF_DIR}/panel.toml" ]; then
    if [ -n "$(read_secret_key)" ]; then
      say "   reusing the panel secret_key already in place"
    else
      # 32 bytes as 64 hex characters: newsecret gives 24, so two of
      # them and a trim. Written to a temporary variable first so a
      # failure here cannot leave a half-written key in the file.
      SECRET_KEY="$(newsecret)$(newsecret)"
      SECRET_KEY="$(printf '%s' "${SECRET_KEY}" | cut -c1-64)"
      if [ "${#SECRET_KEY}" -ne 64 ]; then
        die "generated a ${#SECRET_KEY}-character secret_key, expected 64"
      fi
      if grep -qE '^[[:space:]]*#[[:space:]]*secret_key[[:space:]]*=' "${CONF_DIR}/panel.toml"; then
        sed -i -E "0,/^[[:space:]]*#[[:space:]]*secret_key[[:space:]]*=.*/s||secret_key = \"${SECRET_KEY}\"|" "${CONF_DIR}/panel.toml"
      else
        printf '\nsecret_key = "%s"\n' "${SECRET_KEY}" >> "${CONF_DIR}/panel.toml"
      fi
      # Read back rather than trusting the write. sed -i against a
      # commented line is exactly the kind of substitution that silently
      # matches nothing.
      if [ -z "$(read_secret_key)" ]; then
        die "wrote a secret_key into panel.toml and could not read it back"
      fi
      say "   generated the panel secret_key ($(digest "$(read_secret_key)"))"
    fi
  fi
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

# What is left for a person to do about the four passwords, which is
# now usually nothing.
#
# Split into the two cases rather than printing all four, because they
# want opposite things. A password already written into a fresh config
# file needs no action, and printing it puts a working credential into a
# terminal buffer, a scrollback and whatever captured this script's
# output for no benefit. A password generated for a role whose config
# file was already here does need a person, because this script will not
# overwrite a file it did not create.
declare -a NEEDS_HAND=()
if [ "${DRY_RUN}" -eq 0 ] && [ "${#ROLE_PW[@]}" -gt 0 ]; then
  for role in collector beacon_writer analytics_reader panel_user; do
    [ -n "${ROLE_PW[${role}]:-}" ] || continue
    case "${role}" in
      collector)        file=collector.toml ;;
      beacon_writer)    file=beacon.toml ;;
      analytics_reader) file=analytics-api.toml ;;
      panel_user)       file=panel.toml ;;
    esac
    [ -n "${FRESH_CONF[${file}]:-}" ] || NEEDS_HAND+=("${role} ${file}")
  done
fi

if [ "${#NEEDS_HAND[@]}" -gt 0 ]; then
  cat <<'SECRETS'

These roles were created now, and their configuration files were already
here - so this script did not touch them. It never overwrites a file it
did not write, because that file may hold a working password, a site id
that cannot be regenerated, or both.

Put each password into the DSN in the file beside it, and start nothing
before you have. Shown once; only the hash is kept.

SECRETS
  for entry in "${NEEDS_HAND[@]}"; do
    set -- ${entry}
    printf '  %-18s %-20s %s\n' "$1" "$2" "${ROLE_PW[$1]}"
  done
  echo
elif [ "${#ROLE_PW[@]}" -gt 0 ]; then
  cat <<'SECRETS'

The four database passwords were generated and written into the
configuration files. Each file was read back and checked: it carries the
password this run generated, and its DSN reaches that role in this
database.

They are not printed. They are in the files, which are mode 0640, and a
password on a terminal is a password in a scrollback.

SECRETS
fi

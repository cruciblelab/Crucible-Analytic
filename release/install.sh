#!/usr/bin/env bash
# Installs Crucible Analytic: database, five roles, privileges, secrets,
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
# Where the executables are read from, which is not always beside this
# script: KURULUM.md section 3 recommends building on another machine and
# copying, and a package unpacked next to a checkout has two bin/
# directories. Defaulting to ${ROOT}/bin covers both the release package
# and a repository that has been built.
BIN_DIR="${BIN_DIR:-${ROOT}/bin}"
CONF_DIR="${CONF_DIR:-/etc/crucible-analytic}"
LOG_DIR="${LOG_DIR:-/var/log/crucible-analytic}"
STATE_DIR="${STATE_DIR:-/var/lib/crucible-analytic}"
RUN_AS="${RUN_AS:-crucible}"
# RUN_AS_UPGRADER is the sixth binary's own account, and it is separate
# from RUN_AS on purpose.
#
# upgrader.toml carries the only DSN in this deployment that can run DDL.
# The four services run as ${RUN_AS}; the panel is one of them. If the
# upgrader ran as ${RUN_AS} too, then upgrader.toml would have to be
# readable by ${RUN_AS}, and the panel could read the credential that
# rewrites its own database - undoing with one file permission the whole
# reason there are five database roles.
RUN_AS_UPGRADER="${RUN_AS_UPGRADER:-crucible-upgrader}"
# SYSTEMD_DIR is where the unit files go.
#
# The last member of the PREFIX / CONF_DIR / LOG_DIR / STATE_DIR family,
# and it was missing. Every other path this script writes to could be
# redirected, which is what lets it run into a container image, a chroot
# or a scratch directory - and the one place it could not was the one
# place nothing could therefore test, so the whole systemd stage went
# unexercised. install_test.go's own comment recorded that its
# --no-systemd was "load-bearing"; what it was bearing was the absence of
# this variable.
SYSTEMD_DIR="${SYSTEMD_DIR:-/etc/systemd/system}"
# DB_HOST is the host:port the *services* reach the database at, which is
# not always the one this script reaches it at.
#
# Empty means "leave the example's host alone", which is right on a
# server: the database is on localhost, and an operator pointing at
# another machine edits that line themselves. It is wrong in a container,
# where localhost is the container and the database is a service name -
# and the check that connects with each freshly written DSN found that
# out by failing, correctly, before anything started.
DB_HOST="${DB_HOST:-}"
DOMAIN=""
# PROFILE is the resource profile to write into the configs, empty for
# "leave the examples alone".
#
# Empty is the default rather than a named profile, and that is the same
# judgement as everywhere else in this product: do not decide for
# somebody what they did not ask. What changed is that the install now
# *says* which one it ended up in, which is the half that was missing -
# the examples ship asn_lookup off, so an install nobody steered lands on
# Hafif and used to say nothing about it.
#
# The ids are internal/profile's. release/install_test.go checks this
# list against that package so the two cannot drift.
# Overridable from the environment like DB_NAME and SUPERUSER_DSN above,
# which is how the release suite drives it: the flag is what a person
# uses, the variable is what a test sets without rebuilding the command
# line for one case.
PROFILE="${PROFILE:-}"
DRY_RUN=0
# WANT_SYSTEMD is decided in preflight rather than at the systemd stage,
# and --no-systemd forces it off. See the preflight block for why the
# decision has to happen before the database is touched.
WANT_SYSTEMD=1
# PSQL is the superuser connection used to create roles and apply
# schemas. Overridable so the tests can point it at a database that is
# not on a unix socket as postgres.
PSQL="${PSQL:-psql -v ON_ERROR_STOP=1 -q}"
SUPERUSER_DSN="${SUPERUSER_DSN:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --domain) DOMAIN="$2"; shift 2 ;;
    --db) DB_NAME="$2"; shift 2 ;;
    --profile) PROFILE="$2"; shift 2 ;;
    --prefix) PREFIX="$2"; shift 2 ;;
    --bin-dir) BIN_DIR="$2"; shift 2 ;;
    --conf) CONF_DIR="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --no-systemd) WANT_SYSTEMD=0; shift ;;
    -h|--help)
      sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

say() { printf '== %s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

# ensure_mode <mode> <dir>... sets a directory's mode, and does nothing
# when it is already that.
#
# # Why not just chmod
#
# chmod fails with EPERM for anybody who does not own the directory,
# even when it would have changed nothing. That is the ordinary case in
# a container: the image bakes /opt/crucible-analytic in, root-owned and
# already 0755, and the init container runs as somebody else. `set -e`
# then ends the install over a call that had no work to do.
#
# It broke the nightly container run for two nights. The message named
# the symptom - "Operation not permitted" - and not the fact that the
# permission was already correct.
#
# So the mode is read first. A directory that is already right is left
# alone whoever owns it, and one that is wrong and cannot be fixed stops
# the install with what it is, what it needs, and why - because that one
# really does produce a deployment where the services cannot reach their
# own binaries.
#
# stat missing entirely leaves `have` empty, which never matches, so the
# chmod is attempted exactly as before.
ensure_mode() {
  _want="$1"; shift
  for _dir in "$@"; do
    _have="$(stat -c %a "${_dir}" 2>/dev/null || printf '')"
    # 0755 and 755 are the same mode written two ways.
    case "${_want}" in 0*) _short="${_want#0}" ;; *) _short="${_want}" ;; esac
    if [ "${_have}" = "${_want}" ] || [ "${_have}" = "${_short}" ]; then
      continue
    fi
    chmod "${_want}" "${_dir}" 2>/dev/null || die \
      "${_dir} is mode ${_have:-unknown} and has to be ${_want}, and this account \
cannot change it. The services read their binaries through this directory, so an \
install that carried on here would leave them unable to start. Either run the \
install as the owner of ${_dir}, or set the mode yourself and run it again."
  done
}

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

# The schema version is stamped from a binary, so one of the two ways of
# reading it has to be available. Checked here for the same reason as the
# systemd rule below: found at its own stage, it stops the install after
# the schemas are already applied.
if [ "${DRY_RUN}" -eq 0 ] && [ ! -x "${HERE}/../bin/panel" ] && ! command -v go >/dev/null; then
  die "the schema version is read from the panel binary, and neither
    ${HERE}/../bin/panel nor a Go toolchain is here. Install from the
    release package, or run this from a source tree with Go available."
fi

# Whether systemd units will be installed - decided here, before the
# database is touched.
#
# The old guard asked `[ -d /etc/systemd/system ]`, which is a different
# question from the one that matters: the directory exists on any Ubuntu
# machine, including a CI runner where this script is not root. So the
# install created four roles, applied seven schemas, wrote four config
# files and generated three secrets, and *then* died on a permission
# error - leaving a half-installed machine and a message about systemd
# that says nothing about what to do.
#
# Measured: this is what every CI run had been doing since the systemd
# stage was added. The merge gate was red on every push and the reason
# was one line at the very end.
if [ "${WANT_SYSTEMD}" -eq 1 ]; then
  if [ ! -d "${SYSTEMD_DIR}" ]; then
    WANT_SYSTEMD=0
    say "   no systemd on this machine; units will not be installed"
  elif [ "$(id -u)" -ne 0 ]; then
    die "systemd units need root, and this is running as $(id -un).
    Either run this with sudo, or pass --no-systemd to install the
    database and configuration without the service units. Stopping now,
    before anything is created, rather than part-way through."
  fi
fi
# The database, before anything is created in it.
#
# The same rule as the systemd check above, and it was missing for the
# same reason: nobody had run this on a machine that did not already have
# a database. Measured on one that did not - the whole output a person
# saw was psql's own connection error, printed twice, with not one
# sentence from this script about what to do next.
#
# That is the first minute of the first install, on the path taken by
# every customer who does not want containers. This repository says what
# to do everywhere else; it was silent exactly where a person is most
# likely to be stuck.
#
# # Why this runs in a dry run too, and why that was the real defect
#
# The first version of this block was wrapped in `if DRY_RUN -eq 0`,
# which reads as caution and was the opposite. Measured on a machine with
# no database at all: `--dry-run` printed every stage, ended with "done",
# and exited 0. That is the mode somebody runs to ask "is this machine
# ready", answering yes when the answer is no.
#
# Every check here is a read - three SELECTs and a SHOW - so there is
# nothing for a dry run to protect. Silence was never safety; it was a
# mode that lied.
#
# # All three, then stop
#
# Reported together rather than dying at the first, for the reason
# release/gate.sh gives about itself: one run should say everything that
# is wrong. Somebody installing PostgreSQL to fix the first line, only to
# be told about the extension afterwards, is somebody making two trips.
DB_PROBLEMS=0
db_problem() {
  DB_PROBLEMS=$((DB_PROBLEMS + 1))
  printf '\n!! %s\n' "$1" >&2
}

if ! psql_super -tAc "SELECT 1" >/dev/null 2>&1; then
  db_problem "cannot reach PostgreSQL.

    The usual causes, in order:

      * PostgreSQL is not installed. Install PostgreSQL 16 and the
        TimescaleDB extension - KURULUM.md section 2 has the versions
        this is tested against.
      * It is installed but not running:  systemctl status postgresql
      * It is running somewhere else. Point this script at it:
          SUPERUSER_DSN='postgres://user@host:5432/postgres' ${0}
      * You are not the postgres superuser. Without SUPERUSER_DSN this
        script uses 'sudo -u postgres psql', which needs sudo.

    If you would rather not run a database yourself, the container path
    brings its own - see KURULUM.md section 1.5."
else
  # These two need a connection, so they are only asked when there is
  # one. Asking them anyway would add two more failures saying the same
  # thing as the first.

  # Available, which is a different question from loaded. A distro
  # PostgreSQL with no Timescale packages answers no here, and the
  # answer is worth having before four roles and ten schemas exist.
  if [ -z "$(psql_super -tAc "SELECT 1 FROM pg_available_extensions WHERE name = 'timescaledb'")" ]; then
    db_problem "PostgreSQL is running and the TimescaleDB extension is not installed on it.

    This product stores time series and its retention policies are
    Timescale's; PostgreSQL alone will not do. Install the timescaledb
    package for this server's version - KURULUM.md section 2.

    The container path brings a database with it already - KURULUM.md
    section 1.5."
  fi

  # Loaded. Timescale's own error for this case is excellent - it names
  # the config file and the exact line - but it arrives after the
  # database has been created, and this script's rule is that a
  # prerequisite is checked before anything is made.
  case "$(psql_super -tAc "SHOW shared_preload_libraries" 2>/dev/null)" in
    *timescaledb*) ;;
    *) db_problem "TimescaleDB is installed but not preloaded, so CREATE EXTENSION would fail.

    Add it to the server's configuration and restart PostgreSQL:

      shared_preload_libraries = 'timescaledb'

    On a package install that line is in postgresql.conf; timescaledb-tune
    will add it for you." ;;
  esac
fi

if [ "${DB_PROBLEMS}" -ne 0 ]; then
  die "${DB_PROBLEMS} prerequisite(s) above are not met. Nothing has been created."
fi
say "   database reachable, TimescaleDB installed and preloaded"

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

# The profile, checked here rather than where it is written, for the
# reason this whole block exists: a typo found at the config stage stops
# the install after the database is built.
case "${PROFILE}" in
  ""|hafif|dengeli|tam) ;;
  *) die "unknown profile ${PROFILE}. One of: hafif (no IP intelligence),
    dengeli (country only), tam (country and ASN). Leave it out to keep
    whatever the configuration files already say." ;;
esac

# GNU sed, checked rather than assumed.
#
# This script edits configuration files in place with `sed -i` and with
# the `0,/re/s||…|` address form, which replaces only the first match.
# Neither is POSIX. BusyBox sed - what an Alpine container has - accepts
# the second **silently and does nothing**, so every key this script
# thinks it wrote stays commented out and the file is left holding the
# example's placeholder.
#
# Measured, in this project's own container image before it carried GNU
# sed: the install reported "generated an ip_hash_key", reported "wrote
# the key into collector.toml", reported the two files matching, and had
# written nothing at all. BSD sed, on macOS, fails differently and just
# as quietly.
#
# So the dependency is named here, once, where the message can say what
# to install - rather than surviving as four silent no-ops later.
if ! sed --version 2>/dev/null | head -n1 | grep -q GNU; then
  die "this script needs GNU sed; the sed on PATH is not it.
   Alpine: apk add sed.  macOS: brew install gnu-sed, then put gnubin on PATH.
   Without it the keys and passwords below are written silently nowhere."
fi

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

# ROLE_CREDENTIAL says, once, where each role's password lives: the
# configuration file that carries it and the key inside that file.
#
# One table because it was four lists - written, pointed at the database,
# checked, and reported to a person - and the fourth fell behind. L3
# added schema_admin to three of them and not to the one that tells an
# operator a password they now have to place by hand.
#
# What that produced, measured on a clean cluster with an upgrader.toml
# the script had not written:
#
#   * the role was created with a generated password
#   * the password was not written into the file, correctly, because the
#     script never overwrites a file it did not create
#   * the password was not printed, because this list did not name it
#   * the run reported success and said "the four database passwords
#     were generated and written into the configuration files"
#
# The password existed only inside the database. The upgrader could
# never connect, and nothing in the output said so. See
# TestEveryRoleTheInstallerCreatesCanBeReported.
ROLE_CREDENTIAL=(
  "collector        collector.toml     timescale_dsn"
  "beacon_writer    beacon.toml        timescale_dsn"
  "analytics_reader analytics-api.toml timescale_dsn"
  "panel_user       panel.toml         panel_dsn"
  "schema_admin     upgrader.toml      schema_admin_dsn"
)

# ---------------------------------------------------------------- roles

say "five roles"
#
# Each role checked and created on its own, not "if none exist, create
# all five".
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
  for role in collector beacon_writer analytics_reader panel_user schema_admin; do
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

  # Connect and schema usage for all five, whether they were made now or
  # were already here: an existing role from somewhere else has no reason
  # to already hold these.
  psql_db -c "GRANT CONNECT ON DATABASE ${DB_NAME}
      TO collector, beacon_writer, analytics_reader, panel_user, schema_admin"
  psql_db -c "GRANT USAGE ON SCHEMA public
      TO collector, beacon_writer, analytics_reader, panel_user, schema_admin"

  # CREATE on the schema, to schema_admin alone.
  #
  # It is the one role that may add a table, which is what an upgrade
  # does. The other four are deliberately unable to: a service that can
  # create objects can create one the next migration then collides with.
  psql_db -c "GRANT CREATE ON SCHEMA public TO schema_admin"
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
             internal/heartbeat/schema.sql internal/retention/schema.sql \
             internal/logsink/schema.sql internal/upgrade/schema.sql \
             internal/rangerefresh/schema.sql internal/relupdate/schema.sql \
             internal/backup/schema.sql \
             internal/schemaver/schema.sql; do
      psql_db -f "${ROOT}/${f}"
    done
  fi
fi

# -------------------------------------------------- the schema version

# Recorded after the schemas and before anything else, because what the
# row asserts is "every file above was applied to this database".
#
# The values come from a binary, not from the SQL. The fingerprint is a
# hash *of* the schema files, so a literal inside one of them would
# change the hash it is supposed to state and no value would ever be
# correct. The Go constant sits outside what is hashed.
say "recording the schema version"
if [ "${DRY_RUN}" -eq 0 ]; then
  if [ -x "${HERE}/../bin/panel" ]; then
    schema_stamp="$("${HERE}/../bin/panel" -schema-version)"
  else
    schema_stamp="$(cd "${ROOT}" && go run ./cmd/panel -schema-version)"
  fi
  read -r SCHEMA_VER SCHEMA_FP <<<"${schema_stamp}"

  # Both halves checked before either is written.
  #
  # This project has been bitten once by a hash of nothing: a check whose
  # green state meant "we wrote nothing" rather than "we wrote the right
  # thing". An empty fingerprint here would do the same - the row would
  # exist, the health page would draw it, and it would describe no schema
  # at all.
  case "${SCHEMA_VER}" in
    ''|*[!0-9]*)
      die "schema version: panel -schema-version gave '${SCHEMA_VER:-}', which is not a number" ;;
  esac
  if [ "${#SCHEMA_FP}" -ne 64 ]; then
    die "schema fingerprint: got ${#SCHEMA_FP} characters, want 64 (a hex SHA-256)"
  fi

  psql_db -v ver="${SCHEMA_VER}" -v fp="${SCHEMA_FP}" <<'SQL'
INSERT INTO schema_version (id, version, fingerprint, applied_by)
VALUES (1, :ver, :'fp', 'install.sh')
ON CONFLICT (id) DO UPDATE SET
    version     = EXCLUDED.version,
    fingerprint = EXCLUDED.fingerprint,
    applied_at  = now(),
    applied_by  = EXCLUDED.applied_by;
SQL
  say "  schema ${SCHEMA_VER} (${SCHEMA_FP:0:12}...)"
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
  copy_example upgrader.example.toml      upgrader.toml

  # ---- where the services write their logs ----
  #
  # LOG_DIR used to be accepted, used for one mkdir, and written into no
  # configuration at all - so setting it created a directory nothing
  # opened, while every service went on using whatever its example file
  # said. That is how two path families came to live in one repository:
  # the installer and all five systemd units said
  # /var/log/crucible-analytic, and panel.example.toml said the same name
  # without the suffix.
  #
  # Measured, and it is not a cosmetic difference. The panel is the only
  # service whose example ships an *uncommented* dir, so it is the only
  # one that tries to open a tree at startup - and logging.Setup returns
  # an error rather than falling back, which the nightly reported as
  #
  #     panel: logging setup failed: mkdir <the other spelling>: permission denied
  #
  # with the other three services up and the panel absent. The systemd
  # path fails the same way for a different reason: ProtectSystem=strict
  # plus ReadWritePaths=/var/log/crucible-analytic makes every other
  # directory read-only, so the panel could not have written there
  # either.
  #
  # Rewritten rather than left to the example, because the example cannot
  # know what LOG_DIR this installation chose. Only where the key already
  # exists: a service whose example ships no dir logs to stderr by design,
  # and adding one here would turn a deliberate default into a surprise.
  # Created on both paths, unlike before. The mkdir used to sit inside
  # the systemd branch, so --no-systemd produced configurations naming a
  # directory the installation had never made - which is the half of this
  # the nightly actually tripped over, running as a user who could not
  # create it either.
  if [ "${DRY_RUN}" -eq 0 ]; then
    mkdir -p "${LOG_DIR}" "${STATE_DIR}"
  fi

  for file in collector.toml beacon.toml analytics-api.toml panel.toml upgrader.toml; do
    f="${CONF_DIR}/${file}"
    [ -f "${f}" ] || continue
    sed -i -E "s|^([[:space:]]*)dir[[:space:]]*=[[:space:]]*\"[^\"]*\"|\1dir = \"${LOG_DIR}\"|" "${f}"
  done

  # ---- and where they keep the data they fetch ----
  #
  # STATE_DIR had the identical defect one variable over, and it outlived
  # the fix above by three phases: accepted, used for the mkdir and the
  # chown, and written into no configuration - so setting it created a
  # directory the collector never opened, while bot data went on going
  # wherever the example said.
  #
  # It was missed because the phase that fixed LOG_DIR said, in its own
  # done-criterion, that the rest of the PREFIX / CONF_DIR / STATE_DIR
  # family had been asked the same question. It had not been. The
  # criterion was written, the phase was ticked, and nothing in between
  # measured anything - which is worse than the defect, because a plan
  # entry does not go red.
  #
  # Same rule as the log directory: only where the key already exists.
  # bot_data.path is the one uncommented state path in the examples; the
  # commented ones are defaults a service is meant to be given
  # deliberately, and writing them here would turn an off switch into a
  # surprise.
  #
  # The basename is kept rather than assumed. An operator who pointed
  # bot_data.path at a differently-named file chose that name, and this
  # moves the directory, not their file.
  #
  # Confined to the [bot_data] table by the address range rather than
  # trusting that `path` is unique in the file. It is today - the only
  # other one is local_csv_path, which the anchor would not match anyway
  # - but "the pattern happens to hit one line" is a property of today's
  # example, and a config edit that quietly hits a second line is not an
  # edit.
  for file in collector.toml; do
    f="${CONF_DIR}/${file}"
    [ -f "${f}" ] || continue
    sed -i -E "/^\[bot_data\]/,/^\[/ s|^([[:space:]]*)path[[:space:]]*=[[:space:]]*\"[^\"]*/([^\"/]+)\"|\1path = \"${STATE_DIR}/\2\"|" "${f}"
  done

  # ---- the resource profile ----
  #
  # Two keys in two files, and both files have to agree: a beacon that
  # loads the ASN datasets its collector no longer fills is paying 136 MB
  # for a column nobody writes. The examples say so in prose; this makes
  # it structural.
  #
  # Written only when asked. Without --profile the files keep whatever
  # they say, which for a fresh install is the examples' asn_lookup off -
  # and the next-steps list at the end names that, so a default nobody
  # chose is at least a default somebody was told about.
  if [ -n "${PROFILE}" ]; then
    case "${PROFILE}" in
      hafif)   p_enabled=false; p_country=false ;;
      dengeli) p_enabled=true;  p_country=true  ;;
      tam)     p_enabled=true;  p_country=false ;;
    esac
    for file in collector.toml beacon.toml; do
      f="${CONF_DIR}/${file}"
      [ -f "${f}" ] || continue
      # Confined to the [asn_lookup] table by the address range. Both
      # files carry an `enabled` key elsewhere, so an unanchored pattern
      # would turn something else on or off - the same trap the bot_data
      # rewrite above documents.
      sed -i -E "/^\[asn_lookup\]/,/^\[/ s|^([[:space:]]*)#?[[:space:]]*enabled[[:space:]]*=.*|\1enabled = ${p_enabled}|" "${f}"
      sed -i -E "/^\[asn_lookup\]/,/^\[/ s|^([[:space:]]*)#?[[:space:]]*country_only[[:space:]]*=.*|\1country_only = ${p_country}|" "${f}"
    done
    say "   resource profile: ${PROFILE} (asn_lookup written into both files)"
  fi

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
  for entry in "${ROLE_CREDENTIAL[@]}"; do
    set -- ${entry}
    write_role_password "$1" "$2" "$3"
  done

  # The database name, which is not a secret and must match --db whatever
  # happened with the passwords.
  #
  # Separate from the block above because the two have different
  # conditions, and conflating them was wrong: write_role_password only
  # runs when this script *generated* a password, so an install onto a
  # machine whose four roles already existed left every DSN pointing at
  # the example's `analytics` while the schema went into --db. Every
  # service then started, connected, and found no tables.
  #
  # Measured in this project's own container image, installing into
  # `ca_docker`: four configuration files naming `analytics`.
  #
  # Still only in a file this run created - an operator's own edited DSN
  # is not this script's to rewrite.
  point_at_database() {
    file="$1"; key="$2"
    [ -n "${FRESH_CONF[${file}]:-}" ] || return 0
    f="${CONF_DIR}/${file}"
    [ -f "${f}" ] || return 0
    sed -i -E "s|^([[:space:]]*${key}[[:space:]]*=[[:space:]]*\"postgres://[^\"]*/)[^\"?]*|\1${DB_NAME}|" "${f}"
    if [ -n "${DB_HOST}" ]; then
      sed -i -E "s|^([[:space:]]*${key}[[:space:]]*=[[:space:]]*\"postgres://[^@\"]*@)[^/\"]*|\1${DB_HOST}|" "${f}"
    fi
  }
  for entry in "${ROLE_CREDENTIAL[@]}"; do
    set -- ${entry}
    point_at_database "$2" "$3"
  done

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
  for entry in "${ROLE_CREDENTIAL[@]}"; do
    set -- ${entry}
    check_role_dsn "$1" "$2" "$3"
  done

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
  #
  # Non-empty first, and that is not defensive padding. Two files holding
  # nothing "agree", so the equality check below passed with both keys
  # missing and printed the SHA-256 of the empty string as proof - which
  # is what a silent sed failure looks like from here. A check whose green
  # state can mean "we wrote nothing" is a check that should not exist.
  written_collector="$(read_key collector.toml)"
  written_beacon="$(read_key beacon.toml)"
  if [ -z "${written_collector}" ] || [ -z "${written_beacon}" ]; then
    die "the ip_hash_key is missing from $([ -z "${written_collector}" ] && printf 'collector.toml ')$([ -z "${written_beacon}" ] && printf 'beacon.toml')after writing it; nothing was actually edited"
  fi
  a="$(digest "${written_collector}")"
  b="$(digest "${written_beacon}")"
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

  # ---- the read API's token ----
  #
  # The third secret that lives in two files, and the one that was still
  # being copied by hand. The token goes into panel.toml and its SHA-256
  # into analytics-api.toml, which is the same shape as the ip_hash_key
  # above - and the same failure when it is done wrong, except louder:
  # the panel's site pages say the numbers cannot be read, on every page,
  # from the first day.
  #
  # Left unset, the shipped examples pair an empty token with a sha256 of
  # sixty-four zeros, so a deployment that changed neither had a panel
  # that could show nothing. An unattended install - a container built
  # per customer - has nobody to notice.
  read_api_token() {
    f="${CONF_DIR}/panel.toml"
    [ -f "${f}" ] || return 0
    sed -nE 's|^[[:space:]]*analytics_api_token[[:space:]]*=[[:space:]]*"(.+)"|\1|p' "${f}" | head -n1
  }
  read_api_hash() {
    f="${CONF_DIR}/analytics-api.toml"
    [ -f "${f}" ] || return 0
    sed -nE 's|^[[:space:]]*sha256[[:space:]]*=[[:space:]]*"(.+)"|\1|p' "${f}" | head -n1
  }
  full_digest() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }

  if [ -f "${CONF_DIR}/panel.toml" ] && [ -f "${CONF_DIR}/analytics-api.toml" ]; then
    API_TOKEN="$(read_api_token)"
    API_HASH="$(read_api_hash)"
    PLACEHOLDER_HASH="0000000000000000000000000000000000000000000000000000000000000000"

    if [ -n "${API_TOKEN}" ] || { [ -n "${API_HASH}" ] && [ "${API_HASH}" != "${PLACEHOLDER_HASH}" ]; }; then
      # Something is configured. Checked rather than replaced, for the
      # same reason the ip_hash_key is read before it is written: the
      # failure worth catching is a token pasted into one file and
      # mistyped into the other, and generating a fresh pair would erase
      # the evidence instead of reporting it.
      if [ -z "${API_TOKEN}" ]; then
        say "   the read API has a token hash and the panel has no token; leaving both alone"
      elif [ "$(full_digest "${API_TOKEN}")" != "${API_HASH}" ]; then
        printf 'install: the panel'"'"'s API token does not hash to what the read API expects\n' >&2
        printf '   panel.toml token hashes to  %s\n   analytics-api.toml expects  %s\n' \
          "$(full_digest "${API_TOKEN}")" "${API_HASH}" >&2
        die "refusing to finish; the panel would show \"the numbers cannot be read\" on every site page"
      else
        say "   reusing the read API token already in place"
      fi
    else
      API_TOKEN="ca-$(newsecret)$(newsecret)"
      API_HASH="$(full_digest "${API_TOKEN}")"
      sed -i -E "s|^([[:space:]]*analytics_api_token[[:space:]]*=[[:space:]]*)\".*\"|\1\"${API_TOKEN}\"|" \
        "${CONF_DIR}/panel.toml"
      sed -i -E "s|^([[:space:]]*sha256[[:space:]]*=[[:space:]]*)\"${PLACEHOLDER_HASH}\"|\1\"${API_HASH}\"|" \
        "${CONF_DIR}/analytics-api.toml"

      # Read both back and re-hash. The write is two sed substitutions
      # into two files and either can match nothing.
      if [ "$(full_digest "$(read_api_token)")" != "$(read_api_hash)" ]; then
        die "wrote an API token whose hash does not match what analytics-api.toml now holds"
      fi
      say "   generated the read API token ($(digest "${API_TOKEN}"))"
    fi
  fi
fi

# -------------------------------------------------------------- binaries

# The five executables the units run.
#
# # What was wrong before this existed
#
# Every unit in release/systemd runs /opt/crucible-analytic/bin/<name>.
# This script created the roles, the database, the schema, the config
# files, the service accounts, the directories and the units - and put
# nothing in that directory. It only quoted those paths back in its
# closing instructions.
#
# So an operator who followed KURULUM.md exactly reached `systemctl
# enable --now crucible-collector` and got status=203/EXEC on all four
# services. The only clue was a path inside a unit file nobody had told
# them to read, and the guide's build section said "copy the bin/
# directory" without saying where to. Guarded now by
# TestEveryUnitRunsSomethingTheInstallerPutsThere, which reads the
# destination out of the units rather than being told it.
#
# # Installed by rename, which is the upgrade case
#
# A running executable cannot be written to - Linux answers ETXTBSY -
# so `install` straight over a live collector fails, and the second run
# of this script is the one that matters: it is how somebody moves to a
# new version. rename(2) over an open file is fine: the running process
# keeps the inode it started with and the next start picks up the new
# one. So each binary is written beside its destination and moved onto
# it, which is also atomic - there is no moment where the path exists
# and is half a file.
#
# Nothing is restarted here. Replacing a binary under a running service
# and restarting it are two decisions, and the second one belongs to
# whoever knows what else is happening on that machine.
say "binaries"
if [ "${DRY_RUN}" -eq 0 ]; then
  if [ -d "${BIN_DIR}" ]; then
    mkdir -p "${PREFIX}/bin"
    ensure_mode 0755 "${PREFIX}" "${PREFIX}/bin"
    installed_any=0
    for src in "${BIN_DIR}"/*; do
      [ -f "${src}" ] || continue
      name="$(basename "${src}")"
      tmp="${PREFIX}/bin/.${name}.new.$$"
      install -m 0755 "${src}" "${tmp}"
      mv -f "${tmp}" "${PREFIX}/bin/${name}"
      say "   ${PREFIX}/bin/${name}"
      installed_any=1
    done
    if [ "${installed_any}" -eq 0 ]; then
      say "   ${BIN_DIR} is empty; nothing to install"
    fi
  else
    # Not a failure. Somebody running this from a source checkout has no
    # bin/ yet, and the message names the one command that makes one -
    # because "no binaries" with no instruction is how a person ends up
    # at systemctl with nothing to run.
    say "   no ${BIN_DIR} here; build first:"
    say "     VERSION=\$(git describe --tags --always) ./release/build.sh"
    say "   or unpack a release package, which carries bin/ already"
  fi
else
  say "   dry run: would install ${BIN_DIR}/* into ${PREFIX}/bin"
fi

# ----------------------------------------------------------------- units

say "systemd units"
# Said out loud when nothing is installed. The header printed either way
# in the first version of this, so a run with --no-systemd looked exactly
# like a run that wrote four unit files - and "it said systemd units" is
# the sentence somebody would later use to argue the services were
# registered.
if [ "${WANT_SYSTEMD}" -eq 0 ]; then
  say "   skipped (--no-systemd, or no systemd here); no service units written"
fi
if [ "${DRY_RUN}" -eq 0 ] && [ "${WANT_SYSTEMD}" -eq 1 ]; then
  # Both layouts, and a refusal when neither has anything.
  #
  # # The bug this replaces
  #
  # The units were read from ${HERE}/systemd, which is right in a
  # repository checkout - this script lives in release/ and the units
  # sit beside it. In a *release package* they do not: build.sh stages
  # this script into release/ and the units into systemd/ at the top,
  # so ${HERE}/systemd names a directory that does not exist.
  #
  # The loop's own guard then hid it. With no matches the glob stays
  # literal, `[ -f ]` fails, `continue` runs, and the script carries on
  # to print "systemctl enable --now crucible-collector.service" for
  # units it never wrote. Installing from a package produced a machine
  # with no services and no error - measured by building a package and
  # looking, which is the only way this was ever going to be found.
  #
  # *Hiçbir şeyle eşleşmeyen bir glob, "yapacak iş yok" gibi görünür.*
  UNIT_SRC=""
  for candidate in "${HERE}/systemd" "${ROOT}/systemd"; do
    if [ -d "${candidate}" ]; then
      UNIT_SRC="${candidate}"
      break
    fi
  done
  if [ -z "${UNIT_SRC}" ]; then
    die "no systemd/ directory beside ${HERE} or ${ROOT}; there are no unit files to install.
     Run with --no-systemd if that is what you meant."
  fi

  # .path too, for the optional restarter. The files are installed and
  # the units are NOT enabled: see the note printed at the end.
  units_written=0
  for unit in "${UNIT_SRC}"/*.service "${UNIT_SRC}"/*.timer "${UNIT_SRC}"/*.path; do
    [ -f "${unit}" ] || continue
    install -m 0644 "${unit}" "${SYSTEMD_DIR}/"
    units_written=$((units_written + 1))
  done
  if [ "${units_written}" -eq 0 ]; then
    die "${UNIT_SRC} contains no unit files, so nothing was registered with systemd."
  fi
  say "   ${units_written} unit files -> ${SYSTEMD_DIR}"

  # The restarter's script, beside the binaries its unit names.
  #
  # Installed even though nothing enables it, because the alternative is
  # an operator who decides to turn the restarter on and finds the unit
  # pointing at a file that is not there - which fails as
  # "status=203/EXEC", a message that names the symptom and not this.
  #
  # The directory is created here rather than assumed. It is made in the
  # binaries step, but only on the branch where ${BIN_DIR} exists: a
  # source checkout with nothing built yet skips it, and this install
  # then failed with "No such file or directory" on a path nobody had
  # asked about. Found by the release test, which runs the real script.
  mkdir -p "${PREFIX}/bin"
  ensure_mode 0755 "${PREFIX}" "${PREFIX}/bin"
  restart_script=""
  for candidate in "${HERE}/restart.sh" "${ROOT}/release/restart.sh"; do
    if [ -f "${candidate}" ]; then
      install -m 0755 "${candidate}" "${PREFIX}/bin/restart.sh"
      restart_script="${PREFIX}/bin/restart.sh"
      break
    fi
  done
  # And the tmpfiles entry that creates the directory the doorbell goes
  # in - installed under ${PREFIX}, deliberately NOT under /etc/tmpfiles.d.
  #
  # Putting it in a tmpfiles search path here would create
  # /run/crucible-analytic on every machine at the next boot, and the
  # upgrader reads that directory's existence as "a restarter is
  # listening". A deployment that never opted in would then ring a
  # doorbell nobody answers, wait for services that were never
  # restarted, and roll back a release that was fine. The operator moves
  # this file into place; see the note at the end.
  # The two candidates are the two layouts, exactly as for the units
  # above: release/tmpfiles in a source checkout, tmpfiles/ beside
  # release/ in an unpacked package.
  for candidate in "${HERE}/tmpfiles/crucible-analytic.conf" \
                   "${ROOT}/tmpfiles/crucible-analytic.conf"; do
    if [ -f "${candidate}" ]; then
      mkdir -p "${PREFIX}/tmpfiles"
      install -m 0644 "${candidate}" "${PREFIX}/tmpfiles/crucible-analytic.conf"
      say "   ${PREFIX}/tmpfiles/crucible-analytic.conf (not enabled; see step 2b)"
      break
    fi
  done

  if [ -n "${restart_script}" ]; then
    say "   ${restart_script}"
  else
    # Said, not passed over. crucible-restart.service has just been
    # installed and it names this file; leaving quietly would hand the
    # operator a unit that is enable-able and cannot run.
    say "   restart.sh was not found beside this script, so the optional"
    say "   restarter cannot run. Do not enable crucible-restart.path."
  fi
  id -u "${RUN_AS}" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "${RUN_AS}"
  id -u "${RUN_AS_UPGRADER}" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "${RUN_AS_UPGRADER}"
  chown "${RUN_AS}:${RUN_AS}" "${LOG_DIR}" "${STATE_DIR}"

  # And the configuration those units are about to read.
  #
  # Missing until it was measured, and the effect was total: this script
  # created the `crucible` account, installed four units that run as it,
  # and left ${CONF_DIR} mode 0750 owned by root:root with four 0640
  # root:root files inside. The account could not so much as enter the
  # directory. Every one of the four services this script had just
  # finished configuring failed at startup with "permission denied" on
  # its own configuration file.
  #
  # Measured by starting the panel as the crucible user against the
  # configuration this script wrote:
  #
  #   panel: config error: stat /etc/crucible-analytic/panel.toml:
  #   permission denied
  #
  # It survived because the only test of this script passes
  # --no-systemd, so nothing had ever run the stage the defect was in.
  # See TestTheServicesCanReadWhatInstallWrote.
  #
  # Group, not owner. The service reads its configuration and must never
  # rewrite it: a compromised panel that could edit panel.toml could
  # point the next restart at a database of its choosing. root stays the
  # owner and the account gets read access, which is the whole of what a
  # service needs.
  chgrp "${RUN_AS}" "${CONF_DIR}"
  # 0751, and the last digit is the one worth explaining.
  #
  # Two accounts have to reach files in here: ${RUN_AS} for the four
  # services, and ${RUN_AS_UPGRADER} for upgrader.toml. They are
  # deliberately in no group together - that is the point of the second
  # account - so the upgrader reaches its file through the "other" bit.
  #
  # x without r is "you may open a file in here if you already know its
  # name", not "you may look around": the upgrader can open
  # upgrader.toml and cannot list the directory, and the file modes below
  # are what actually decide who reads what. The alternative was putting
  # the upgrader in the ${RUN_AS} group, which would have handed it read
  # access to all four service configurations to solve a traversal
  # problem.
  chmod 0751 "${CONF_DIR}"
  for f in collector.toml beacon.toml analytics-api.toml panel.toml; do
    [ -f "${CONF_DIR}/${f}" ] || continue
    chgrp "${RUN_AS}" "${CONF_DIR}/${f}"
    chmod 0640 "${CONF_DIR}/${f}"
  done

  # ip_hash_key is deliberately not in that list. No service reads it -
  # the key lives inside collector.toml and beacon.toml, and this file
  # exists so that a re-run of this script can find the key it already
  # generated rather than rotating it and orphaning every stored
  # pseudonym. It stays root-only.

  # And upgrader.toml is deliberately not in it either, for the opposite
  # reason: it goes to the *other* account.
  #
  # This is the file that must not be readable by ${RUN_AS}. It carries
  # the DSN for schema_admin, which owns every table; a panel that could
  # read it could connect as the role that rewrites the database the
  # panel is forbidden to even SELECT from.
  #
  # ${CONF_DIR} is now group ${RUN_AS} and mode 0750, so ${RUN_AS} can
  # list the directory and see that this file exists. That is fine and
  # it is not an oversight: the *name* is in KURULUM.md, in the example
  # configuration and in this script. What must not leak is the
  # contents, and 0640 root:${RUN_AS_UPGRADER} is what withholds it.
  if [ -f "${CONF_DIR}/upgrader.toml" ]; then
    chgrp "${RUN_AS_UPGRADER}" "${CONF_DIR}/upgrader.toml"
    chmod 0640 "${CONF_DIR}/upgrader.toml"
  fi

  # Only when the units really went to systemd. Told to reload a
  # directory it does not manage, systemctl succeeds and does nothing,
  # which would read here as a reload that happened.
  if [ "${SYSTEMD_DIR}" = "/etc/systemd/system" ]; then
    systemctl daemon-reload 2>/dev/null || true
  fi
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

# What to do next, in the order it has to be done.
#
# # Why this is here and not only in KURULUM.md
#
# It is in KURULUM.md, across four sections, and that is the right place
# for the reasoning. It is the wrong place for the next three commands:
# somebody who has just watched this script succeed is looking at a
# terminal, not at a document, and the gap between "it installed" and "I
# can see my analytics" is where an install stops being a product.
#
# Measured on the path this list is for: with no systemd units and no
# reverse proxy, nothing this script writes is reachable by a person.
# The script said so nowhere.
#
# # The order is not decoration
#
# The panel binds to loopback, so without a reverse proxy there is
# nothing to open; the developer link points at a URL, so it needs that
# URL to exist first; and the snippet is worth nothing until somebody has
# signed in and finished the wizard. Reversing any two of these produces
# a step that cannot be completed, which is how a list of next steps
# loses the reader.
if [ "${DRY_RUN}" -eq 0 ]; then
  # The site id, read back out of the file rather than remembered.
  #
  # This script does not set it - the examples ship "example-site" and a
  # person decides - and the list below said nothing about that until it
  # was read. A step the reader has to do, that the installer neither
  # does nor mentions, is a step that gets skipped; and this one is
  # irreversible, because every stored row is keyed by it and changing it
  # later starts a second site rather than renaming the first.
  INSTALLED_SITE_ID="$(sed -n 's/^site_id[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
    "${CONF_DIR}/collector.toml" 2>/dev/null | head -1)"

  # Which profile the configuration actually ended up in, read back out
  # of the file for the same reason the site id is: what a run intended
  # and what a file says are two things, and the second is the one that
  # runs.
  #
  # Named even when it was not chosen, which is the point. The examples
  # ship asn_lookup off, so an install nobody steered lands on Hafif -
  # no country breakdown, no ASN breakdown - and used to say nothing.
  # A default the customer never chose and was never told about is
  # discovered weeks later, looking at an empty chart.
  INSTALLED_PROFILE="hafif"
  if grep -qE '^[[:space:]]*enabled[[:space:]]*=[[:space:]]*true' "${CONF_DIR}/collector.toml" 2>/dev/null; then
    if sed -n '/^\[asn_lookup\]/,/^\[/p' "${CONF_DIR}/collector.toml" 2>/dev/null |
       grep -qE '^[[:space:]]*country_only[[:space:]]*=[[:space:]]*true'; then
      INSTALLED_PROFILE="dengeli"
    else
      INSTALLED_PROFILE="tam"
    fi
  fi

  printf '\nNEXT STEPS\n\n'


  if [ -z "${INSTALLED_SITE_ID}" ] || [ "${INSTALLED_SITE_ID}" = "example-site" ]; then
    cat <<NEXT
  0. Decide the site id and write it into the config files, before
     anything starts:
       site_id = "..."      in ${CONF_DIR}/collector.toml
       sites   = ["..."]    in ${CONF_DIR}/beacon.toml

     It is still the example's "example-site". Every stored row is keyed
     by it, and changing it later does not rename anything - it starts a
     second site whose history begins that day. KURULUM.md section 6.

NEXT
  fi

  if [ "${INSTALLED_PROFILE}" = "hafif" ]; then
    cat <<NEXT
     While you are in those files: IP intelligence is off, which is what
     the example configs ship. No country breakdown and no ASN
     breakdown - those charts will be empty and nothing else is
     affected. Re-run with --profile dengeli (country only, about
     256 MB) or --profile tam (country and ASN, about 512 MB), or set
     asn_lookup.enabled in both files yourself.

NEXT
  fi

  cat <<NEXT
  1. Reverse proxy and TLS - KURULUM.md section 8.
     Every service binds to 127.0.0.1 on purpose, so until something
     terminates TLS in front of them there is nothing to open in a
     browser.
NEXT

  if [ "${WANT_SYSTEMD}" -eq 1 ]; then
    cat <<NEXT
  2. Start the services:
       systemctl enable --now crucible-collector.service crucible-beacon.service
       systemctl enable --now crucible-analytics-api.service crucible-panel.service
       systemctl enable --now crucible-upgrader.timer

     The upgrader is a timer, not a service: enabling the service itself
     would run one upgrade and stop.

  2b. Optional, and a decision: let the panel restart the services after
     a version update.

       install -m 0644 ${PREFIX}/tmpfiles/crucible-analytic.conf /etc/tmpfiles.d/
       systemd-tmpfiles --create /etc/tmpfiles.d/crucible-analytic.conf
       systemctl enable --now crucible-restart.path

     All three lines, in that order. The tmpfiles one creates
     /run/crucible-analytic, and the upgrader reads that directory's
     existence as its answer to "is a restarter listening?" - so
     enabling the unit without it gives you a unit that is correct,
     running, and can never fire.

     Without it, a version update replaces the binaries and the page
     tells you to restart the services yourself - because the running
     processes keep the old files open until something restarts them.

     With it, the upgrader can create one file in one directory, and a
     unit whose command root wrote does the restart. The upgrader gains
     no permission: the file's contents are never read, so the worst it
     could ask for is this exact restart.

     What that buys you is the automatic undo. The upgrader waits for
     every service to report back through the database, and if one does
     not, it puts the previous binaries back and restarts again - by
     itself, in seconds, without anybody watching.
NEXT
  else
    cat <<NEXT
  2. Start the services yourself. No unit files were written (--no-systemd,
     or no systemd here), so nothing is running yet:
       ${PREFIX}/bin/panel -config ${CONF_DIR}/panel.toml
     ...and the same for collector, beacon and analytics-api.
     Without systemd nothing restarts them; that is yours to arrange.
NEXT
  fi

  cat <<NEXT
  3. Create the one-time developer link, open it, and run the wizard:
       ${PREFIX}/bin/panel -config ${CONF_DIR}/panel.toml -dev-link -base-url https://${DOMAIN:-panel.example.com}
     The wizard ends by handing the installation to the customer's own
     account; after that this machine's operator cannot sign in without
     their approval.

  4. Put the snippet on the site - KURULUM.md section 10. It is printed by:
       ${PREFIX}/bin/beacon -snippet https://${DOMAIN:-example.com} ${INSTALLED_SITE_ID:-<site-id>}

  Resource profile: ${INSTALLED_PROFILE}. The collector checks it against
  this machine's memory at startup and refuses only what a container
  limit would kill.

  KURULUM.md section 13 is how to tell whether it is really working.

NEXT
fi

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
  for entry in "${ROLE_CREDENTIAL[@]}"; do
    set -- ${entry}
    role="$1"; file="$2"
    [ -n "${ROLE_PW[${role}]:-}" ] || continue
    [ -n "${FRESH_CONF[${file}]:-}" ] || NEEDS_HAND+=("${role} ${file}")
  done
fi

if [ "${#NEEDS_HAND[@]}" -gt 0 ]; then
  cat <<'SECRETS'

These roles were created now, and their configuration files were already
here - so this script did not touch them. It never overwrites a file it
did not write, because that file may hold a working password, a site id
that cannot be regenerated, or both.

**This is the only time these passwords are shown.** They exist nowhere
else: not in a file, not in a log, and not recoverably in the database,
which stores only a hash. Copy them somewhere before you close this
terminal.

Put each into the DSN in the file named beside it, and start nothing
before you have.

SECRETS
  for entry in "${NEEDS_HAND[@]}"; do
    set -- ${entry}
    printf '  %-18s %-20s %s\n' "$1" "$2" "${ROLE_PW[$1]}"
  done
  echo
elif [ "${#ROLE_PW[@]}" -gt 0 ]; then
  # The count is counted, not written out.
  #
  # This paragraph said "the four database passwords" while five roles
  # were being created, and the sentence was wrong in the way that
  # matters: it asserted every generated password had been written, on a
  # run where one had not been. A number in a message is a claim, and a
  # claim nobody recomputes is a claim that goes stale in the direction
  # of reassurance.
  cat <<SECRETS

The ${#ROLE_PW[@]} database password(s) generated by this run were written into
the configuration files. Each file was read back and checked: it carries
the password this run generated, and its DSN reaches that role in this
database.

They are not printed. They are in the files, which are mode 0640, and a
password on a terminal is a password in a scrollback. Read one out of
its file if you ever need it.

SECRETS
fi

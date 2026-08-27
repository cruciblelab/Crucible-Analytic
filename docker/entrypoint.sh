#!/bin/sh
# The image's five entry points, plus the one-time install.
#
# `docker run … collector` starts the collector with the config the init
# step wrote. `docker run … init` is that init step. Anything else is
# executed as given, so `docker run … psql "$DSN"` and
# `docker run … devpass` both work without this script having to know
# about them.
#
# POSIX sh, not bash: the runtime image is alpine and adding bash to it
# for one dispatcher would be a package nobody asked for.
set -eu

CONF_DIR="${CA_CONF_DIR:-/etc/crucible-analytic}"

usage() {
    cat >&2 <<'USAGE'
crucible-analytic — one image, five entry points.

  init             create the roles, apply the schemas and the grants,
                   write the configuration files, once. Needs
                   SUPERUSER_DSN and DB_NAME.
  collector        the TLS proxy that fingerprints every connection
  beacon           the endpoint the page snippet posts to
  analytics-api    the read-only HTTP API
  panel            the customer's web interface
  devpass          hash a developer password, or generate an ip_hash_key

Anything else is run as a command, so `psql "$DSN"` works too.

Each service reads /etc/crucible-analytic/<name>.toml. Give a different
path by passing -config yourself; the arguments after the service name
are handed to it unchanged.
USAGE
    exit 64
}

# run_service starts one binary against its configuration file.
#
# The config path is supplied only when the caller has not given one, so
# `collector -config /somewhere/else.toml` still does what it says - and
# only when the caller wants the service to *start*. `collector -version`
# is the question somebody asks when a container will not come up, so it
# must not need a configuration file to answer: the first version of this
# refused to run it, which is precisely backwards.
run_service() {
    binary="$1"
    config="$2"
    shift 2

    for arg in "$@"; do
        case "${arg}" in
            -config|--config|-version|--version|-h|-help|--help)
                exec "${binary}" "$@" ;;
        esac
    done

    if [ ! -f "${config}" ]; then
        # The likeliest failure on a fresh stack, and worth naming: the
        # service started before the init did, or the volume that holds
        # the configuration is not mounted. A "file not found" from a Go
        # flag parser would send somebody looking in the wrong place.
        echo "crucible: ${config} does not exist." >&2
        echo "  Either the one-time \`init\` has not run, or ${CONF_DIR} is not mounted." >&2
        exit 78
    fi
    exec "${binary}" -config "${config}" "$@"
}

# do_init runs the installer once, then adapts what it wrote to the fact
# that this is a container.
#
# install.sh is the same script a hand-installed server runs, and that is
# deliberate: one installer, exercised by everybody, rather than a second
# container-shaped one that drifts. It already does the right thing here -
# its systemd block is guarded on /etc/systemd/system existing, which it
# does not in this image, so no unit files and no useradd.
#
# Three things afterwards are genuinely container-specific.
do_init() {
    : "${SUPERUSER_DSN:?set SUPERUSER_DSN to a superuser connection}"
    DB_NAME="${DB_NAME:-analytics}"

    # Where the *services* will reach the database, which is not
    # localhost here. Taken from SUPERUSER_DSN rather than from a second
    # variable: whoever ran init already had to say where the database
    # is, and a second place to say it is a second place to get it wrong.
    #
    # Passed in rather than patched afterwards, because install.sh
    # connects with each DSN it writes to check that the file is usable -
    # and a DSN still saying localhost fails that check inside a
    # container, correctly and confusingly.
    db_host="$(printf '%s' "${SUPERUSER_DSN}" | sed -E 's|^[a-z+]+://||; s|^[^@]*@||; s|[/?].*$||')"

    SUPERUSER_DSN="${SUPERUSER_DSN}" DB_NAME="${DB_NAME}" DB_HOST="${db_host}" \
        CONF_DIR="${CONF_DIR}" PREFIX=/opt/crucible \
        LOG_DIR=/var/log/crucible STATE_DIR=/var/lib/crucible \
        /opt/crucible/release/install.sh

    # 1. Logs to stdout.
    #
    # The file tree exists because a hand-installed server has no
    # journald guarantee and somebody has to be able to read yesterday's
    # errors. A container's platform contract is the opposite: whatever
    # goes to stdout is collected, rotated and shipped by the thing
    # running the container, and a log file inside a container is a log
    # file that disappears with it.
    #
    # logging.Config treats an empty dir as "stderr only", so this is a
    # supported setting rather than a container-only code path.
    for f in collector.toml beacon.toml analytics-api.toml panel.toml; do
        [ -f "${CONF_DIR}/${f}" ] || continue
        sed -i -E 's|^dir = "/var/log/crucible"$|dir = ""|' "${CONF_DIR}/${f}"
    done

    # 2. Listen on the container's own address, not its loopback.
    #
    # This is the one place where a container reverses a security
    # decision, so it is worth being explicit about what replaces it.
    #
    # On a server, `listen_addr = "127.0.0.1:8082"` is the control: the
    # read API is unreachable from the network and a reverse proxy in
    # front of it terminates TLS. Inside a container, 127.0.0.1 is that
    # container alone - the panel in the next container cannot reach it,
    # and the service is not merely protected but useless.
    #
    # What replaces the loopback bind is the compose network: these ports
    # are reachable from the other services and from nowhere else, because
    # nothing publishes them to the host. That control lives in the
    # compose file rather than here, which is why the compose file says so
    # in as many words. Publishing 8082 to the host would hand the read
    # API, bearer token and all, to the internet.
    for f in beacon.toml analytics-api.toml panel.toml; do
        [ -f "${CONF_DIR}/${f}" ] || continue
        sed -i -E 's|^listen_addr = "127\.0\.0\.1:|listen_addr = "0.0.0.0:|' "${CONF_DIR}/${f}"
    done

    # 3. Where the other containers are.
    #
    # The database half is handled above by DB_HOST. What is left is the
    # panel reaching the read API over HTTP, for the same reason: the
    # example says 127.0.0.1, which inside the panel's container is the
    # panel.
    if [ -f "${CONF_DIR}/panel.toml" ]; then
        sed -i -E "s|^analytics_api_url = \"http://127\.0\.0\.1:([0-9]+)\"|analytics_api_url = \"http://${CA_API_HOST:-analytics-api}:\\1\"|" \
            "${CONF_DIR}/panel.toml"
    fi

    # What only the operator knows: which site this stack is for, and
    # what the collector proxies to. Both are irreversible in different
    # ways - every stored row is keyed by the site id, and a wrong
    # backend means the customer's visitors get nothing - so neither is
    # guessed and neither is defaulted. Absent, the example's placeholder
    # stays and the service says what is wrong with it.
    if [ -n "${CA_SITE_ID:-}" ] && [ -f "${CONF_DIR}/collector.toml" ]; then
        sed -i -E "s|^site_id = \".*\"|site_id = \"${CA_SITE_ID}\"|" "${CONF_DIR}/collector.toml"
        sed -i -E "s|^sites = \[.*\]|sites = [\"${CA_SITE_ID}\"]|" "${CONF_DIR}/beacon.toml"
    fi
    if [ -n "${CA_BACKEND_ADDR:-}" ] && [ -f "${CONF_DIR}/collector.toml" ]; then
        sed -i -E "s|^backend_addr = \".*\"|backend_addr = \"${CA_BACKEND_ADDR}\"|" "${CONF_DIR}/collector.toml"
    fi

    echo "== container adjustments applied (stdout logging, container-local bind, service hostnames)"
}

[ $# -ge 1 ] || usage

command="$1"
shift

case "${command}" in
    init)           do_init ;;
    collector)      run_service collector      "${CONF_DIR}/collector.toml" "$@" ;;
    beacon)         run_service beacon         "${CONF_DIR}/beacon.toml" "$@" ;;
    analytics-api)  run_service analytics-api  "${CONF_DIR}/analytics-api.toml" "$@" ;;
    panel)          run_service panel          "${CONF_DIR}/panel.toml" "$@" ;;
    devpass)        exec devpass "$@" ;;
    -h|--help|help) usage ;;
    *)              exec "${command}" "$@" ;;
esac

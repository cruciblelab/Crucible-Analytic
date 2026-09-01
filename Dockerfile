# One image, five entry points.
#
# # Why one image and not five
#
# The deployment model this product is sold under is one stack per
# customer: a collector, a beacon, a read API and a panel that were built
# together, share a database schema, and share the ip_hash_key that makes
# the crossover join possible. Five images would be five tags to keep in
# step, and the failure when they drift is not a crash - it is a
# deployment where the beacon writes a pseudonym the collector cannot
# match, and the one view that proves this product works quietly reports
# zero.
#
# So: one image, one tag, five commands. `docker run … collector`,
# `… beacon`, `… analytics-api`, `… panel`, `… devpass`, and `… init` for
# the one-time install. Version skew between services stops being a thing
# that can happen.
#
# It also carries psql, which is not padding: release/install.sh needs it,
# and an operator who has to look at their own database during an
# incident should not have to install anything to do it.
#
# # What this shares with release/build.sh
#
# The same flags, for the same reasons, and the reasons are written out
# there: -trimpath and CGO_ENABLED=0 so the binary does not carry the
# build machine, -buildvcs=false so a build from an exported copy of a
# commit produces the same bytes as a build from the repository, and the
# version supplied by -X rather than read from git, which a build context
# does not have anyway.

# ---------------------------------------------------------------- build

# Pinned to the version go.mod pins. release/build.sh refuses to run when
# the toolchain differs, and the reason applies here too: a package built
# with a different Go is not the same package, and the difference shows
# up as a behaviour change nobody can trace.
FROM golang:1.25.13-alpine AS build

WORKDIR /src

# Dependencies first, so editing a .go file does not re-download the
# module cache.
COPY go.mod go.sum ./
# The same optional CA as the runtime stage below, for the same reason:
# behind a TLS-terminating proxy this is where the build stops first,
# with an error that names proxy.golang.org and not the proxy.
RUN --mount=type=secret,id=extra_ca,target=/run/secrets/extra_ca \
    set -eux; \
    if [ -s /run/secrets/extra_ca ]; then \
        cat /run/secrets/extra_ca >> /etc/ssl/certs/ca-certificates.crt; \
    fi; \
    go mod download

COPY . .

# Stamped from the outside. The build context is a directory, not a git
# repository, so nothing in here can work out what commit it is - which
# is exactly why release/build.sh passes it in too.
ARG VERSION=unknown
RUN set -eux; \
    for b in collector beacon analytics-api panel devpass upgrader; do \
        CGO_ENABLED=0 go build \
            -trimpath -buildvcs=false \
            -ldflags "-s -w -X main.version=${VERSION}" \
            -o "/out/${b}" "./cmd/${b}"; \
    done

# -------------------------------------------------------------- runtime

FROM alpine:3.22

# bash and postgresql-client for install.sh, ca-certificates for
# outbound TLS - the bot-data refresh and the mail wizard both make one -
# and tzdata because the panel renders every timestamp in a configured
# zone and "Europe/Istanbul" has to resolve to something.
#
# GNU sed for the same reason as bash, and it is the sharper of the two:
# install.sh edits config files in place with the `0,/re/s||…|` form,
# which BusyBox sed accepts *silently and ignores*. Before this line the
# init reported writing an ip_hash_key, reported the two files matching,
# and had written nothing - the "matching" being two empty strings. The
# installer now refuses to start without GNU sed, so the same mistake
# elsewhere fails loudly instead.
#
# bash rather than trusting alpine's sh: release/install.sh uses an
# associative array for the generated role passwords and a here-string
# for the failure report, and neither is POSIX. Found by running the init
# in this image, where it stopped at
#
#   env: can't execute 'bash': No such file or directory
#
# before printing anything. The alternative - rewriting the installer in
# POSIX sh so alpine can run it - would mean a second installer to keep
# in step with the one a hand-installed server uses, which is the thing
# this image exists not to do.
#
# The optional `extra_ca` secret is for building behind a network that
# terminates TLS in the middle, which is most corporate networks and
# every locked-down CI runner. Without it the package fetch fails with
# "certificate verify failed" and the message points at Alpine's mirror
# rather than at the proxy, which is a wrong-looking error to debug.
#
#   docker build --secret id=extra_ca,src=/path/to/ca.crt .
#
# A secret rather than a COPY: it is mounted for one instruction and
# never becomes a layer, so an image built behind such a proxy does not
# carry that network's CA to whoever runs it. Appended twice on purpose -
# installing ca-certificates rewrites the bundle, so the first append
# gets the fetch through and the second survives it.
RUN --mount=type=secret,id=extra_ca,target=/run/secrets/extra_ca \
    set -eux; \
    if [ -s /run/secrets/extra_ca ]; then \
        cat /run/secrets/extra_ca >> /etc/ssl/certs/ca-certificates.crt; \
    fi; \
    apk add --no-cache bash sed postgresql17-client ca-certificates tzdata; \
    if [ -s /run/secrets/extra_ca ]; then \
        cat /run/secrets/extra_ca >> /etc/ssl/certs/ca-certificates.crt; \
    fi

# A user that is not root. Nothing here binds a privileged port: the
# collector listens on 8443 inside the container and the host publishes
# 443 to it, which is the orchestrator's job rather than the process's.
RUN addgroup -S crucible && adduser -S -G crucible -H -s /sbin/nologin crucible

COPY --from=build /out/ /opt/crucible/bin/

# What the install needs, in the layout install.sh already knows how to
# find: it looks for ../schema and ../ornek-yapilandirma relative to
# itself, the same as inside an unpacked release tarball.
COPY internal/panel/schema.sql      /opt/crucible/schema/01-panel.sql
COPY internal/storage/schema.sql    /opt/crucible/schema/02-storage.sql
COPY internal/beacon/schema.sql     /opt/crucible/schema/03-beacon.sql
COPY internal/asnlookup/schema.sql  /opt/crucible/schema/04-asnlookup.sql
COPY internal/heartbeat/schema.sql  /opt/crucible/schema/05-heartbeat.sql
COPY internal/retention/schema.sql  /opt/crucible/schema/06-retention.sql

COPY config.example.toml            /opt/crucible/ornek-yapilandirma/config.example.toml
COPY beacon.example.toml            /opt/crucible/ornek-yapilandirma/beacon.example.toml
COPY analytics-api.example.toml     /opt/crucible/ornek-yapilandirma/analytics-api.example.toml
COPY panel.example.toml             /opt/crucible/ornek-yapilandirma/panel.example.toml
COPY upgrader.example.toml          /opt/crucible/ornek-yapilandirma/upgrader.example.toml

COPY release/install.sh release/verify.sh /opt/crucible/release/
COPY release/sql/                         /opt/crucible/release/sql/
COPY docker/entrypoint.sh                 /opt/crucible/entrypoint.sh

COPY LICENSE NOTICE THIRD-PARTY.md KURULUM.md README.md /opt/crucible/

RUN chmod 0755 /opt/crucible/entrypoint.sh /opt/crucible/release/install.sh \
               /opt/crucible/release/verify.sh

# Two directories, and only one of them is a volume.
#
# /etc/crucible-analytic holds the configuration the init step writes,
# including the ip_hash_key. It must survive the container, because
# regenerating that key silently breaks the pseudonyms of every row
# already stored - so it is a volume, and the compose file says so.
#
# /var/lib/crucible holds the downloaded bot-data file, which is a cache:
# losing it costs one refresh. Not a volume, deliberately.
#
# /var/log/crucible-analytic exists because install.sh creates LOG_DIR on
# every path now, and inside the image it runs as `crucible`, who cannot
# create a directory under /var/log. The entrypoint then blanks the dir
# in every configuration - a container logs to stdout - so nothing is
# written here; what the directory buys is an install that does not stop
# at its own mkdir.
#
# Its name follows the installer and the systemd units rather than the
# /var/lib spelling above. Two spellings for the log directory is exactly
# the defect this came from - see TestOneLogDirectoryFamily - and the
# /var/lib pair is the same shape still open, one directory over.
RUN mkdir -p /etc/crucible-analytic /var/lib/crucible /var/log/crucible-analytic \
 && chown -R crucible:crucible /etc/crucible-analytic /var/lib/crucible /var/log/crucible-analytic \
 && chmod 0750 /etc/crucible-analytic

ENV PATH="/opt/crucible/bin:${PATH}" \
    CA_CONF_DIR="/etc/crucible-analytic"

USER crucible
WORKDIR /opt/crucible

ENTRYPOINT ["/opt/crucible/entrypoint.sh"]
# No default command. A container started with no argument should say
# what the arguments are, not pick one - four of the five do different
# jobs and none of them is the obvious default.

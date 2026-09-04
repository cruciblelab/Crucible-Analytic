#!/bin/sh
# Restart the four services. Nothing else, and nothing from anywhere.
#
# Started by crucible-restart.service, which is started by
# crucible-restart.path when the upgrader touches a file. This script
# reads no arguments, no environment and no file: every name it acts on
# is written below, by whoever installed it.
#
# # Why it takes no input
#
# The process that triggers it is the upgrader, which fetches archives
# from an address in a config file. If the request said which units to
# restart, then owning the upgrader would mean choosing what root runs.
# It says nothing, so owning the upgrader means being able to cause this
# exact restart - which is a thing anybody with the machine can already
# do.
#
# *Bir isteğin taşıdığı her alan, onu yazana verilmiş bir yetkidir.*
#
# # Restart, never stop
#
# `systemctl restart` starts a unit that is not running. There is no
# path through this script that leaves a service down: no stop, no
# disable, no mask. A unit that was deliberately stopped will be started
# by this, which is the right trade - the alternative is a script that
# can be used to keep a service down.
#
# # What it does not decide
#
# Whether the new binaries work, and whether to put the old ones back.
# Both need the database - the heartbeat is what says a service came
# back rather than merely started - and this script deliberately has no
# database credentials. The upgrader watches, and rolls back if it has
# to. See internal/relupdate/restart.go.
set -eu

UNITS="crucible-collector crucible-beacon crucible-analytics-api crucible-panel"

# The doorbell is cleared first, not last.
#
# Cleared afterwards, a restart that failed halfway would leave the file
# in place; the .path unit fires on modification, so it would not
# re-trigger, and the file would sit there looking like a pending
# request nobody served. Clearing first means the state on disk always
# reads "no request outstanding" while one is being served.
rm -f /run/crucible-analytic/restart-please

# Refuse to run against a tree that is not there.
#
# The check is worth its two lines because of what the failure looks
# like without it: systemctl restarts units whose ExecStart points at a
# missing file, they fail, systemd retries five times, and the customer's
# site is down while the logs say "status=203/EXEC" - which names the
# symptom and not this.
for unit in ${UNITS}; do
  if ! systemctl cat "${unit}" >/dev/null 2>&1; then
    echo "restart.sh: ${unit} is not installed on this machine; restarting nothing" >&2
    exit 1
  fi
done

echo "restart.sh: restarting ${UNITS}"

# One systemctl call rather than four.
#
# systemd orders them by their own After= relationships, which is what
# those declarations are for; four sequential calls would impose this
# script's order instead, and this script does not know the dependency
# graph.
systemctl restart ${UNITS}

# And say what is actually running, so journalctl carries the answer
# rather than only the request.
for unit in ${UNITS}; do
  printf 'restart.sh: %-28s %s\n' "${unit}" "$(systemctl is-active "${unit}" || true)"
done

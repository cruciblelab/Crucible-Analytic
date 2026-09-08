#!/usr/bin/env bash
# Runs what CI's merge gate runs, in one command.
#
#   ./release/gate.sh            # everything that needs no database
#   ./release/gate.sh --all      # plus the integration half
#
# # Why this exists
#
# The gate was eight commands in CONTRIBUTING.md, and eight commands is a
# list somebody runs the familiar two of. That happened: a package was
# pushed after `go test ./...` came back clean, and the gate went red on
# gosec - a step that is not part of `go test`, needs a tool installed,
# and therefore never runs by accident.
#
# "The suite is clean" was true and it was not the claim that mattered.
#
# # What it will not do
#
# It does not replace CI. The integration half needs a database and the
# browser half needs Chromium, so a green run here is "the gate will
# probably pass", not "the gate passed". What it removes is the failure
# mode where a whole step was never run at all.
set -uo pipefail

# Where this script is, resolved before the cd below moves the ground
# under a relative path. `bash release/gate.sh` and `./release/gate.sh`
# and `bash gate.sh` from inside release/ all have to reach the same
# file, because the transcript wrapper re-runs it.
gate_self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"

cd "$(dirname "$0")/.."

# The whole run, on disk, whatever the caller does with the output.
#
# # Why
#
# A gate went red on TestNoServiceStopsWhileTheSchemaIsApplied and the
# message was gone: the run had been piped through grep to keep the
# summary lines, and the summary lines are exactly the ones that do not
# carry the numbers. Three re-runs passed, so the one run that had
# something to say was the one nobody could read.
#
# That is this repository's oldest lesson wearing a new hat - *bir
# teşhis, ona ulaşamayan bir yol için yok demektir* - and the fix is the
# same shape as the compose-log one: put the diagnostic somewhere the
# failing path cannot lose it.
#
# The script re-runs itself once, through tee, with the log path in the
# environment so the second pass knows not to do it again. The path is
# printed on stderr, so a filter on stdout cannot swallow it either.
if [ -z "${CA_GATE_LOG:-}" ]; then
  CA_GATE_LOG="${TMPDIR:-/tmp}/ca-gate-$(date +%Y%m%d-%H%M%S)-$$.log"
  export CA_GATE_LOG
  set -o pipefail
  "${gate_self}" "$@" 2>&1 | tee "${CA_GATE_LOG}"
  gate_status=$?
  printf '\nfull transcript: %s\n' "${CA_GATE_LOG}" >&2
  exit "${gate_status}"
fi

# The handshake is over: this is the wrapped pass. The variable is
# unset before any step runs, so a gate started *by* a step - which is
# how release/gatelog_test.go checks this behaviour - wraps itself and
# writes its own transcript rather than inheriting the decision not to.
unset CA_GATE_LOG

# Pinned to the same versions the workflow pins, for the reason the
# workflow gives: an unpinned analyser moves the baseline under a gate
# without anybody choosing to. release/gate_test.go checks these against
# .github/workflows/ci.yml so the two cannot drift.
GOSEC_VERSION="v2.29.0"
DEADCODE_VERSION="v0.49.0"

failed=0
ran=0

# CA_GATE_ONLY runs the steps whose names match one pattern.
#
# For iterating on a single check without paying for the other eight -
# and it is what makes the transcript above testable in a second rather
# than in ten minutes.
step() {
  if [ -n "${CA_GATE_ONLY:-}" ]; then
    case "$1" in
      *${CA_GATE_ONLY}*) ;;
      *) return 0 ;;
    esac
  fi
  ran=$((ran + 1))
  printf '\n== %s\n' "$1"
  shift
  if "$@"; then
    return 0
  fi
  printf '!! FAILED: %s\n' "$1"
  failed=1
}

# Kept going rather than stopping at the first red, deliberately. One
# run should report everything that is wrong; a script that stops at the
# first failure turns a five-minute check into five of them.
check_gofmt() {
  local out
  out="$(gofmt -l . | grep -v '^dist/' || true)"
  [ -z "${out}" ] || { printf '%s\n' "${out}"; return 1; }
}

check_tags() {
  local tag
  for tag in loadtest network release e2e docker integration; do
    go vet -tags "${tag}" ./... || return 1
  done
}

check_gosec() {
  go install "github.com/securego/gosec/v2/cmd/gosec@${GOSEC_VERSION}" || return 1
  "$(go env GOPATH)/bin/gosec" -fmt=json -quiet -no-fail -severity=medium \
    -out=/tmp/ca-gosec.json ./... || return 1
  go run ./internal/sast/cmd/sastdiff -report /tmp/ca-gosec.json
}

check_deadcode() {
  go install "golang.org/x/tools/cmd/deadcode@${DEADCODE_VERSION}" || return 1
  "$(go env GOPATH)/bin/deadcode" -test -tags=integration ./... > /tmp/ca-deadcode.txt || return 1
  go run ./internal/sast/cmd/deadcodediff -report /tmp/ca-deadcode.txt
}

step "gofmt"                check_gofmt
step "go build"             go build ./...
step "go vet"               go vet ./...
step "go vet, every tag"    check_tags
step "go test -race"        go test -count=1 -race ./...
step "gosec vs baseline"    check_gosec
step "deadcode vs allowlist" check_deadcode

if [ "${1:-}" = "--all" ]; then
  step "release tests"      go test -tags release -count=1 ./release/
  step "integration"        env CA_BROWSER_TEST=1 go test -tags integration -race -count=1 ./...
else
  printf '\n== skipped: release and integration (need a database)\n'
  printf '   run with --all once CA_SUPERUSER_DSN points at one\n'
fi

printf '\n'
if [ "${ran}" -eq 0 ]; then
  # A filter that matched no step is not a pass. The whole point of this
  # script is that a step nobody ran cannot be reported as a step that
  # succeeded.
  printf 'gate: RED - no step matched CA_GATE_ONLY=%s\n' "${CA_GATE_ONLY:-}"
  exit 1
fi
if [ "${failed}" -ne 0 ]; then
  printf 'gate: RED\n'
  exit 1
fi
if [ -n "${CA_GATE_ONLY:-}" ]; then
  # Never the plain word. A filtered run is a green for the steps it
  # chose, and somebody reading "gate: green" in a terminal three days
  # later has no way to know a filter was set.
  printf 'gate: green for the %d step(s) matching %s - not the whole gate\n' \
    "${ran}" "${CA_GATE_ONLY}"
  exit 0
fi
printf 'gate: green\n'

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

cd "$(dirname "$0")/.."

# Pinned to the same versions the workflow pins, for the reason the
# workflow gives: an unpinned analyser moves the baseline under a gate
# without anybody choosing to. release/gate_test.go checks these against
# .github/workflows/ci.yml so the two cannot drift.
GOSEC_VERSION="v2.29.0"
DEADCODE_VERSION="v0.49.0"

failed=0
step() {
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
if [ "${failed}" -ne 0 ]; then
  printf 'gate: RED\n'
  exit 1
fi
printf 'gate: green\n'

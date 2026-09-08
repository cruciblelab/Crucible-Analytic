#!/usr/bin/env sh
# Runs one fuzz target, and tells a finding apart from the clock.
#
#   release/fuzz.sh <target> <package> <fuzztime>
#   release/fuzz.sh 'FuzzStripComments' ./internal/schemaver/ 5m
#
# # The night this was written after
#
# nightly #18 went red on this line, after sixty-five minutes:
#
#   --- FAIL: FuzzAFileThisBuildWroteIsAFileThisBuildCanRead (301.01s)
#       context deadline exceeded
#
# No failing input, no crasher written, nothing added to testdata/fuzz -
# and 301.01s is the 5m budget, to the hundredth. The fuzzer found
# nothing. It ran out of time, and the toolchain reported running out of
# time as a failure.
#
# That is a race inside `go test -fuzz` rather than anything this
# repository does. The coordinator sets a deadline on one context,
# derives the workers' context from it, and when it stops it suppresses
# the error only if it is *identical* to the derived context's error:
#
#   if err == fuzzCtx.Err() || isInterruptError(err) { err = nil }
#
# (go/src/internal/fuzz/fuzz.go). At the moment the deadline fires there
# is a window where the parent already reports DeadlineExceeded and the
# child has not been told yet, so the comparison is against nil, the
# error survives, and a run that found nothing exits 1. Other projects
# hit the same thing and wrap the command the same way; the Go tree
# carries its own flaky-test issues for the scripts that cover it.
#
# # Why this is worth a script rather than a shrug
#
# A red nightly that is not a defect is worse than no nightly. The three
# nights this repository already spent on a log that could not name its
# own cause are the reason: a check people learn to re-run without
# reading has stopped being a check.
#
# # What it will not do
#
# It never swallows a finding. A crasher is not quiet: `go test` writes
# the input under testdata/fuzz/ and prints "Failing input written to".
# Both are checked, and either one is a failure no matter what else the
# log says. So is any failure that is not the deadline, and so is a
# deadline that arrives early - a seed corpus failing in the first
# second also says "--- FAIL", and it says it at 0.00s rather than at
# the end of the budget.
set -u

say() { printf '%s\n' "$*"; }
die() { printf '!! %s\n' "$*" >&2; exit 1; }

[ "$#" -eq 3 ] || die "usage: $0 <target> <package> <fuzztime>"

target="$1"
package="$2"
fuzztime="$3"

# The corpus directory for this package, which is where a crasher lands.
corpus="$(printf '%s' "${package}" | sed 's|^\./||; s|/*$||')/testdata/fuzz"

corpus_files() {
  [ -d "${corpus}" ] || return 0
  find "${corpus}" -type f 2>/dev/null | LC_ALL=C sort
}

# seconds turns Go's duration spelling into a number, for the one
# comparison below that needs it. Only the shapes this repository writes
# are understood; anything else is an error rather than a guess, because
# a misread budget would make the deadline check accept a failure that
# arrived far too early.
seconds() {
  case "$1" in
    *h) printf '%s' "$(( ${1%h} * 3600 ))" ;;
    *m) printf '%s' "$(( ${1%m} * 60 ))" ;;
    *s) printf '%s' "${1%s}" ;;
    *[!0-9]*) die "cannot read a fuzz budget from $1" ;;
    *) printf '%s' "$1" ;;
  esac
}

budget="$(seconds "${fuzztime}")" || exit 1
[ "${budget}" -gt 0 ] 2>/dev/null || die "the fuzz budget ${fuzztime} is not a positive duration"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT INT TERM
log="${work}/log"
code="${work}/code"

before="$(corpus_files)"

say "== fuzzing ${target} in ${package} for ${fuzztime}"
# The status is written from inside the pipeline so the output can still
# stream: a five-minute step that prints nothing until it ends is a step
# nobody can tell from a hung one. `set -o pipefail` is not in POSIX sh,
# hence the file.
{ "${GO:-go}" test -run XXX -fuzz "${target}\$" -fuzztime "${fuzztime}" "${package}" 2>&1
  printf '%s' "$?" >"${code}"
} | tee "${log}"
status="$(cat "${code}" 2>/dev/null || printf '1')"

if [ "${status}" -eq 0 ]; then
  exit 0
fi

after="$(corpus_files)"
if [ "${before}" != "${after}" ]; then
  say ""
  say "!! the fuzzer wrote a regression case - this is a finding, not the clock:"
  printf '%s\n' "${after}" | grep -vxF "${before:-}" || true
  exit 1
fi

if grep -q 'Failing input written to' "${log}"; then
  say ""
  say "!! the fuzzer recorded a failing input - this is a finding, not the clock"
  exit 1
fi

# One failure, and it is the deadline, and it arrived when the budget
# ran out rather than at the start.
failures="$(grep -c '^--- FAIL' "${log}" || true)"
elapsed="$(sed -n 's/^--- FAIL: [A-Za-z0-9_]* (\([0-9][0-9.]*\)s).*/\1/p' "${log}" | head -1)"

if [ "${failures}" = "1" ] &&
   grep -q 'context deadline exceeded' "${log}" &&
   [ -n "${elapsed}" ] &&
   awk -v e="${elapsed}" -v b="${budget}" 'BEGIN { exit !(e >= b * 0.9) }'; then
  say ""
  say "== ${target} ran its whole ${fuzztime} and found nothing."
  say "   \`go test -fuzz\` reported its own deadline as a failure (${elapsed}s of a"
  say "   ${budget}s budget, no failing input, nothing written to ${corpus})."
  say "   Treated as a pass; see the comment at the top of release/fuzz.sh."
  exit 0
fi

say ""
say "!! ${target} failed for a reason that is not the deadline"
exit "${status}"

#!/usr/bin/env bash
# check-build-token.sh -- assert the dashboard build token is bumped in lockstep.
#
# The PWA shell version lives in three production constants that MUST agree, plus
# a handful of tests/verifiers that pin the same literal. Bumping some and not
# others is the single most repeated mistake in this app: the service worker then
# keeps serving the stale shell, or the dashboard flags itself "stale" forever.
# The failure is silent at build time and only shows up on a device, so it is
# worth a dedicated check with a clear message rather than an obscure assertion
# failure three tests deep.
#
# Some older tokens are INTENTIONAL fixtures, not stale bumps -- verify-dashboard
# feeds a deliberately-mismatched server/client build to prove the staleness
# banner renders, and several tests use a frozen historical build in golden
# output. Those are allowlisted below; anything else that is not the current
# token is a half-finished bump.
#
# Usage:  ./check-build-token.sh          (from the module directory)
# Exits non-zero with an explanation on any mismatch.

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

# Historical tokens that are deliberately NOT the current build.
#   v99 / v80 -> verify-dashboard.mjs stale-server / stale-client banner fixtures
#   v90       -> verify-dashboard.mjs old service-worker cache key to be evicted
#   v97 / v77 -> frozen builds in settings/serve golden output and release fixtures
ALLOWED_HISTORICAL=(v77 v80 v90 v97 v99)

fail() { printf '\033[0;31mFAIL\033[0m %s\n' "$1" >&2; failed=1; }
failed=0

# --- 1. the three production constants must agree ----------------------------
read_token() {
  # $1 = file, $2 = grep pattern; prints the sysmon-static-vN it declares
  grep -oE 'sysmon-static-v[0-9]+' <(grep -E "$2" "$1" | head -1) | head -1
}

status_token=$(read_token status.go 'const dashboardBuild')
app_token=$(read_token static/app.js 'const dashboardBuild')
sw_token=$(read_token static/sw.js 'const STATIC_CACHE')

for pair in "status.go:$status_token" "static/app.js:$app_token" "static/sw.js:$sw_token"; do
  if [ -z "${pair#*:}" ]; then
    fail "could not read a build token from ${pair%%:*}"
  fi
done
[ "$failed" -eq 0 ] || exit 1

TOKEN="$status_token"
if [ "$app_token" != "$TOKEN" ] || [ "$sw_token" != "$TOKEN" ]; then
  fail "production constants disagree -- these three must be bumped together:
    status.go      $status_token
    static/app.js  $app_token
    static/sw.js   $sw_token"
  exit 1
fi

# --- 2. no file may carry a token that is neither current nor allowlisted -----
# This is what catches a half-finished bump: a pinned assertion or POST body left
# on the previous build.
allow_re="$(IFS='|'; printf '%s' "${ALLOWED_HISTORICAL[*]}")"
while IFS= read -r line; do
  file="${line%%:*}"
  tok="${line##*:}"
  ver="${tok#sysmon-static-}"
  [ "$tok" = "$TOKEN" ] && continue
  if [[ "$ver" =~ ^($allow_re)$ ]]; then
    continue
  fi
  fail "$file pins $tok but the current build is $TOKEN (unfinished lockstep bump)"
done < <(
  grep -rhoE --include='*.go' --include='*.js' --include='*.mjs' \
    'sysmon-static-v[0-9]+' . 2>/dev/null |
    sort -u |
    while read -r t; do
      for f in $(grep -rlF "$t" --include='*.go' --include='*.js' --include='*.mjs' . 2>/dev/null); do
        printf '%s:%s\n' "$f" "$t"
      done
    done
)

[ "$failed" -eq 0 ] || exit 1
printf '\033[0;32mok\033[0m dashboard build token %s is consistent across all sites\n' "$TOKEN"

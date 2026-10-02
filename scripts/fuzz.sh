#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
# Runs every fuzz target for FUZZTIME (default 15s each). Crashes are written to testdata/fuzz/ by the Go tool;
# commit those files: they become permanent regression tests.
set -eu
FUZZTIME="${FUZZTIME:-15s}"
status=0
for pkg in $(go list ./...); do
  for target in $(go test -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz' || true); do
    printf '%-70s ' "$pkg $target"
    if out=$(go test "$pkg" -run '^$' -fuzz "^${target}\$" -fuzztime "$FUZZTIME" 2>&1); then
      echo "ok  ($(printf '%s' "$out" | grep -o 'execs: [0-9]*' | tail -1))"
    else
      echo "FAIL"; printf '%s\n' "$out" | tail -15; status=1
    fi
  done
done
exit $status

#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
# Enforces NFR-7 on a coverage profile: per-package floors, and 100 % on the cleanup safety predicate.
#   go test -coverprofile=coverage.out ./... && ./scripts/coverage-gate.sh coverage.out
set -eu
profile="${1:-coverage.out}"
fail=0

# package floor (percent); anything not listed uses DEFAULT_FLOOR
DEFAULT_FLOOR="${DEFAULT_FLOOR:-75}"
floor_for() {
  case "$1" in
    */internal/cleanup)    echo 90 ;;
    */internal/askpass)    echo 90 ;;
    */internal/ratelimit)  echo 90 ;;
    */internal/secrets)    echo 85 ;;
    */internal/clock|*/internal/audit|*/internal/obs) echo 85 ;;
    */internal/gitxtest|*/internal/provider/providertest|*/cmd/repo-keeper-tray) echo 0 ;; # test fixtures / thin GUI glue
    */cmd/repo-keeper) echo 70 ;; # CLI glue; main() itself is not unit-testable
    */internal/paths) echo 70 ;;  # Windows/macOS branches cannot run on the Linux CI leg
    *) echo "$DEFAULT_FLOOR" ;;
  esac
}

awk 'NR>1 { split($1, a, ":"); n=split(a[1], p, "/"); pkg=p[1]; for(i=2;i<n;i++) pkg=pkg "/" p[i];
            stmts[pkg]+=$2; if ($3>0) cov[pkg]+=$2 }
     END { for (k in stmts) printf "%s %.1f\n", k, cov[k]*100/stmts[k] }' "$profile" | sort > /tmp/rk-cover.pkgs

while read -r pkg pct; do
  floor=$(floor_for "$pkg")
  if awk -v a="$pct" -v b="$floor" 'BEGIN{exit !(a+0 < b+0)}'; then
    echo "FAIL $pkg: ${pct}% < ${floor}%"; fail=1
  else
    printf 'ok   %-62s %5s%% (floor %s%%)\n' "$pkg" "$pct" "$floor"
  fi
done < /tmp/rk-cover.pkgs

# the deletion safety predicate must be fully covered
go tool cover -func="$profile" | grep 'internal/cleanup/decide.go' > /tmp/rk-cover.funcs
while read -r _ fn pct; do
  [ "$pct" = "100.0%" ] || { echo "FAIL cleanup predicate $fn at $pct (must be 100.0%)"; fail=1; }
done < /tmp/rk-cover.funcs
[ "$fail" = 0 ] && echo "coverage gate passed"
exit $fail

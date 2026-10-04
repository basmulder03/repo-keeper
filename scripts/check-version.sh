#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# One version, one place: VERSION. Fails when anything disagrees with it.
#   scripts/check-version.sh            consistency of the tracked files (CI)
#   scripts/check-version.sh v0.1.0-x   same, plus: the tag is v$(VERSION) and the changelog heading is final (release)
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "check-version: $*" >&2; exit 1; }

[[ -f VERSION ]] || fail "VERSION is missing"
v=$(<VERSION)
[[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || fail "VERSION '$v' is not a semantic version"

# first release heading of the changelog (a leading "Unreleased" section is allowed between releases)
first=$(grep -m1 -E '^## ' CHANGELOG.md | sed 's/^## //')
if [[ "$first" == "Unreleased" ]]; then
  [[ -z "${1:-}" ]] || fail "CHANGELOG.md still has an Unreleased section; run scripts/release-prep.sh <version> before tagging"
  first=$(grep -E '^## ' CHANGELOG.md | sed -n '2p' | sed 's/^## //')
fi
[[ "${first%% *}" == "$v" ]] || fail "VERSION is $v but the newest changelog release is '${first%% *}'"

# no other tracked copy of a version number in the packaging
if grep -nE '[0-9]+\.[0-9]+\.[0-9]+-?[0-9A-Za-z.]*' flake.nix | grep -vE 'go-version|nixpkgs|http|vendorHash|#' >/dev/null; then
  fail "flake.nix contains a hardcoded version; it must read ./VERSION"
fi
grep -q 'builtins.readFile ./VERSION' flake.nix || fail "flake.nix does not read ./VERSION"

if [[ -n "${1:-}" ]]; then
  [[ "$1" == "v$v" ]] || fail "tag is $1 but VERSION is $v (expected v$v)"
fi
echo "version $v is consistent"

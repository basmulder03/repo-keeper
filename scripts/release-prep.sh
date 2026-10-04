#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Prepare a release in the working tree: VERSION and the changelog heading. Usage: scripts/release-prep.sh 0.1.0-beta.6
set -euo pipefail
cd "$(dirname "$0")/.."

new="${1:-}"
[[ "$new" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || { echo "usage: $0 <semver, e.g. 0.1.0-beta.6>" >&2; exit 2; }
old=$(<VERSION)
[[ "$new" != "$old" ]] || { echo "release-prep: $new is already the current version" >&2; exit 1; }
grep -q '^## Unreleased$' CHANGELOG.md || { echo "release-prep: CHANGELOG.md has no '## Unreleased' section to release" >&2; exit 1; }
[[ "$(sed -n '/^## Unreleased$/,/^## /p' CHANGELOG.md | grep -cE '^- ')" -gt 0 ]] || { echo "release-prep: the Unreleased section has no entries" >&2; exit 1; }

printf '%s\n' "$new" > VERSION
sed -i "0,/^## Unreleased\$/s//## $new/" CHANGELOG.md
scripts/check-version.sh "v$new"
cat <<MSG

Prepared $old -> $new. Next:
  git checkout -b chore/release-$new && git commit -am "chore: release $new" && git push -u origin HEAD
  open the PR, merge it, then: git checkout main && git pull && git tag -a v$new -m $new && git push origin v$new
MSG

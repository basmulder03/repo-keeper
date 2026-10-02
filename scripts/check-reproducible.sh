#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
# Builds the snapshot release twice and fails if any archive, package or binary differs between the builds.
set -eu
SOURCE_DATE_EPOCH=$(git log -1 --format=%ct)
export SOURCE_DATE_EPOCH
sums() {
  goreleaser release --snapshot --clean --skip=sign,sbom,publish >/dev/null
  (cd dist && sha256sum repo-keeper_linux_*/repo-keeper repo-keeper-tray_linux_*/repo-keeper-tray repo-keeper_*_linux_*.tar.gz repo-keeper_*_linux_*.deb repo-keeper_*_linux_*.rpm)
}
first=$(sums)
sleep 2 # a different wall clock must not matter
second=$(sums)
if [ "$first" != "$second" ]; then
  echo "NOT reproducible:" >&2
  printf '%s\n' "$first" > /tmp/rk-build-1.sums
  printf '%s\n' "$second" > /tmp/rk-build-2.sums
  diff /tmp/rk-build-1.sums /tmp/rk-build-2.sums >&2 || true
  exit 1
fi
echo "reproducible: $(printf '%s\n' "$first" | wc -l) artifacts identical across two builds"

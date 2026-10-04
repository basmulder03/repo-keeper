#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Exercises scripts/install.sh against a synthetic release served from localhost, with a stand-in cosign.
# (The real signature path was checked by hand against a real signed release; CI cannot download draft assets.)
set -euo pipefail
cd "$(dirname "$0")/.."
INSTALL="$PWD/scripts/install.sh"
T=$(mktemp -d)
trap 'kill $(cat "$T/pid" 2>/dev/null) 2>/dev/null || true; rm -rf "$T"' EXIT

V=1.2.3
fails=0
ok() { echo "ok   $*"; }
bad() { echo "FAIL $*"; fails=$((fails + 1)); }

# --- a synthetic release
mkdir -p "$T/srv/v$V" "$T/pkg/systemd" "$T/bin"
printf '#!/bin/sh\necho "repo-keeper fake %s"\n' "$V" > "$T/pkg/repo-keeper"
printf '#!/bin/sh\necho tray\n' > "$T/pkg/repo-keeper-tray"
printf '[Service]\nExecStart=%%h/.local/bin/repo-keeper daemon\n' > "$T/pkg/systemd/repo-keeper.service"
chmod +x "$T/pkg/repo-keeper" "$T/pkg/repo-keeper-tray"
arch=amd64; [ "$(uname -m)" = aarch64 ] && arch=arm64
ARCHIVE="repo-keeper_${V}_linux_${arch}.tar.gz"
tar -czf "$T/srv/v$V/$ARCHIVE" -C "$T/pkg" repo-keeper repo-keeper-tray systemd
( cd "$T/srv/v$V" && sha256sum "$ARCHIVE" > checksums.txt )
echo sig > "$T/srv/v$V/checksums.txt.sig"; echo pem > "$T/srv/v$V/checksums.txt.pem"
cp -r "$T/srv/v$V" "$T/good"   # pristine copy for resets

# --- a stand-in cosign that records its arguments and obeys COSIGN_RC
mkdir -p "$T/shim"
cat > "$T/shim/cosign" <<'SH'
#!/bin/sh
echo "$@" > "${COSIGN_LOG:-/dev/null}"
exit "${COSIGN_RC:-0}"
SH
chmod +x "$T/shim/cosign"

# --- local server
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
( cd "$T/srv" && exec python3 -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1 ) &
echo $! > "$T/pid"
for _ in $(seq 1 50); do curl -fs "http://127.0.0.1:$port/v$V/checksums.txt" >/dev/null 2>&1 && break; sleep 0.1; done
export REPO_KEEPER_BASE_URL="http://127.0.0.1:$port"

reset_release() { rm -rf "${T:?}/srv/v$V"; cp -r "$T/good" "$T/srv/v$V"; }
# run NAME EXPECTED_RC [env...] -- args...   (fresh HOME each time; output in $OUT, home in $H)
run() {
  local name=$1 want=$2; shift 2
  H=$(mktemp -d -p "$T")
  local envs=() 
  while [ "$1" != "--" ]; do envs+=("$1"); shift; done; shift
  set +e
  OUT=$(env HOME="$H" "${envs[@]}" PATH="$T/shim:$PATH" sh "$INSTALL" "$@" 2>&1); rc=$?
  set -e
  if [ "$rc" -eq "$want" ]; then ok "$name (rc=$rc)"; else bad "$name: rc=$rc, want $want"; echo "$OUT" | sed 's/^/     /'; fi
}
expect_out() { echo "$OUT" | grep -q -- "$1" && ok "  says: $1" || { bad "  output lacks: $1"; echo "$OUT" | sed 's/^/     /'; }; }
expect_none() { [ -z "$(ls "$H/.local/bin" 2>/dev/null)" ] && ok "  nothing installed" || bad "  something was installed: $(ls "$H/.local/bin")"; }

# 1 happy path
reset_release
COSIGN_LOG="$T/cosign.log" run "verified install" 0 COSIGN_RC=0 COSIGN_LOG="$T/cosign.log" -- --version "$V"
expect_out "checksum and signature verified"
[ -x "$H/.local/bin/repo-keeper" ] && [ -x "$H/.local/bin/repo-keeper-tray" ] && ok "  binaries installed" || bad "  binaries missing"
[ -f "$H/.config/systemd/user/repo-keeper.service" ] && ok "  units copied (not enabled)" || bad "  units missing"
grep -q -- "--certificate-identity https://github.com/basmulder03/repo-keeper/.github/workflows/release.yml@refs/tags/v$V" "$T/cosign.log" && ok "  identity pinned to this exact tag" || bad "  identity not pinned: $(cat "$T/cosign.log")"
grep -q -- "--certificate-oidc-issuer https://token.actions.githubusercontent.com" "$T/cosign.log" && ok "  issuer pinned" || bad "  issuer not pinned"

# 2 'v' prefix accepted; 3 --no-units; 4 custom prefix
run "v-prefixed version" 0 COSIGN_RC=0 -- --version "v$V"
run "--no-units" 0 COSIGN_RC=0 -- --version "$V" --no-units
[ ! -e "$H/.config/systemd/user/repo-keeper.service" ] && ok "  no units" || bad "  units copied despite --no-units"
run "custom prefix" 0 COSIGN_RC=0 -- --version "$V" --prefix "$T/custom"
[ -x "$T/custom/bin/repo-keeper" ] && [ ! -e "$H/.config/systemd/user/repo-keeper.service" ] && ok "  installed under the prefix, units left alone" || bad "  prefix handling wrong"

# 5 signature failure installs nothing
run "bad signature" 1 COSIGN_RC=1 -- --version "$V"
expect_out "signature verification FAILED"; expect_none

# 6 tampered archive; 7 archive missing from checksums
reset_release; printf X >> "$T/srv/v$V/$ARCHIVE"
run "tampered archive" 1 COSIGN_RC=0 -- --version "$V"
expect_out "SHA-256 mismatch"; expect_none
reset_release; : > "$T/srv/v$V/checksums.txt"
run "archive not listed" 1 COSIGN_RC=0 -- --version "$V"
expect_out "not listed in checksums.txt"; expect_none
reset_release

# 8 no cosign => refuse; --skip-signature => warn and proceed
mkdir -p "$T/nocosign"; for t in sh curl tar gzip awk cut mktemp rm cp chmod mv mkdir uname sha256sum sed grep cat ls env; do p=$(command -v "$t" || true); [ -n "$p" ] && ln -sf "$p" "$T/nocosign/$t"; done
H=$(mktemp -d -p "$T"); set +e; OUT=$(env HOME="$H" PATH="$T/nocosign" "$T/nocosign/sh" "$INSTALL" --version "$V" 2>&1); rc=$?; set -e
[ "$rc" -eq 1 ] && echo "$OUT" | grep -q "cosign is required" && ok "missing cosign is refused" || { bad "missing cosign: rc=$rc"; echo "$OUT" | sed 's/^/     /'; }
H=$(mktemp -d -p "$T"); set +e; OUT=$(env HOME="$H" PATH="$T/nocosign" "$T/nocosign/sh" "$INSTALL" --version "$V" --skip-signature 2>&1); rc=$?; set -e
[ "$rc" -eq 0 ] && echo "$OUT" | grep -q "WARNING: skipping the signature check" && echo "$OUT" | grep -q "signature NOT checked" && ! echo "$OUT" | grep -q "and signature verified" && [ -x "$H/.local/bin/repo-keeper" ] && ok "--skip-signature warns loudly and proceeds" || { bad "--skip-signature: rc=$rc"; echo "$OUT" | sed 's/^/     /'; }

# 9 inputs
REPO_KEEPER_BASE_URL=http://example.com run "non-local http mirror" 1 REPO_KEEPER_BASE_URL=http://example.com COSIGN_RC=0 -- --version "$V"
expect_out "refusing a non-https download URL"
run "unknown release" 1 COSIGN_RC=0 -- --version 9.9.9
expect_out "is v9.9.9 a published release"; expect_none
run "injection attempt in version" 1 COSIGN_RC=0 -- --version '1.0;touch pwned'
expect_out "unusable version"; [ ! -e "$T/pwned" ] && [ ! -e pwned ] && ok "  nothing executed" || bad "  injection executed"
run "no version pinned" 1 COSIGN_RC=0 -- 
expect_out "no version pinned"
run "unknown option" 1 COSIGN_RC=0 -- --nope
expect_out "unknown option"

# 10 unsupported OS and CPU (uname shim)
mkdir -p "$T/uname-shim"
printf '#!/bin/sh\ncase "$1" in -s) echo "${FAKE_S:-Linux}";; -m) echo "${FAKE_M:-x86_64}";; esac\n' > "$T/uname-shim/uname"; chmod +x "$T/uname-shim/uname"
H=$(mktemp -d -p "$T"); set +e; OUT=$(env HOME="$H" FAKE_S=Darwin PATH="$T/uname-shim:$T/shim:$PATH" sh "$INSTALL" --version "$V" 2>&1); rc=$?; set -e
[ "$rc" -eq 1 ] && echo "$OUT" | grep -q "Linux only" && ok "macOS is refused with an explanation" || bad "macOS: rc=$rc"
H=$(mktemp -d -p "$T"); set +e; OUT=$(env HOME="$H" FAKE_M=riscv64 PATH="$T/uname-shim:$T/shim:$PATH" sh "$INSTALL" --version "$V" 2>&1); rc=$?; set -e
[ "$rc" -eq 1 ] && echo "$OUT" | grep -q "unsupported CPU" && ok "unsupported CPU is refused" || bad "cpu: rc=$rc"

# 11 upgrade replaces the binaries in place
H=$(mktemp -d -p "$T"); mkdir -p "$H/.local/bin"; echo old > "$H/.local/bin/repo-keeper"
set +e; OUT=$(env HOME="$H" PATH="$T/shim:$PATH" COSIGN_RC=0 sh "$INSTALL" --version "$V" 2>&1); rc=$?; set -e
leftovers=$(find "$H/.local/bin" -name ".*" -type f | wc -l)
[ "$rc" -eq 0 ] && "$H/.local/bin/repo-keeper" | grep -q "fake $V" && [ "$leftovers" -eq 0 ] && ok "upgrade replaced the old binary, no temp files left" || { bad "upgrade: rc=$rc"; echo "$OUT" | sed 's/^/     /'; }

echo
[ "$fails" -eq 0 ] && echo "all install tests passed" || { echo "$fails install test(s) FAILED"; exit 1; }

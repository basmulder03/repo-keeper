#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# repo-keeper installer for Linux (amd64, arm64). It downloads one pinned release, verifies the cosign signature of
# checksums.txt and the SHA-256 of the archive, and only then installs. Nothing is installed if any check fails.
#
#   sh install.sh [--version X.Y.Z] [--prefix DIR] [--no-units] [--skip-signature]
#
# --prefix     default ~/.local (binaries go to DIR/bin). The systemd user units expect ~/.local/bin.
# --no-units   do not copy the systemd user units into ~/.config/systemd/user.
# --skip-signature  only for machines without cosign: the SHA-256 is still checked, but the checksum file itself is then
#              only as trustworthy as the download connection. Prefer installing cosign.
# Environment: REPO_KEEPER_VERSION, REPO_KEEPER_BASE_URL (a mirror or a local test server; the signature identity stays pinned).
set -eu

VERSION="${REPO_KEEPER_VERSION:-@VERSION@}"
BASE="${REPO_KEEPER_BASE_URL:-https://github.com/basmulder03/repo-keeper/releases/download}"
PREFIX="${HOME}/.local"
UNITS=1
SKIP_SIG=0

die() { echo "install: $*" >&2; exit 1; }
say() { echo "install: $*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
    --prefix) [ $# -ge 2 ] || die "--prefix needs a value"; PREFIX="$2"; shift 2 ;;
    --no-units) UNITS=0; shift ;;
    --skip-signature) SKIP_SIG=1; shift ;;
    -h|--help) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option $1 (try --help)" ;;
  esac
done

case "$VERSION" in
  @VERSION@|"") die "no version pinned in this copy of the script; pass --version X.Y.Z (see https://github.com/basmulder03/repo-keeper/releases)" ;;
esac
VERSION="${VERSION#v}"
case "$VERSION" in
  *[!0-9A-Za-z.-]*|-*|.*) die "unusable version '$VERSION'" ;;
esac

[ "$(uname -s)" = "Linux" ] || die "this installer supports Linux only; macOS and Windows builds are not published yet (see the install page for Nix)"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU $(uname -m) (amd64 and arm64 are published)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
if command -v sha256sum >/dev/null 2>&1; then
  sum() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sum() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "sha256sum (or shasum) is required"
fi

if [ "$SKIP_SIG" -eq 0 ]; then
  command -v cosign >/dev/null 2>&1 || die "cosign is required to verify the release signature (https://docs.sigstore.dev/cosign/system_config/installation/); or rerun with --skip-signature to rely on the SHA-256 alone"
else
  say "WARNING: skipping the signature check; only the SHA-256 from the download itself will be verified"
fi

TAG="v$VERSION"
ARCHIVE="repo-keeper_${VERSION}_linux_${ARCH}.tar.gz"
URL="$BASE/$TAG"
case "$URL" in
  https://*) PROTO="=https" ;;
  http://127.0.0.1*|http://localhost*) PROTO="=http" ;; # a local test server only
  *) die "refusing a non-https download URL: $URL" ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM
fetch() { curl --fail --silent --show-error --location --proto "$PROTO" --proto-redir "=https" --tlsv1.2 --retry 2 --output "$TMP/$1" "$URL/$1" || die "could not download $URL/$1 (is $TAG a published release?)"; }

say "downloading $TAG for linux/$ARCH"
fetch checksums.txt
fetch "$ARCHIVE"

if [ "$SKIP_SIG" -eq 0 ]; then
  fetch checksums.txt.sig
  fetch checksums.txt.pem
  say "verifying the signature of checksums.txt"
  cosign verify-blob "$TMP/checksums.txt" \
    --certificate "$TMP/checksums.txt.pem" --signature "$TMP/checksums.txt.sig" \
    --certificate-identity "https://github.com/basmulder03/repo-keeper/.github/workflows/release.yml@refs/tags/$TAG" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com >/dev/null 2>&1 \
    || die "signature verification FAILED: not installing"
fi

want="$(awk -v f="$ARCHIVE" '$2 == f { print $1 }' "$TMP/checksums.txt")"
[ -n "$want" ] || die "$ARCHIVE is not listed in checksums.txt: not installing"
got="$(sum "$TMP/$ARCHIVE")"
[ "$want" = "$got" ] || die "SHA-256 mismatch for $ARCHIVE (expected $want, got $got): not installing"
if [ "$SKIP_SIG" -eq 0 ]; then say "checksum and signature verified"; else say "checksum verified (signature NOT checked)"; fi

mkdir "$TMP/x"
tar -xzf "$TMP/$ARCHIVE" -C "$TMP/x" repo-keeper repo-keeper-tray systemd 2>/dev/null || tar -xzf "$TMP/$ARCHIVE" -C "$TMP/x" || die "could not unpack $ARCHIVE"
[ -f "$TMP/x/repo-keeper" ] || die "the archive does not contain repo-keeper"

BIN="$PREFIX/bin"
mkdir -p "$BIN"
for f in repo-keeper repo-keeper-tray; do
  [ -f "$TMP/x/$f" ] || continue
  cp "$TMP/x/$f" "$BIN/.$f.new" && chmod 755 "$BIN/.$f.new" && mv -f "$BIN/.$f.new" "$BIN/$f" # replace atomically, even while it runs
done
say "installed to $BIN"

if [ "$UNITS" -eq 1 ] && [ "$PREFIX" = "$HOME/.local" ] && [ -d "$TMP/x/systemd" ]; then
  UD="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$UD"
  for u in "$TMP"/x/systemd/*.service; do cp "$u" "$UD/"; done
  say "systemd user units copied to $UD (not enabled)"
fi

case ":$PATH:" in *":$BIN:"*) ;; *) say "note: $BIN is not on your PATH" ;; esac
"$BIN/repo-keeper" version || true
cat <<MSG

Next:
  repo-keeper init
  repo-keeper accounts add <name> --token-stdin --include 'you/*' < token.txt
  repo-keeper discover
  systemctl --user daemon-reload && systemctl --user enable --now repo-keeper    # or: repo-keeper start
MSG

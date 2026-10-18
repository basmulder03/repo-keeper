# SPDX-License-Identifier: Apache-2.0
# repo-keeper installer for Windows (amd64, arm64). It downloads one pinned release, verifies the cosign signature of
# checksums.txt and the SHA-256 of the archive, and only then installs. Nothing is installed if any check fails.
#
#   .\install.ps1 [-Version X.Y.Z] [-Prefix DIR] [-SkipSignature]
#
# -Version         default: the latest published release (GitHub's "latest", which skips drafts and pre-releases).
# -Prefix           default %LOCALAPPDATA%\Programs\repo-keeper; its bin folder is added to the user PATH.
# -SkipSignature    only for machines without cosign: the SHA-256 is still checked, but the checksum file itself is then
#                   only as trustworthy as the download connection. Prefer installing cosign.
# Environment: REPO_KEEPER_VERSION, REPO_KEEPER_BASE_URL (a mirror or a local test server; the signature identity stays pinned).
[CmdletBinding()]
param(
  [string]$Version = $env:REPO_KEEPER_VERSION,
  [string]$Prefix = (Join-Path $env:LOCALAPPDATA 'Programs\repo-keeper'),
  [switch]$SkipSignature
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with the progress bar

function Fail($msg) { throw "install: $msg" }
function Say($msg) { Write-Host "install: $msg" }

$Base = if ($env:REPO_KEEPER_BASE_URL) { $env:REPO_KEEPER_BASE_URL } else { 'https://github.com/basmulder03/repo-keeper/releases/download' }

if (-not $Version) {
  if ($env:REPO_KEEPER_BASE_URL) { Fail 'cannot look up the latest release on a mirror; pass -Version X.Y.Z' }
  try {
    $r = Invoke-WebRequest -UseBasicParsing -Method Head -Uri 'https://github.com/basmulder03/repo-keeper/releases/latest'
    # Windows PowerShell 5.1 and PowerShell 7 expose the final URL differently
    $final = if ($r.BaseResponse.ResponseUri) { $r.BaseResponse.ResponseUri.AbsoluteUri } else { $r.BaseResponse.RequestMessage.RequestUri.AbsoluteUri }
  } catch { Fail 'could not look up the latest release; pass -Version X.Y.Z (see https://github.com/basmulder03/repo-keeper/releases)' }
  if ($final -notmatch '/releases/tag/([^/?#]+)$') { Fail 'no stable release is published yet; pass -Version X.Y.Z for a pre-release (see https://github.com/basmulder03/repo-keeper/releases)' }
  $Version = $Matches[1]
  Say "latest release is $Version"
}
$Version = $Version.TrimStart('v')
if ($Version -notmatch '^[0-9A-Za-z][0-9A-Za-z.-]*$') { Fail "unusable version '$Version'" }

$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { 'amd64' }
  'ARM64' { 'arm64' }
  default { Fail "unsupported CPU $($env:PROCESSOR_ARCHITECTURE) (amd64 and arm64 are published)" }
}

if ($Base -notmatch '^https://' -and $Base -notmatch '^http://(127\.0\.0\.1|localhost)') { Fail "refusing a non-https download URL: $Base" }

if (-not $SkipSignature) {
  if (-not (Get-Command cosign -ErrorAction SilentlyContinue)) { Fail 'cosign is required to verify the release signature (https://docs.sigstore.dev/cosign/system_config/installation/); or rerun with -SkipSignature to rely on the SHA-256 alone' }
} else {
  Say 'WARNING: skipping the signature check; only the SHA-256 from the download itself will be verified'
}

$Tag = "v$Version"
$Archive = "repo-keeper_${Version}_windows_$Arch.zip"
$Url = "$Base/$Tag"
$Tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $Tmp | Out-Null

try {
  function Fetch($name) {
    try { Invoke-WebRequest -UseBasicParsing -Uri "$Url/$name" -OutFile (Join-Path $Tmp $name) }
    catch { Fail "could not download $Url/$name (is $Tag a published release?)" }
  }

  Say "downloading $Tag for windows/$Arch"
  Fetch 'checksums.txt'
  Fetch $Archive

  if (-not $SkipSignature) {
    Fetch 'checksums.txt.sig'
    Fetch 'checksums.txt.pem'
    Say 'verifying the signature of checksums.txt'
    & cosign verify-blob (Join-Path $Tmp 'checksums.txt') `
      --certificate (Join-Path $Tmp 'checksums.txt.pem') --signature (Join-Path $Tmp 'checksums.txt.sig') `
      --certificate-identity "https://github.com/basmulder03/repo-keeper/.github/workflows/release.yml@refs/tags/$Tag" `
      --certificate-oidc-issuer https://token.actions.githubusercontent.com *> $null
    if ($LASTEXITCODE -ne 0) { Fail 'signature verification FAILED: not installing' }
  }

  $want = Get-Content (Join-Path $Tmp 'checksums.txt') | ForEach-Object { $f = $_ -split '\s+', 2; if ($f[1] -eq $Archive) { $f[0] } } | Select-Object -First 1
  if (-not $want) { Fail "$Archive is not listed in checksums.txt: not installing" }
  $got = (Get-FileHash -Algorithm SHA256 (Join-Path $Tmp $Archive)).Hash.ToLowerInvariant()
  if ($want.ToLowerInvariant() -ne $got) { Fail "SHA-256 mismatch for $Archive (expected $want, got $got): not installing" }
  if ($SkipSignature) { Say 'checksum verified (signature NOT checked)' } else { Say 'checksum and signature verified' }

  $x = Join-Path $Tmp 'x'
  Expand-Archive -Path (Join-Path $Tmp $Archive) -DestinationPath $x
  if (-not (Test-Path (Join-Path $x 'repo-keeper.exe'))) { Fail 'the archive does not contain repo-keeper.exe' }

  $Bin = Join-Path $Prefix 'bin'
  New-Item -ItemType Directory -Force -Path $Bin | Out-Null
  foreach ($f in 'repo-keeper.exe', 'repo-keeper-tray.exe') {
    $src = Join-Path $x $f
    if (-not (Test-Path $src)) { continue }
    $dst = Join-Path $Bin $f
    # a running exe cannot be overwritten but can be renamed away
    if (Test-Path $dst) { Move-Item -Force $dst "$dst.old" }
    Copy-Item $src $dst
    Remove-Item -Force "$dst.old" -ErrorAction SilentlyContinue
  }
  Say "installed to $Bin"

  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (($userPath -split ';') -notcontains $Bin) {
    [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ";$Bin").TrimStart(';'), 'User')
    Say "added $Bin to your user PATH (open a new terminal to pick it up)"
  }
  & (Join-Path $Bin 'repo-keeper.exe') version

  @'

Next:
  repo-keeper init
  repo-keeper accounts add <name> --token-stdin --include 'you/*' < token.txt
  repo-keeper discover
  repo-keeper install-service    # per-user autostart via Task Scheduler, or: repo-keeper start
'@ | Write-Host
} finally {
  Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue
}

# Distribution, installation & anti-virus friendliness

## Targets
linux/macOS/windows × amd64/arm64, static binaries, built by GoReleaser in GitHub Actions with pinned actions.

## Install channels

Shipped now (Linux only; Windows and macOS binaries are deliberately not published until they can be code-signed, because unsigned executables are exactly what SmartScreen and antivirus engines distrust):

| Channel | Notes |
|---|---|
| Nix flake (`packages`, `apps`, `homeManagerModules.default`, `nixosModules.default`) | First-class; both modules are evaluated and asserted by `nix flake check` (the generated config must pass `repo-keeper config validate`) |
| `.deb` / `.rpm` (amd64, arm64) | Built by GoReleaser/nfpm; user units under `/usr/lib/systemd/user/`, not enabled automatically |
| Tarball (amd64, arm64) | Binaries, LICENSE, install guide, example units |

Planned: Homebrew tap, AUR, winget/Scoop (after signing), container image (headless servers). See [INSTALL](INSTALL.md).

`repo-keeper install-service` / `uninstall-service` register per-user autostart using **official** mechanisms only: systemd `--user`, launchd LaunchAgent, Windows Task Scheduler (per-user, "at logon"). No registry Run-key hacks beyond that, no admin, no drivers, no services running as SYSTEM.

## Why AV might flag us, and what we do about it
Behaviours that heuristics associate with malware: background persistence, reading credential stores, outbound network, spawning `git`, unsigned Go binaries (Go binaries are a common false-positive target).

| Risk | Mitigation |
|---|---|
| Unsigned binary / no reputation | **Authenticode signing** (free for OSS via SignPath Foundation, or an EV/OV cert), macOS Developer ID + notarization, cosign for everything |
| Packers/obfuscation | **Never** use UPX or obfuscators; ship plain, reproducible binaries |
| Suspicious persistence | Documented, user-initiated `install-service`; visible in standard OS UIs; clean uninstall; no self-hiding |
| Credential-store access | Only our own entries via official OS APIs; no enumeration of other apps' secrets |
| Self-modifying/auto-update | Never automatic. Package managers update their own installs; the tarball install may use the explicit, verified `repo-keeper update --apply` (ADR-0021) |
| Downloader behaviour | No downloading/executing code at runtime; `git` is found on PATH, not fetched |
| Unknown hash | Keep build reproducible so the same source ⇒ same hash; stable file names and version resources (company, product, description, icon) embedded in Windows PE |
| Lack of reputation | Submit each release to Microsoft Defender (WDSI) false-positive portal pre-announcement; CI uploads to VirusTotal and fails the release checklist if > 0–2 engines flag (review manually) |
| Behaviour from `git` child processes | Always absolute path to detected `git`, deterministic arguments, no shell (`exec` without `sh -c`/`cmd /c`/PowerShell) |
| Local web server | Loopback only; clearly named process; documented firewall expectation (no inbound) |

Build hygiene: `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w"` only (stripping is OK; packing is not), embed version info, `go version -m` in release notes.

## Supply-chain assurance
SBOM (SPDX/CycloneDX via syft), SLSA provenance (`slsa-github-generator`), cosign keyless signatures, checksums, OpenSSF Scorecard ≥ 7, branch protection + required reviews, Dependabot/Renovate, `go mod verify`, reproducible build check in CI (build twice, diff).

## Uninstall & data
`repo-keeper uninstall-service`, remove binary via package manager. Data dirs (XDG / `~/Library/Application Support` / `%AppData%`) are documented; `repo-keeper purge` removes state and keychain entries but **never** the cloned repositories.

## Versioning & releases
SemVer, Conventional Commits, changelog auto-generated, release PR gate with security checklist.

## What M5 actually verifies

- **Reproducibility:** `make repro` (and the `release-config` CI job) builds the snapshot release twice, with a different wall clock, and fails unless every binary, tarball, `.deb` and `.rpm` is byte-identical. `SOURCE_DATE_EPOCH` (the last commit time) drives all timestamps. One known wrinkle: tarball entries for the two binaries carry the builder's user/group names, so tarballs are identical only on the same account; the binaries inside and the `.deb`/`.rpm` files are identical everywhere.
- **Release pipeline** (`.github/workflows/release.yml`, on `v*` tags): GoReleaser builds, `syft` writes an SPDX SBOM per archive, `cosign` keyless-signs `checksums.txt`, GitHub build-provenance attestations cover archives, packages and checksums, and the GitHub release is created as a **draft** for a human to review and publish. This workflow has not run yet (it needs a pushed tag); its pieces were exercised locally except signing, SBOM and attestation.
- **Service sandbox:** the unit settings are shared by the Nix modules and the package units. `systemd-analyze security` rates the generated unit **1.6 (OK)**; the daemon and the tray were run for real under the full set of restrictions (`MemoryDenyWriteExecute`, `SystemCallFilter=@system-service ~@privileged`, empty capability set, `ProtectSystem=strict`, ...) and synced, served the UI and registered the tray icon.

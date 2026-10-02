# Distribution, installation & anti-virus friendliness

## Targets
linux/macOS/windows × amd64/arm64, static binaries, built by GoReleaser in GitHub Actions with pinned actions.

## Install channels (in priority order)
| Channel | Platforms | Notes |
|---|---|---|
| Homebrew tap | macOS, Linux | Formula + `brew services` |
| winget + Scoop | Windows | Manifest PRs automated |
| `.deb` / `.rpm` | Linux | systemd **user** unit shipped, not enabled by default |
| Nix flake / nixpkgs (**first target**) | NixOS, any | Home-Manager module (`services.repo-keeper`) |
| AUR | Arch | `repo-keeper-bin` |
| Tarball/zip | all | Checksums + cosign signature |
| Container (optional) | Linux | For headless servers; non-root, read-only rootfs |

`repo-keeper install-service` / `uninstall-service` register per-user autostart using **official** mechanisms only: systemd `--user`, launchd LaunchAgent, Windows Task Scheduler (per-user, "at logon"). No registry Run-key hacks beyond that, no admin, no drivers, no services running as SYSTEM.

## Why AV might flag us, and what we do about it
Behaviours that heuristics associate with malware: background persistence, reading credential stores, outbound network, spawning `git`, unsigned Go binaries (Go binaries are a common false-positive target).

| Risk | Mitigation |
|---|---|
| Unsigned binary / no reputation | **Authenticode signing** (free for OSS via SignPath Foundation, or an EV/OV cert), macOS Developer ID + notarization, cosign for everything |
| Packers/obfuscation | **Never** use UPX or obfuscators; ship plain, reproducible binaries |
| Suspicious persistence | Documented, user-initiated `install-service`; visible in standard OS UIs; clean uninstall; no self-hiding |
| Credential-store access | Only our own entries via official OS APIs; no enumeration of other apps' secrets |
| Self-modifying/auto-update | No silent self-update; package managers update |
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

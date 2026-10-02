# Security Policy

## Supported versions
The latest minor release receives security fixes. Pre-1.0: only `main` and the latest tag.

## Reporting a vulnerability
**Do not open a public issue.** Use GitHub's *Private vulnerability reporting* (Security tab → "Report a vulnerability") on this repository, or email the maintainer listed in the repo profile. Please include version, OS, reproduction steps and impact.

- Acknowledgement within **3 business days**, triage within **7 days**.
- Fix target: critical ≤ 14 days, high ≤ 30 days; coordinated disclosure, credit given unless you prefer otherwise.
- Good-faith research that respects user data and provider ToS is welcome; no DoS or testing against providers' production APIs beyond normal use.

## Security guarantees (design)
- Credentials live only in the OS keychain (or passphrase-encrypted file) and are never logged, written to config, or put on command lines.
- The UI listens on loopback only (free port, discovered via a `0600` runtime file), is entered through a one-time login URL, and requires session cookie + CSRF protection.
- Git runs with hooks and external-command config disabled; repo content is untrusted.
- Local data is only ever deleted recoverably (trash refs) and never force-pushed or force-overwritten.
- No telemetry.

See [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md).

## Verifying releases
Each release ships `checksums.txt`, a cosign signature/certificate, an SBOM (SPDX) and SLSA provenance. Verify with `cosign verify-blob` (exact commands in release notes).

## For contributors
No secrets in code, tests or fixtures (use canary values). Run `make security` (govulncheck, gosec, license check) before opening a PR.

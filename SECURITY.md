# Security Policy

## Supported versions
The latest minor release receives security fixes. Pre-1.0: only `main` and the latest tag.

## Reporting a vulnerability
**Do not open a public issue.** Report privately through GitHub: **<https://github.com/basmulder03/repo-keeper/security/advisories/new>** (Security tab → "Report a vulnerability"). Only the maintainers can see the report. Please include the version (`repo-keeper version`), OS, reproduction steps and impact.

Policy and past advisories: <https://github.com/basmulder03/repo-keeper/security>. For how releases are signed and verified see [docs/INSTALL.md](docs/INSTALL.md#verifying-a-release); the design and the self-review are in [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) and [docs/SECURITY-REVIEW.md](docs/SECURITY-REVIEW.md).

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

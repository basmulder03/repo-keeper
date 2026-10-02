# Changelog

All notable changes are listed here. Versions follow [SemVer](https://semver.org); `0.x` means the configuration file, CLI and UI may still change between releases (breaking changes are called out).

## 0.1.0-beta.1 (first beta)

First version meant for real-world testing. **Linux only.** GitHub and GitLab only. Read [docs/BETA.md](docs/BETA.md) before pointing it at accounts you care about.

### What it does
- Keeps local clones of your repositories up to date: fetches every branch, detects the default branch (including renames such as `master` → `main`) and fast-forwards it. It never merges, rebases, forces or touches a dirty working tree.
- Optionally deletes **local** branches that are already merged (including squash merges, using the platform's PR/MR data). Dry-run by default; every deletion is journaled and recoverable for 30 days. Remote branches are never touched.
- Discovers every repository an account can access (GitHub, GitHub Enterprise Server, GitLab.com and self-managed incl. nested groups), applies include/exclude rules and clones what is missing.
- Respects platform limits: per-host pacing, `Retry-After` and quota headers, backoff, circuit breaker, conditional requests.
- Runs as a user service with a local web UI (`repo-keeper ui`), a CLI, and an optional tray icon.
- Credentials live in the OS keychain, a passphrase-encrypted file, or a protected token file; git receives them through a host-bound helper, never in argv, URLs or config.

### Included
Sync engine, safe cleanup with audit journal and restore, rate limiting, SQLite state, scheduler and daemon, GitHub and GitLab providers, web UI (dashboard, repo detail, accounts, cleanup review, audit, config editor, debug and diagnostics bundle), tray helper, Home Manager and NixOS modules, `.deb`/`.rpm`/tarball packaging with reproducible builds, fuzzing and performance gates, and a self-conducted security review ([docs/SECURITY-REVIEW.md](docs/SECURITY-REVIEW.md)).

### Known limitations
- Not yet exercised against live GitHub/GitLab by the maintainers' CI (all provider behaviour is tested against faithful fakes); expect surprises and report them.
- No Windows or macOS builds. No GitLab device-flow login (use access tokens). Other platforms (Azure DevOps, Bitbucket, Gitea/Forgejo) are not implemented.
- The dashboard lists all repositories on one page (fast at 500, will need pagination at thousands).
- The release pipeline (signing, SBOM, provenance) has not yet run on a real tag; verify checksums yourself for now.
- Provider terms/limits in `docs/PROVIDERS.md` are marked *last verified: TBD*.

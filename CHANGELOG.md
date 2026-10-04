# Changelog

All notable changes are listed here. Versions follow [SemVer](https://semver.org); `0.x` means the configuration file, CLI and UI may still change between releases (breaking changes are called out).

## Unreleased

### Added
- **Install one-liners for Linux**: a verified installer (`install.sh`, published on the documentation site with its SHA-256) that downloads one pinned release, checks the cosign signature (identity pinned to the release workflow and the exact tag) and the archive SHA-256, and installs to `~/.local/bin` only if everything passes; plus Nix commands and ready-to-paste `.deb`/`.rpm` commands. See the Install page.
- **Documentation site** (`tools/site`, `make site`): the project documentation as a static site, with the command-line reference and starter configuration generated from the binary, statistics (size, tests, coverage, dependencies, releases) and release downloads. Built and link-checked by CI, published to GitHub Pages from `main`.

### Changed
- The usage text no longer promises "later milestones" and its command column is aligned.
- Release process: the version now lives in one file (`VERSION`); the Nix package reads it, and CI and the release workflow fail when the tag, `VERSION`, the changelog or the flake disagree. `make release-prep NEW=x.y.z` does the bump (docs/RELEASING.md).

## 0.1.0-beta.5

### Added
- **Generic git accounts** (`provider = "git"`): keep any list of https/ssh clone URLs in sync, with no platform API. Optional credential (SSH keys or public repositories need none), never offered to more than one https host, no passwords inside URLs. Also in the web form and `accounts add --provider git --urls ... --no-credential`. Cleanup uses only what git can prove, since there is no pull-request data.

## 0.1.0-beta.4

### Fixed
- **Gitea, Forgejo and Bitbucket: squash-merged branches were never recognised as merged** in the running daemon (the merged-PR lookup read repository fields the daemon does not pass, so it always failed closed). Nothing was ever deleted wrongly; cleanup just could not use the PR information. The provider contract suite now calls the lookup exactly as the daemon does.

### Changed
- Clone paths may contain interior spaces (Azure DevOps project and repository names do); leading and trailing spaces, dots-only names, traversal and control characters are still refused.

### Added
- **Azure DevOps support** (`provider = "azuredevops"`, Services and Server): discovery across all projects of an organization, clone, fast-forward and safe cleanup via completed pull requests, with an organization-scoped PAT (`base_url` required). Project and repository names with spaces are supported (interior spaces are now allowed in clone paths). Tested against fixtures only so far; see docs/PROVIDERS.md.
- **Bitbucket Cloud support** (`provider = "bitbucket"`): discovery across all your workspaces, clone, fast-forward and safe cleanup via merged-PR lookup, using an Atlassian API token (Atlassian retired app passwords). Tested against fixtures only so far; see docs/PROVIDERS.md.

## 0.1.0-beta.3

### Fixed
- `start` and `restart` no longer report "did not become ready" / "exited right away" for a healthy daemon that has no web interface (`--no-ui` or `[ui] enabled = false`); readiness is now taken from the daemon's own "daemon started" log line, and a second `start` says "already running".
- `stop` (and `restart`) now work without a web interface: the daemon is found through its instance lock and asked to exit with SIGTERM.
- The Windows CI leg passes and gates merges again (test-only fixes).

### Added
- **Gitea and Forgejo support** (`provider = "gitea"` / `"forgejo"`, incl. Codeberg): discovery, clone, fast-forward and safe cleanup via merged-PR lookup. `base_url` is required. Tested against fixtures only so far; see docs/PROVIDERS.md.

## 0.1.0-beta.2

### Fixed
- **Every form in the web UI was refused with "cross-origin request refused"** (config save, Sync now, Restore, ...): browsers send `Origin: null` on form posts under a no-referrer policy, which the origin check rejected. The check now trusts the browser's `Sec-Fetch-Site` header and the policy is `same-origin`. (CSRF tokens were always required in addition.)
- A race when two processes opened a brand-new database at once (daemon + `status`) could fail with "table already exists" or `SQLITE_BUSY`.
- Flaky account-status tests: the explaining event is now written before the new status is published.

### Added
- **Run the daemon detached, no service manager needed:** `repo-keeper start`, `stop` and `restart` (restart re-executes in place, same PID, so it also works under systemd). Logs go to `<state dir>/daemon.log` (private, size-capped).
- **Daemon controls in the UI** (Debug page): restart and stop.
- **Everything is configurable from the web UI, with forms instead of a raw TOML editor:** Settings (clone folder, intervals, cleanup policy, UI), add/edit/remove accounts, add/remove single repositories. Pasted tokens are verified against the platform before anything is saved and go to the secret store, never the config file. Edits keep your comments and write a timestamped backup first. The config page is now a read-only view. Generated (Nix) or symlinked configs stay read-only by design.
- **GitHub device-code sign-in from the UI** ("Sign in with GitHub" on the account form): shows the code and link, waits for approval, then stores the token. Removing an account never deletes cloned repositories; it can also forget the stored token.
- Removed accounts are forgotten by the daemon (repos deactivated, saved state dropped) on reload.

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

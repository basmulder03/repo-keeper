# Roadmap

Ordering, not promises (one part-time maintainer). **Linux/NixOS first; GitHub first; UI from the first usable release.**

| Milestone | Theme | Exit criteria |
|---|---|---|
| **M0** Foundations | Repo, Nix devShell, CI (Linux; macOS/Windows build-only), conventions | CI green; lint/security gates; docs approved |
| **M1** ✅ done: Local engine | `gitx`, default-branch detection, FF, all-branch fetch, cleanup + trash + audit journal | CLI syncs/cleans a local repo safely; property + integration tests; 100 % predicate coverage |
| **M2** ✅ done: Rate limiting & scheduler | `httpx`, `ratelimit`, `sched`, daemon, single instance | 429/Retry-After honoured in simulation; kill -9 safe |
| **M3a** ✅ done: GitHub | Provider interface + GitHub (token, device flow, GHES), keychain/`token_file`/`token_env`, discovery with include/exclude, cloning, credentialed git via askpass, provider-assisted squash detection, `accounts` and `discover` CLI | Real account syncs within quota |
| **M3b** ✅ done: Web UI (first usable release) | Local HTTP API, `repo-keeper ui` + one-time login URL, dashboard, config editing, cleanup review, audit view, debug page | CSRF/Host tests; axe clean |
| **M3.5** ✅ done: Tray helper (Linux SNI, Windows) | `repo-keeper-tray`: status icon + menu | Works on KDE/GNOME-AppIndicator/waybar; daemon unaffected when absent |
| **M4** ✅ done: Provider extensibility proof | Second provider (GitLab, nested groups) added **without touching core** | Shared contract suite passes; layout supports N-level namespaces |
| **M5** ✅ done: Packaging (Linux) | Nix flake + Home-Manager module, systemd user unit, `.deb`/`.rpm`, container, GoReleaser, cosign, SBOM | Signed Linux releases; reproducible build check |
| **M6** Remaining providers | Azure DevOps (org/project), Bitbucket Cloud, Gitea/Forgejo, generic | Contract suite + compliance checklist per provider |
| **M7** ✅ done (beta soak pending): Hardening & 0.x beta | Fuzzing, perf (NFR-1), hostile-repo suite, self-audit | 4 weeks dogfooding without data-loss bugs |
| **1.0** | Stable config/CLI | Docs complete |
| **Later** | macOS (launchd, notarization), Windows (Task Scheduler, Authenticode/SignPath), Windows/macOS tray, Bitbucket DC | Tackled when a release for that OS is wanted |

## Todo (not started; nothing here is to be implemented yet)
- [ ] **Single source for the version.** `flake.nix` hardcodes the version (twice) and beta.3 shipped with it stale; the release pipeline should set it from the tag (and fail if a tracked copy disagrees), so a release can never be built or installed under the wrong version.
- [ ] Remove the hand-maintained version from `docs/BETA.md` titles for the same reason.
- [ ] **Static documentation site (GitHub Pages).** Always up to date with the code. Hand-written pages come from the Markdown in `docs/` (requirements, architecture, install, beta guide, threat model, providers); pipeline-generated pages come from the build: release notes (from `CHANGELOG.md` and GitHub releases), the config reference and JSON schema (`make gen`), CLI help for every command, the provider capability matrix, supported versions and download links with checksums, and project statistics (test count and coverage, binary size, dependency and SBOM summary, CI/benchmark trends). Built and deployed by CI on every release and on `main`; no manual publishing step.
- [ ] **Install one-liners for every environment**, all documented on the site and in `docs/INSTALL.md`: Nix (`nix run` / `nix profile add github:...`, Home Manager and NixOS module snippets), Debian/Ubuntu (`.deb` or an apt repository), Fedora/RHEL (`.rpm` or a dnf repository), generic Linux (tarball), a container image, and, once those builds are signed, macOS (Homebrew tap) and Windows (winget/Scoop). Any shell one-liner must fetch a pinned version, verify the cosign signature and checksum before running anything, and be copy-paste safe (no `curl | sh` without verification).
- [ ] **Version check and explicit self-update, per environment where it is possible** (policy settled in ADR-0021). `update --check` queries the official GitHub releases API (through `httpx` and the rate limiter); a background check is on by default (opt-out `[update] check = false`), daily at most with jitter, and only notifies in `status`, `doctor` and the UI, flagging security releases as urgent as a call to action. Where a package manager owns the binary (Nix, apt/dnf, Homebrew, winget, container) the command prints the exact update command. Only the plain tarball install gets `update --apply`: explicit user action with confirmation, never automated, cosign + checksum verified before an atomic swap, previous binary kept for `update --rollback`. Needs an architecture test that keeps the apply code out of the daemon, UI and tray.

## Post-1.0 ideas (not committed)
GitHub App auth UX, Prometheus metrics, post-sync hooks, worktree-aware tools, config export, out-of-process provider plugins.

## Explicit non-goals
Pushing/rebasing/merging user work, **deleting remote branches (never)**, acting as a Git GUI, hosting a server, cloud sync, telemetry, scraping.

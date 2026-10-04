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
| **M6** ✅ done: Remaining providers | Azure DevOps (org/project), Bitbucket Cloud, Gitea/Forgejo, generic | Contract suite + compliance checklist per provider |
| **M7** ✅ done (beta soak pending): Hardening & 0.x beta | Fuzzing, perf (NFR-1), hostile-repo suite, self-audit | 4 weeks dogfooding without data-loss bugs |
| **1.0** | Stable config/CLI | Docs complete |
| **Later** | macOS (launchd, notarization), Windows (Task Scheduler, Authenticode/SignPath), Windows/macOS tray, Bitbucket DC | Tackled when a release for that OS is wanted |

## Todo (not started; nothing here is to be implemented yet)
- [x] **Single source for the version** (done, see `docs/RELEASING.md`): `VERSION` is the one tracked copy, the flake reads it, `scripts/check-version.sh` runs in CI and as the first release-workflow step (tag, `VERSION`, changelog and flake must agree), and `release-prep` does the bump. Hand-written versions were removed from the README and `docs/BETA.md`.
- [x] **Static documentation site (GitHub Pages)** (built; goes live when Pages is enabled with source "GitHub Actions"): `tools/site` renders the Markdown in `docs/` and the root documents and generates the command-line reference and starter configuration from the real binary, plus statistics (size, tests, coverage per package, dependencies, releases) and release downloads from the GitHub releases. The `site` workflow builds and link-checks it on every relevant pull request and deploys from `main`, tags and published releases. Still to do on it: a search box, and per-release pages with the verification commands filled in.
- [~] **Install one-liners** (Linux done, the rest waits for signed builds): Nix (`nix run` / `nix profile add`, also pinned to a tag), a verified tarball installer (`scripts/install.sh`, published on the site with its SHA-256; cosign identity pinned to the exact tag, nothing installed on any failed check, covered by `scripts/test-install.sh` in CI), and ready-to-paste `.deb`/`.rpm` commands that verify before installing, all generated on the site pinned to the newest *published* release. Still to do: apt/dnf repositories, a container image, Homebrew and winget (macOS and Windows builds must be signed first).
- [x] **Version check and explicit self-update** (built as ADR-0021/0022; Linux, tarball installs): `repo-keeper update [--check | --apply | --rollback]`, the daily opt-out check surfaced in `status`, `doctor` and the web interface, evidence printout, cosign + SHA-256 verification, interactive confirmation, hard-linked previous binary, and an architecture test keeping the apply code out of the daemon, UI and tray. Review pending (see `docs/SECURITY-REVIEW.md`). Not yet: package-manager hooks (apt/dnf repositories, Homebrew, winget), native signature verification, a settings toggle in the web UI.

## Post-1.0 ideas (not committed)
GitHub App auth UX, Prometheus metrics, post-sync hooks, worktree-aware tools, config export, out-of-process provider plugins.

## Explicit non-goals
Pushing/rebasing/merging user work, **deleting remote branches (never)**, acting as a Git GUI, hosting a server, cloud sync, telemetry, scraping.

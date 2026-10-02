# Roadmap

Ordering, not promises (one part-time maintainer). **Linux/NixOS first; GitHub first; UI from the first usable release.**

| Milestone | Theme | Exit criteria |
|---|---|---|
| **M0** Foundations | Repo, Nix devShell, CI (Linux; macOS/Windows build-only), conventions | CI green; lint/security gates; docs approved |
| **M1** Local engine | `gitx`, default-branch detection, FF, all-branch fetch, cleanup + trash + audit journal | CLI syncs/cleans a local repo safely; property + integration tests; 100 % predicate coverage |
| **M2** Rate limiting & scheduler | `httpx`, `ratelimit`, `sched`, daemon, single instance | 429/Retry-After honoured in simulation; kill -9 safe |
| **M3** GitHub + UI (first usable) | GitHub discovery/auth (token, device flow BYO client id), merged-PR lookup, keychain, **web UI incl. debug page**, config editing, audit view | End-to-end sync of a real GitHub account within quota; CSRF/Host tests; axe clean; `repo-keeper ui` + runtime-file discovery |
| **M3.5** Tray helper (Linux SNI) | `repo-keeper-tray`: status icon + menu | Works on KDE/GNOME-AppIndicator/waybar; daemon unaffected when absent |
| **M4** Provider extensibility proof | Second provider (GitLab, nested groups) added **without touching core** | Shared contract suite passes; layout supports N-level namespaces |
| **M5** Packaging (Linux) | Nix flake + Home-Manager module, systemd user unit, `.deb`/`.rpm`, container, GoReleaser, cosign, SBOM | Signed Linux releases; reproducible build check |
| **M6** Remaining providers | Azure DevOps (org/project), Bitbucket Cloud, Gitea/Forgejo, generic | Contract suite + compliance checklist per provider |
| **M7** Hardening & 0.x beta | Fuzzing, perf (NFR-1), hostile-repo suite, self-audit | 4 weeks dogfooding without data-loss bugs |
| **1.0** | Stable config/CLI | Docs complete |
| **Later** | macOS (launchd, notarization), Windows (Task Scheduler, Authenticode/SignPath), Windows/macOS tray, Bitbucket DC | Tackled when a release for that OS is wanted |

## Post-1.0 ideas (not committed)
GitHub App auth UX, Prometheus metrics, post-sync hooks, worktree-aware tools, config export, out-of-process provider plugins.

## Explicit non-goals
Pushing/rebasing/merging user work, **deleting remote branches (never)**, acting as a Git GUI, hosting a server, cloud sync, telemetry, scraping.

# repo-keeper

A lightweight, local-first background agent that keeps every repository you can access on GitHub, GitLab, Bitbucket, Azure DevOps and Gitea/Forgejo cloned and up to date, safely.

> **Status:** feature-complete for GitHub and GitLab on Linux (Gitea/Forgejo, Bitbucket Cloud and Azure DevOps added, fixture-tested): sync engine, safe cleanup, rate limiting, daemon, web UI, tray, packaging, hardening and fuzzing are done and measured. More providers (M6) and a beta soak are next. See [`docs/`](docs/README.md), [installation](docs/INSTALL.md), [security review](docs/SECURITY-REVIEW.md) and the [roadmap](docs/ROADMAP.md).

## What it does

- **Discovers** all remotes you can access per configured account (via official APIs).
- **Syncs** them on a schedule: fetch, prune, fast-forward the default branch (`main`/`master`/anything, auto-detected).
- **Cleans up** local branches that are already merged (including squash/rebase merges), with safety rails and undo.
- **Respects** provider rate limits and terms of service (back-off, conditional requests, polite minimum intervals).
- **Shows state** in a small local web UI (and CLI): last sync, errors, rate-limit budget, pending cleanups, configuration.

## Principles

1. **Never lose work.** Fast-forward only, never touch dirty trees, never force, every deletion recoverable.
2. **Least privilege.** Read-only tokens by default; secrets live in the OS keychain only.
3. **Lightweight.** One static binary, no runtime, idle ≈ 0% CPU, <50 MB RSS.
4. **Polite.** Official APIs only, identifiable User-Agent, honors `Retry-After` and rate-limit headers.
5. **Boring to install.** Signed, notarized, reproducible releases; user-level autostart, no admin rights.

## Documentation

| Doc | Purpose |
|---|---|
| [Requirements](docs/REQUIREMENTS.md) | What it must do (FR/NFR/SEC, prioritised) |
| [Architecture](docs/ARCHITECTURE.md) | Components, data model, sync algorithm |
| [Providers & ToS](docs/PROVIDERS.md) | Per-platform auth, limits, compliance |
| [Security](SECURITY.md) · [Threat model](docs/THREAT-MODEL.md) | Policy and analysis |
| [Distribution & AV](docs/DISTRIBUTION.md) | Packaging, signing, avoiding false positives |
| [Testing](docs/TESTING.md) | Strategy and quality gates |
| [Implementation plan](docs/IMPLEMENTATION-PLAN.md) | Repo layout, work breakdown |
| [Roadmap](docs/ROADMAP.md) | Milestones |
| [Security review](docs/SECURITY-REVIEW.md) · [Performance](docs/PERFORMANCE.md) | Self-audit findings, measured budgets |
| [Decisions (ADRs)](docs/adr/) | Why we chose what we chose |
| [Beta guide](docs/BETA.md) · [Changelog](CHANGELOG.md) | How to test 0.1.0-beta.1 safely, what changed |
| [Decisions & open items](docs/OPEN-QUESTIONS.md) | Resolved choices, what remains |
| [AGENTS.md](AGENTS.md) | Rules for AI coding agents |

## License

[Apache-2.0](LICENSE). Contributions: see [CONTRIBUTING.md](CONTRIBUTING.md).

Installing as a service (NixOS, Home Manager, deb, rpm, tarball): see [docs/INSTALL.md](docs/INSTALL.md).

## Quick start (development builds)

```sh
nix develop            # or install Go ≥ 1.26 and git ≥ 2.34
make build
./bin/repo-keeper init                      # writes a starter config
# GitHub: a fine-grained read-only token is recommended
./bin/repo-keeper accounts add personal --token-stdin --include 'me/*,my-org/*' < token.txt
# GitLab (nested groups welcome): a group/project access token with read_api + read_repository
./bin/repo-keeper accounts add work --provider gitlab --base-url https://gitlab.example.com --token-stdin --include 'acme/**' < token.txt
$EDITOR ~/.config/repo-keeper/config.toml   # set [general] root = "/home/you/code"   # or open `repo-keeper ui` → Settings
./bin/repo-keeper discover                  # preview what would be cloned
./bin/repo-keeper daemon                    # sync in the background (see packaging/systemd)
./bin/repo-keeper status
./bin/repo-keeper ui                        # opens the web interface (one-time sign-in link; --print-url for SSH)
./bin/repo-keeper-tray &                    # optional status icon (KDE, waybar/sway, GNOME with AppIndicator)
```

No keychain (headless server, container)? Use `--token-file /run/secrets/gh-token` (mode 600) or `--token-env GH_TOKEN` instead of `--token-stdin`.

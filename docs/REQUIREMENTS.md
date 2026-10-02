# Requirements

Priority: **M** must (v1.0), **S** should, **C** could (post-1.0). Each requirement has an ID used in tests and commits.

## 1. Scope

**In:** discover, clone, fetch, fast-forward default branches, prune merged branches, scheduling, rate-limit handling, local UI/CLI, secure credential handling, installers.
**Out (v1):** pushing, rebasing/merging user branches, creating PRs, code hosting, multi-user/server mode, cloud sync of config, telemetry.

## 2. Functional requirements

### Accounts & auth
| ID | P | Requirement |
|---|---|---|
| FR-A1 | M | Multiple accounts per provider; each account = provider kind + base URL + credential ref. |
| FR-A2 | M | Credential types: PAT/API token, OAuth device flow (where supported), SSH key via ssh-agent. |
| FR-A3 | S | GitHub App installation tokens; Azure DevOps Entra ID (OAuth); GitLab OAuth refresh. |
| FR-A4 | M | Secrets live in the OS keychain, or are read from a mode-600 `token_file` / an environment variable (agenix, sops-nix, systemd credentials, containers). A passphrase-encrypted file store for headless hosts without any of these is planned for hardening (M7). |
| FR-A5 | M | Self-hosted instances (GitHub Enterprise Server, GitLab self-managed, Gitea/Forgejo, Bitbucket DC) via base URL + custom CA. |
| FR-A6 | M | Credential health check: detect expired/revoked/under-scoped token and surface in UI; stop retrying until fixed. |

### Discovery
| ID | P | Requirement |
|---|---|---|
| FR-D1 | M | List all repos an account can access (owned, org, collaborator) via official API with pagination. |
| FR-D2 | M | Include/exclude rules: owner/org globs, name globs, visibility, archived, forks, size cap, topics. |
| FR-D3 | M | Handle renamed/transferred/deleted/archived repos (update remote URL; mark missing, never auto-delete local clones). |
| FR-D4 | S | Re-discover on a slower cadence than sync (default 6 h) using ETag/conditional requests. |
| FR-D5 | C | Plain "generic git" provider: static list of URLs, no API. |

### Sync
| ID | P | Requirement |
|---|---|---|
| FR-S1 | M | Clone missing repos into a tree `<root>/<provider>/<namespace…>/<repo>`; namespace has **N levels** (GitHub owner; GitLab nested groups; Azure DevOps org/project; Bitbucket workspace/project). Template configurable. |
| FR-S2 | M | Periodic `fetch --prune` of all remotes, per-repo interval with jitter. |
| FR-S3 | M | Auto-detect default branch from remote (`ls-remote --symref` / API), re-check each cycle, handle rename (`master`→`main`). |
| FR-S4 | M | Update local default branch **fast-forward only**: if checked out and clean → `merge --ff-only`; if not checked out → `fetch origin main:main`; if diverged/dirty → skip and report. |
| FR-S5 | M | Skip, never fail destructively, on: dirty tree, in-progress rebase/merge, detached HEAD, locked index, unreachable remote. |
| FR-S6 | M | Fetch **all** remote branches by default; per-repo override to default-branch-only. |
| FR-S7 | S | Support shallow/partial clones (`--filter=blob:none`) and Git LFS (fetch pointers only unless enabled). |
| FR-S8 | S | Adopt existing clones (match by remote URL) without re-cloning. |
| FR-S9 | S | Submodules: fetch only, never update working tree. |
| FR-S10 | C | Post-sync hooks (user command), off by default. |

### Branch cleanup
| ID | P | Requirement |
|---|---|---|
| FR-C1 | M | Delete **local** branches only when fully merged into the default branch (`git branch --merged`). |
| FR-C2 | M | Detect squash/rebase merges: upstream gone **and** (provider reports PR merged **or** `git cherry` shows patch-equivalent); a provider "merged" signal counts only if the PR head SHA equals the local tip. |
| FR-C3 | M | Never delete: current branch, branches never pushed (unless `--allow-never-pushed`), default branch, branches checked out in any worktree, protected globs (`release/*`, `develop`…), branches with unpushed/unmerged commits, branches younger than N days (default 7). |
| FR-C4 | M | Cleanup mode is user-configurable globally and per repo: `off`, `dry-run`, `auto` (default `dry-run`). Safeguards (FR-C3) apply in every mode and cannot be disabled; a dirty working tree always blocks deletion in that repo. |
| FR-C5 | M | Recoverable: before delete, record SHA in `refs/repo-keeper/trash/<name>` and journal; `repo-keeper restore <branch>`; trash expires after 30 d. |
| FR-C6 | M | **Remote branches are never deleted.** repo-keeper only mutates local clones and requests no provider write scopes. |
| FR-C7 | S | Report of "stale but unmerged" branches (informational only). |
| FR-C8 | M | **Audit journal**: every deletion, restore, failed delete and blocked cleanup is appended (repo, branch, SHA, reason, mode, timestamp) before the action takes effect; viewable via `repo-keeper audit` (UI in M3). Per-branch skip reasons are in the run report, not journaled (they would repeat every cycle). |

### Scheduling & rate limiting
| ID | P | Requirement |
|---|---|---|
| FR-R1 | M | Per-host token bucket + concurrency cap (default 2 per host, 4 global). |
| FR-R2 | M | Honor `Retry-After`, `X-RateLimit-*`, `RateLimit-*`, GitHub secondary-limit responses; exponential backoff + full jitter; circuit breaker per host. |
| FR-R3 | M | Conditional requests (ETag / If-Modified-Since); skip fetch when `ls-remote` refs unchanged. |
| FR-R4 | M | Floor on intervals (default 15 min, hard minimum 5 min) so config cannot make the tool abusive. |
| FR-R5 | M | Budget awareness: stop discovery calls at a configurable % of remaining quota; surface remaining quota in UI. |
| FR-R6 | S | Quiet hours, pause on battery/metered network, manual "sync now" (still rate-limited). |
| FR-R7 | S | Wake/resume handling: run missed schedule once, not N times. |

### Interfaces
| ID | P | Requirement |
|---|---|---|
| FR-U1 | M | Local web UI **from the first release**: dashboard (repo table: status, last sync, ahead/behind, error), accounts, rules, schedule, cleanup review + audit journal, logs, rate-limit gauges, and a **debug page** (live event stream, per-repo recent git commands/exit codes with secrets redacted, force-run job, state inspector, config diff, diagnostics bundle export). Optional at runtime (`--no-ui`). |
| FR-U2 | M | UI can edit all config; writes validated, atomic, versioned (keep last 10). |
| FR-U3 | M | CLI: `init`, `daemon`, `ui`, `sync [repo]`, `status`, `doctor`, `accounts add/rm`, `cleanup --dry-run`, `restore`, `audit`, `config validate`, `install-service`, `version`. |
| FR-U4 | M | Config as a human-editable file (TOML) with JSON Schema; hot-reload. |
| FR-U5 | S | Desktop notifications on persistent failure/expired credentials (opt-in). |
| FR-U6 | S | UI accessible (WCAG 2.2 AA), keyboard navigable, light/dark, works without JS framework build step. |
| FR-U7 | S | Optional **tray helper** (`repo-keeper-tray`, separate process): status icon (ok / syncing / attention), menu: Open UI, Sync now, Pause/Resume, Open logs, Quit. Talks to the daemon only through the local API; daemon never depends on it. |
| FR-U8 | M | `repo-keeper ui` opens the UI of the running daemon in the default browser; `--print-url` for headless/SSH. It deliberately does **not** spawn the daemon (explicit service control, nothing hidden in the background; start it with systemd or `repo-keeper daemon`). UI port: prefer configured/default `7878`, else any free port; discovery via runtime file (FR-U9). |
| FR-U9 | M | Daemon writes `ui.json` (address, pid, started_at) to `$XDG_RUNTIME_DIR/repo-keeper/` (macOS/Windows: per-user runtime dir), mode `0600`; CLI and tray read it. `ui` mints a **one-time login URL** (short TTL, single use) that sets the session cookie, so no token is typed or stored in browser history. |

### Operation
| ID | P | Requirement |
|---|---|---|
| FR-O1 | M | User-level background service. **First target: Linux/NixOS** (systemd `--user`, Nix flake + Home-Manager module); launchd and Windows Scheduled Task later. No admin/root. Headless mode (daemon + CLI, container) supported in v1. |
| FR-O2 | M | Single instance per user (lockfile/socket); graceful shutdown; crash-safe state. |
| FR-O3 | M | Structured logs (JSON optional), rotation, redaction of secrets/tokens. |
| FR-O4 | M | `doctor`: checks git version, keychain access, connectivity, clock skew, disk space, permissions. |
| FR-O5 | S | Self-update **check** only (notify); updates via package manager. No silent self-replace. |

## 3. Non-functional requirements

| ID | Requirement |
|---|---|
| NFR-1 | Idle CPU < 0.5 %, RSS < 50 MB with 500 repos tracked; sync memory bounded. |
| NFR-2 | Single static binary per OS/arch (linux, macOS, windows × amd64/arm64); no CGO. |
| NFR-3 | Startup < 300 ms; UI first paint < 500 ms on localhost. |
| NFR-4 | Crash safety: kill -9 at any point leaves repos consistent and state recoverable. |
| NFR-5 | Idempotent operations; re-running a cycle is always safe. |
| NFR-6 | Works offline (queues, no error storms); resumes cleanly. |
| NFR-7 | ≥ 85 % line coverage on `internal/`, 100 % on cleanup-safety predicate; race detector clean. |
| NFR-8 | Docs for every config key (generated from schema); every public func has a one-line doc comment. |
| NFR-9 | Reproducible builds; SBOM + provenance attached to each release. |
| NFR-10 | i18n-ready strings in UI (English at v1). |

## 4. Security requirements (summary; see [THREAT-MODEL](THREAT-MODEL.md))

| ID | Requirement |
|---|---|
| SEC-1 | UI binds `127.0.0.1` (never `0.0.0.0`) on a free port; optional unix socket; one-time login URL → session cookie; Host/Origin validation; CSRF protection; strict CSP; no CORS. |
| SEC-2 | Tokens never in config files, argv, URLs, logs or the DB. They are stored in the OS keychain (or read from `token_file`/`token_env`) and reach git only through the environment of that single git child, via the `GIT_ASKPASS` helper, which answers prompts for exactly one host (ADR-0015). |
| SEC-3 | TLS verification always on; custom CA bundle allowed; no `insecure-skip-verify` option. |
| SEC-4 | Least-privilege scopes documented per provider; read-only default; warn if token is over-scoped. |
| SEC-5 | Repo content treated as untrusted: disable hooks (`core.hooksPath=/dev/null`), `protocol.file.allow=never`, `safe.directory` honoured, no fsmonitor/`core.sshCommand` from repo config, no submodule recursion exec. |
| SEC-6 | Path traversal safe: sanitize owner/repo names, refuse symlink escapes from clone root. |
| SEC-7 | Dependencies pinned, `govulncheck`, SCA, CodeQL, secret scanning in CI. |
| SEC-8 | Signed releases (cosign + platform signing), provenance (SLSA ≥ L3 target). |
| SEC-9 | Coordinated vulnerability disclosure via [SECURITY.md](../SECURITY.md). |
| SEC-10 | No telemetry; the only outbound traffic is to configured providers (+ optional release check). |

## 5. Compliance requirements

| ID | Requirement |
|---|---|
| CMP-1 | Only documented public APIs and standard git protocols; no scraping, no private endpoints. |
| CMP-2 | Identifiable `User-Agent: repo-keeper/<ver> (+repo-url)`. |
| CMP-3 | Each provider's ToS/acceptable-use/API-limits reviewed, recorded with *last-verified* date in [PROVIDERS](PROVIDERS.md); re-review each release. |
| CMP-4 | OAuth: **bring-your-own client ID** is the default (device flow needs no client secret). A project-registered app may be added later; its client id is public and no secret is ever shipped in the binary. |
| CMP-5 | Never circumvent limits (no token rotation/multi-account pooling to exceed quota). |
| CMP-6 | Dependency licenses compatible with Apache-2.0 (CI license check); REUSE/SPDX headers. |
| CMP-7 | GDPR-friendly by design: all data local; documented data inventory. |

## 6. Acceptance scenarios (excerpt)

1. *Given* a clean clone on `main` that is behind, *when* a cycle runs, *then* `main` fast-forwards and the UI shows "up to date".
2. *Given* a repo whose default branch was renamed `master`→`main` upstream, *then* the next cycle detects it, updates `origin/HEAD`, warns, and does not delete local `master`.
3. *Given* a dirty working tree on `main`, *then* sync fetches but does not merge; status = "skipped: dirty".
4. *Given* a branch squash-merged via PR and deleted upstream, *then* it is listed in cleanup review (or deleted if auto-delete is on) with a trash ref.
5. *Given* HTTP 429 with `Retry-After: 120`, *then* no request is sent to that host for ≥120 s and the UI shows the cooldown.
6. *Given* a revoked token, *then* the account is marked "needs attention", retries stop, notification fires once.

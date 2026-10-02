# Architecture

## 1. Stack (see ADRs)

| Concern | Choice | Why |
|---|---|---|
| Language | Go ≥ 1.24 | Static binary, tiny footprint, great stdlib (net/http, slog), easy cross-compile (ADR-0001) |
| Git engine | System `git` CLI (≥ 2.34) via hardened wrapper | Full fidelity, LFS/partial clone/credentials; go-git lacks features (ADR-0002) |
| State | SQLite via pure-Go driver (`modernc.org/sqlite`), WAL | Queryable, crash-safe, no CGO (ADR-0003) |
| Config | TOML + JSON Schema | Human-editable, comments (ADR-0004) |
| UI | Server-rendered Go templates + htmx + a little vanilla JS, embedded via `embed` | No Node build, tiny, auditable (ADR-0005) |
| Secrets | `zalando/go-keyring` (+ encrypted file fallback) | OS-native stores (ADR-0006) |
| Releases | GoReleaser + cosign + SBOM | (ADR-0007) |

## 2. Component view

```
            ┌──────────────── repo-keeper (single process) ────────────────┐
 CLI ──────▶│  cmd/repo-keeper  ─▶  app (wiring, lifecycle, single-instance)│
 Browser ──▶│  ui/http  (127.0.0.1, token, CSRF) ─▶ api service layer    │
            │                                                            │
            │  scheduler ─▶ job queue ─▶ worker pool                     │
            │                 │             │                            │
            │         ratelimit (per host)  │                            │
            │                 ▼             ▼                            │
            │   provider/{github,gitlab,bitbucket,azdo,gitea}   gitx      │
            │        (REST, ETag, paging)        (git CLI wrapper)       │
            │                 │                    │                     │
            │            secrets (keyring)     fs (clone root)           │
            │                 └──────── store (SQLite) ─────────         │
            └────────────────────────────────────────────────────────────┘
```

### Packages (`internal/`)
| Package | Responsibility |
|---|---|
| `config` | Load/validate/hot-reload TOML, schema, defaults, atomic save + history |
| `secrets` | Keyring abstraction, redaction types (`secrets.Token` never prints) |
| `provider` | `Provider` interface + registry; one subpackage per platform |
| `httpx` | HTTP client: UA, timeouts, retries, ETag cache, rate-limit header parsing |
| `ratelimit` | Per-host token bucket, cooldowns, circuit breaker, budget tracking |
| `gitx` | Run git safely: env sanitising, hooks off, askpass, timeouts, output parsing |
| `syncer` | Per-repo flow: lock → safety checks → ls-remote → fetch → fast-forward → cleanup (named `syncer` to avoid clashing with stdlib `sync`) |
| `audit` | Append-only journal (JSONL, fsynced) of deletions/restores/blocks; moves into SQLite `store` in M2 |
| `paths` | Per-user state locations |
| `gitxtest` | Test fixtures: isolated bare origin + clones |
| `cleanup` | Pure safety predicate + executor + trash/restore |
| `sched` | Cron-like scheduler, jitter, quiet hours, wake handling |
| `store` | Migrations, repositories (accounts, repos, runs, events, trash) |
| `ui` | HTTP handlers, templates, static assets, session/CSRF |
| `tray` (cmd) | Optional tray helper; client of the local API only |
| `svc` | OS autostart integration (systemd/launchd/Task Scheduler) |
| `obs` | slog setup, redaction, in-memory ring buffer for UI log view |

Dependency rule: `syncer` and `cleanup` depend on **interfaces** (`Provider`, `Git`, `Clock`, `Store`) so all logic is unit-testable with fakes. `ui` depends only on `svc`-level service interfaces, never on `gitx` directly.

## 3. Provider interface

```go
type Provider interface {
    Kind() Kind
    // ListRepos pages through accessible repos; honours ETag via opts.
    ListRepos(ctx context.Context, opts ListOpts) iter.Seq2[Repo, error]
    // DefaultBranch returns the authoritative default branch.
    DefaultBranch(ctx context.Context, r RepoRef) (string, error)
    // MergedRefs reports branches whose PR/MR is merged (squash/rebase aware).
    MergedRefs(ctx context.Context, r RepoRef, branches []string) (map[string]MergeInfo, error)
    // Limits exposes the last-seen quota for UI and budgeting.
    Limits() Quota
    // CheckAuth validates the credential and its scopes.
    CheckAuth(ctx context.Context) (AuthStatus, error)
}
```
Capabilities are declared (`Caps()`), so features degrade gracefully (e.g. generic git has no `MergedRefs`; cleanup falls back to `git cherry`).

## 4. Data model (SQLite)

- `accounts(id, provider, base_url, display_name, cred_ref, status, last_check_at)`
- `repos(id, account_id, remote_id, full_name, clone_url, local_path, default_branch, archived, missing, rules_hash, next_sync_at, last_status, last_sync_at)`
- `runs(id, repo_id, started_at, finished_at, outcome, detail_json)`
- `events(id, ts, level, repo_id, code, message)` (ring-capped)
- `trash(id, repo_id, branch, sha, deleted_at, reason, expires_at)` and `audit(id, ts, repo_id, branch, sha, action, mode, reason)` (append-only journal of deleted/skipped branches)
- `ratelimits(host, remaining, reset_at, cooldown_until)`
- `meta(schema_version, ...)`

No secrets in the DB. Migrations are forward-only, embedded, tested.

## 5. Sync algorithm (per repo)

```
acquire repo lock (file lock in .git/repo-keeper.lock) ── held? skip
if not cloned: clone (partial/shallow per rules) ─▶ done
preflight: refuse if in-progress op (rebase/merge/cherry-pick/bisect), index.lock present
detect default branch D:
    remote HEAD via `ls-remote --symref origin HEAD` (cheap, no API)
    fallback provider.DefaultBranch(); fallback origin/HEAD; fallback main|master
    if changed vs stored → update refs/remotes/origin/HEAD, emit event
ls-remote refs unchanged since last run? ─▶ skip fetch (FR-R3)
git fetch --prune --no-tags(configurable) origin
fast-forward D:
    if D checked out here:
        clean?  ─▶ merge --ff-only origin/D     else skip("dirty")
    elif D exists locally and is ancestor of origin/D:
        update-ref D (via `fetch origin D:D`, refuses non-FF)
    else skip("diverged") + warn
cleanup (if enabled): candidates = locals − {current, D, worktrees, protected, young}
    for b in candidates: safe(b) ⇒ trash ref + delete (or list in review)
record run, schedule next (interval ± jitter), release lock
```

### Cleanup safety predicate (pure function; 100 % tested, property-tested)
`delete(b) = !protected ∧ !default ∧ !current ∧ checkedOut=No ∧ tipAge ≥ minAge ∧ (hasUpstream ∨ allowNeverPushed) ∧ (merged(b, D) ∨ (upstreamGone ∧ (providerMergedTip ∨ patchEquivalent)))`

- Every input is tri-state (`Yes/No/Unknown`); `Unknown` always means skip (fail closed). Zero-valued facts never delete.
- `hasUpstream` guards fresh/empty branches: a branch created from `main` looks "merged" but was never pushed, so it needs `--allow-never-pushed`.
- `providerMergedTip` means the provider reports a merged PR **whose head SHA equals the local tip**, so late local commits are never lost to a squash merge.
- `patchEquivalent` (`git cherry`) only recognises single-commit squashes; multi-commit squashes need provider data (M3).
- Before deleting, the executor re-reads the tip, re-checks `UniqueCommits == 0` for merged branches, writes the trash ref, **journals first (write-ahead; journal failure aborts)**, then deletes with `update-ref -d <ref> <expected-sha>`.
- Dirty working tree (including untracked files), in-progress operations, or a held lock block all deletions in that repo; this is journaled as `blocked`.
- The predicate is verified by an exhaustive enumeration of all fact combinations (not sampling) in `internal/cleanup/decide_test.go`.

## 6. Scheduling & rate limiting

- Scheduler ticks every 30 s, enqueues due repos (`next_sync_at ≤ now`), ordered by staleness; jitter ±20 %.
- Worker pool: `min(4, GOMAXPROCS)`; per-host semaphore (default 2).
- Every outbound call (API **and** git network op) passes `ratelimit.Wait(host)`.
- Response handling: parse `Retry-After`, `X-RateLimit-Remaining/Reset`, `RateLimit-*` (IETF draft), GitHub secondary limits (403/429 + message) → set host cooldown; exponential backoff (base 30 s, cap 1 h, full jitter); circuit opens after 5 consecutive failures, half-open probe.
- Discovery uses ETags; unchanged = 304 (does not count against quota on GitHub).
- Clock: monotonic time for cooldowns; wall-clock jumps (sleep/resume) handled once.

## 7. Web UI

Pages: **Dashboard** (repo table, filters, bulk "sync now"), **Repo detail** (history, branches, cleanup candidates with reasons), **Accounts** (add via device flow/token, health, scopes), **Rules & schedule**, **Cleanup review** (approve/undo, trash), **Rate limits**, **Logs**, **Settings**.

Startup: the daemon binds `127.0.0.1:7878` (configurable); if taken it binds port `0` (OS-assigned free port) and records the real address in `$XDG_RUNTIME_DIR/repo-keeper/ui.json` (`0600`). `repo-keeper ui` reads it, requests a **one-time login URL** (TTL 60 s, single use) from the daemon over the local API and opens it in the browser; the URL exchanges for an HttpOnly, SameSite=Strict session cookie. Every mutating request needs a CSRF token and `Origin`/`Host` match (blocks DNS rebinding); CSP `default-src 'self'`; no inline scripts; no external assets/CDNs. Headless: `repo-keeper ui --print-url` (use with SSH port-forward).

**Tray (optional):** separate binary/subcommand `repo-keeper-tray`, a thin client of the same local API (status poll/SSE). On Linux it speaks StatusNotifierItem over D-Bus (pure Go, no CGO; works on KDE, GNOME with the AppIndicator extension, waybar/sway trays); on Windows it uses the notification area via Win32 syscalls. macOS later. Keeping it out of the daemon keeps the daemon headless-friendly, small and free of GUI dependencies; if the tray crashes or is absent nothing else changes.

## 8. Config sketch

```toml
[general]
root = "~/code"
layout = "{provider}/{namespace}/{repo}"   # namespace = 1..N levels (group/subgroup, org/project)
interval = "30m"            # min 5m enforced
quiet_hours = "23:00-07:00"

[cleanup]
mode = "dry-run"            # off | dry-run | auto (per repo overridable; dirty tree always blocks)
min_age = "7d"
protected = ["develop", "release/*"]

[[account]]
name = "work-gh"
provider = "github"
base_url = "https://api.github.com"
auth = "oauth-device"       # token | oauth-device | app | ssh
include = ["my-org/*"]
exclude = ["*/archive-*"]
```

## 9. Error handling & observability

Typed errors (`ErrDirty`, `ErrDiverged`, `ErrRateLimited{RetryAfter}`, `ErrAuth`) map to run outcomes and UI badges. Logs via `slog` with redaction handler; `/healthz` (local) and `repo-keeper status --json` for scripting. Optional Prometheus `/metrics` on the local socket (C).

## 10. Cross-platform notes

Windows: long paths, case-insensitive collisions (detect, warn), `git.exe` discovery, Credential Manager. macOS: Keychain, LaunchAgent, notarization. Linux: Secret Service (fallback file for headless), systemd user unit; NixOS/Nix flake provided.

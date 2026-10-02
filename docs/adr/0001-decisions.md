# ADRs (initial batch, all *Proposed* until the maintainer confirms; see OPEN-QUESTIONS)

Format: Context → Decision → Consequences. Supersede by adding a new ADR, never by editing history.

## ADR-0001 Language: Go
**Context:** need tiny, static, cross-platform, background-friendly binary, easy concurrency and HTTP.
**Decision:** Go ≥ 1.26, `CGO_ENABLED=0`.
**Consequences:** + single binary, fast builds, mature tooling. − Go binaries get more AV scrutiny (mitigated by signing/no packing); GC overhead small.

## ADR-0002 Git engine: system git CLI
**Context:** correctness with LFS, partial clone, credentials, worktrees matters more than avoiding a dependency.
**Decision:** shell out to `git` (≥ 2.34) via `gitx`, hardened (hooks off, scrubbed env, no shell).
**Consequences:** + fidelity, + less code. − requires git installed (checked by `doctor`), output parsing (use porcelain/`-z`). Revisit go-git for read-only ops.

## ADR-0003 State store: SQLite (pure Go)
**Decision:** `modernc.org/sqlite`, WAL, embedded migrations. **Consequences:** + queries/atomicity, no CGO; − larger binary than bbolt (~+5 MB).

## ADR-0004 Config: TOML + JSON Schema
**Decision:** TOML file is source of truth; UI edits via validated atomic writes with history. **Consequences:** + comments, diffable; − UI must preserve comments (use a round-trip-capable library or write only managed sections).

## ADR-0005 UI: embedded server-rendered web (htmx)
**Decision:** Go templates + htmx + minimal JS, served on loopback. **Consequences:** + no Node toolchain, tiny, easy CSP; − less rich than SPA (acceptable).

## ADR-0006 Secrets: OS keychain, no secrets in config
**Decision:** `go-keyring`; encrypted file fallback (age/argon2id) for headless. **Consequences:** + best practice; − headless UX needs passphrase or env-injected secret file.

## ADR-0007 Release: GoReleaser + cosign + SBOM + SLSA
**Decision:** see DISTRIBUTION. **Consequences:** + verifiable supply chain; − signing certs need setup (SignPath/Apple).

## ADR-0008 Cleanup is fail-closed, dry-run first, recoverable
**Decision:** see ARCHITECTURE §5. **Consequences:** + trust; − some stale branches linger until user opts in.

## ADR-0009 License: Apache-2.0
**Context:** open source, permissive, contributor/patent protection, enterprise-friendly. **Decision:** Apache-2.0 + DCO. **Consequences:** + patent grant; − NOTICE file upkeep. Alternative: MIT (simpler, no patent grant).

## ADR-0010 Local-only mutation, auditable cleanup
**Decision:** repo-keeper never deletes or writes remote branches (no write scopes); every local branch removal/skip is appended to an `audit` journal in addition to the trash ref. **Consequences:** + smaller blast radius, + simpler provider ToS posture; − remote stale branches are only reported.

## ADR-0011 Platform order: Linux/NixOS → macOS → Windows; GitHub → others
**Decision:** v0 targets Linux/NixOS (Nix flake, Home-Manager module, systemd user unit); signing for Windows/macOS deferred until those releases. UI ships in the first usable release. **Consequences:** + fast feedback, − AV/signing work postponed (still designed for, see DISTRIBUTION).

## ADR-0012 OAuth: bring-your-own client ID by default
**Decision:** device flow with user-supplied client id; no secret embedded. **Consequences:** + nothing to leak/rotate, + ToS-simple; − slightly more setup (documented, with a one-command helper).

## ADR-0010 Local-only mutation, auditable cleanup
**Decision:** repo-keeper never deletes or writes remote branches (no write scopes); every local branch removal/skip is appended to an `audit` journal in addition to the trash ref. **Consequences:** + smaller blast radius and simpler provider ToS posture; − stale remote branches are only reported.

## ADR-0011 Platform order: Linux/NixOS → macOS → Windows; GitHub → others
**Decision:** v0 targets Linux/NixOS (Nix flake, Home-Manager module, systemd user unit); Windows/macOS signing deferred until those releases. UI ships in the first usable release. **Consequences:** + fast feedback; − AV/signing work postponed (still designed for, see DISTRIBUTION).

## ADR-0012 OAuth: bring-your-own client ID by default
**Decision:** device flow with a user-supplied client id; no secret embedded. **Consequences:** + nothing to leak or rotate; − slightly more setup (documented).

## ADR-0013 UI access: `repo-keeper ui` + free port + optional separate tray
**Context:** the UI should be one command away and also reachable from a tray icon, without making the daemon heavy or GUI-dependent.
**Decision:** daemon owns the web server (default port 7878, falls back to an OS-assigned free port) and publishes its address in a `0600` runtime file; `repo-keeper ui` opens a one-time login URL; a separate optional `repo-keeper-tray` helper talks to the same local API. Minimum git version **2.34** (Ubuntu 22.04 floor; has `GIT_CONFIG_COUNT`, `safe.directory`, mature worktree/partial-clone support), `doctor` recommends ≥ 2.39.
**Consequences:** + headless-friendly, tray crash-isolated, no token pasting; − two processes to package, tray depends on desktop support (SNI on Linux).

## ADR-0014 Dependencies and audit storage in M2
**Decision:** two third-party modules: `modernc.org/sqlite` (pure Go, BSD-3, needed for ADR-0003) and `github.com/pelletier/go-toml/v2` (MIT, strict decoding with unknown-key rejection). Go floor raised to 1.26 by the SQLite module. The audit journal stays an append-only fsynced JSONL file instead of moving into SQLite: it is trivially greppable, survives database corruption/migration, and a single `write+fsync` before each deletion is the strongest simple guarantee. **Consequences:** + auditability independent of the DB; − two storage formats (UI reads the journal through `audit.File`). `vendorHash` in `flake.nix` must be refreshed when `go.mod` changes.

## ADR-0015 How git receives credentials
**Context:** private clones/fetches over HTTPS need a token. Options: URL userinfo or `http.extraHeader` in argv (visible in `ps`, `/proc/*/cmdline`, error messages), config files (persisted), or the environment of the single git child.
**Decision:** `GIT_ASKPASS` points at the repo-keeper binary, which switches into helper mode when `REPO_KEEPER_ASKPASS=1`. The daemon puts the secret in the environment of that one git invocation only. The helper parses git's prompt and answers **only if the prompt's host equals the one the credential was issued for**, so redirects, submodules and `insteadOf` rewrites cannot receive it. The runner's environment is allow-listed, so the secret is never inherited by other children. Git errors are scrubbed of token shapes and the daemon's logger additionally redacts every token it has loaded.
**Consequences:** + nothing persists on disk or in `ps`; + host-bound; − the secret is readable by the same user (and root) through `/proc/<git-pid>/environ` during the call, the same trust level as the daemon's own memory and the keychain session. SSH remotes use the user's ssh-agent instead. Verified by an end-to-end test against `git http-backend` (right secret works, no/wrong secret and wrong host fail, nothing lands in `.git/config`).

## ADR-0016 Web UI shape
**Decision:** server-rendered Go templates with a ~40-line vanilla `app.js` (filter, 10 s fragment refresh, confirm dialogs) instead of htmx or an SPA, so nothing third-party ships to the browser and the CSP can be `script-src 'self'` with no inline code. Every page works without JavaScript; mutations are plain POST forms with post-redirect-get. Configuration is edited as the raw TOML file (validated with the same parser the daemon uses, previous version archived, last 10 kept, optimistic-concurrency version token, applied via the daemon's reload) rather than through per-field forms, which would have to round-trip comments. Sign-in uses a one-time 60 s code minted through a control endpoint guarded by a token in the 0600 runtime file; sessions live 12 h in memory. "Delete safe branches now" re-runs the normal safety predicate rather than deleting a client-supplied branch list, so the UI cannot be used to delete something the engine would refuse. **Consequences:** + tiny attack surface and no front-end toolchain; − raw TOML editing is less friendly than forms (structured editors can be layered on later), and the cookie cannot carry `Secure` because the origin is `http://127.0.0.1`.

## ADR-0017 Tray helper and the machine API
**Decision:** `repo-keeper-tray` (fyne.io/systray, pure Go on Linux and Windows; macOS needs cgo and is excluded by build tags) is a separate binary that polls `GET /api/status` every 5 s and calls `POST /api/sync-all|pause|resume|login-url`. These endpoints are authenticated **only** by the control token from the 0600 runtime file; a browser session cookie does not unlock them, and the Host/Origin guards still apply. Pause stops scheduled work only (manual and "sync all" still run), the same semantics as quiet hours, and is in-memory (a daemon restart resumes). Attention outranks pause in the icon so a problem is never hidden. The tray re-reads the runtime file on every call, so a restarted daemon is picked up, and an instance lock prevents duplicate icons. **Consequences:** + the daemon stays headless and GUI-free; + tray crash or absence changes nothing; − GNOME needs the AppIndicator extension to show it; − no desktop notifications yet.

## ADR-0018 What adding GitLab taught us about the provider seam
**Context:** M4 was the extensibility test: add a second platform without reworking the core.
**Result:** the new code is one package (`provider/gitlab`) plus its tests. The core needed four small seams that GitHub had hidden: (1) a `GitUsername()` method (GitHub's `x-access-token` was hard-coded in the daemon), (2) provider names validated through the registry instead of a literal `"github"` check in `config`, (3) one registration point (`provider/all`) instead of a GitHub import inside the daemon, and (4) the CLI refusing `--device` for platforms without a device flow. Glob matching also had to learn `**` because nested GitLab groups made `*`-within-a-level surprising. Adding the next REST platform should now need its package and one line in `provider/all`; a platform with a different auth scheme (Azure DevOps) will probably add one more seam.
**Decision:** every provider runs the same contract suite (`providertest`), written once and fed by a per-provider fake server serving an equivalent fixture; GitHub was retrofitted onto it. GitLab device flow is deferred (expiring tokens need refresh-token storage).

## ADR-0019 Packaging: Nix modules as the reference, same sandbox everywhere, Linux only
**Decision:** (1) The Home Manager module is the full declarative path (generated read-only config, user service, optional tray, `environmentFile` for `token_env`, an assertion against secrets in the store); the NixOS module installs the package and units and leaves per-user config alone. Because a Nix-managed config is a symlink, the daemon reports it as read-only and the UI refuses to edit it instead of replacing the symlink and fighting the tool that owns it. (2) One hardening set (`nix/common.nix`, mirrored in the package units) is applied to daemon and tray; it was bisected on a real session so every option is known to be compatible. The packaged unit can only guess clone folders (`~/code ~/src ~/projects ~/git`, optional), so users with another root add a one-line drop-in; the Nix modules derive it from `general.root`. (3) Releases are reproducible by construction and checked in CI, signed keylessly, and published as drafts. (4) No Windows/macOS artifacts until signing exists.
**Consequences:** + a user can `nix run` or enable a hardened service declaratively; + supply-chain claims are testable (`make repro`); − the tarball's user/group names make it builder-specific; − GoReleaser's template fields for mtimes are not evaluated in this version, hence the `SOURCE_DATE_EPOCH` hook; − the release workflow is unproven until the first tag.

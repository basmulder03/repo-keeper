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

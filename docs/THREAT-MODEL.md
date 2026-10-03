# Threat model (STRIDE-lite)

**Assets:** provider credentials; integrity of the user's local repos (unpushed work!); local filesystem; the user's provider accounts and quotas; release artifacts.
**Trust boundaries:** (1) user ↔ local UI/CLI; (2) process ↔ provider APIs (network); (3) process ↔ git subprocess ↔ **untrusted repo content**; (4) project ↔ users (supply chain).
**Attackers:** malicious website in the user's browser; other local user/process; malicious repo/remote; network attacker; compromised dependency/CI.

| # | Threat | Boundary | Mitigation | Req |
|---|---|---|---|---|
| T1 | Malicious webpage calls the local UI API (CSRF, DNS rebinding) | 1 | Loopback/unix socket, random session token in HttpOnly SameSite=Strict cookie, CSRF token, `Host`+`Origin` allow-list, no CORS, JSON content-type enforced | SEC-1 |
| T2 | Other local user reads/uses UI port or config | 1 | Runtime file `0600`; one-time login URL; token/cookie auth on TCP; optional unix socket `0600`; config `0600`; per-user dirs | SEC-1 |
| T3 | Token leakage via logs, argv, URLs, shared env, crash dumps, config | 2,3 | Keychain only; `secrets.Token` type with redacting `String()/MarshalJSON`; per-invocation askpass helper (secret only in that git child's env, helper refuses other hosts); log redaction tests with canary tokens; core dumps disabled | SEC-2 |
| T4 | Token over-scoped → blast radius | 2 | Read-only defaults, scope check on `CheckAuth`, UI warning | SEC-4 |
| T5 | MITM on API/git | 2 | TLS verify always on; no skip-verify; optional custom CA only | SEC-3 |
| T6 | Malicious repo executes code via git (hooks, `core.fsmonitor`, `core.sshCommand`, `diff.external`, filters, submodule `update=!cmd`) | 3 | Run git with `-c core.hooksPath=<empty> -c core.fsmonitor=false -c protocol.ext.allow=never -c protocol.file.allow=never`, `GIT_CONFIG_NOSYSTEM`-style isolation for our invocations, never run checkout/merge with filters from untrusted config where avoidable, no recursive submodule update, no `post-*` execution | SEC-5 |
| T7 | Path traversal / symlink escape via repo/owner names | 3 | Validate names `[A-Za-z0-9._-]`, reject `..`, resolve and check clone path stays under root, Windows reserved names handled | SEC-6 |
| T8 | Data loss: cleanup deletes unmerged work; FF overwrites changes | 3 | Fail-closed predicate, FF-only, dirty-tree skip, dry-run default, trash refs + restore, repo lock, property tests | FR-C* |
| T9 | Abuse of provider (accidental DoS / ban) | 2 | Rate limiter, floors, jitter, circuit breaker, serial per host | FR-R*, CMP-* |
| T10 | Compromised dependency / build | 4 | Pinned + verified modules (`go.sum`), minimal deps, Dependabot/Renovate, `govulncheck`, CodeQL, SLSA provenance, cosign, 2FA + signed commits, branch protection, reviewed CI actions pinned by SHA | SEC-7/8 |
| T11 | Malicious update / fake installer | 4 | No automated update path: apply exists only as the explicit `update --apply` command, only for a tarball install, verifying cosign signature (pinned release-workflow identity) and checksum before the atomic swap, with rollback; checks only notify and are opt-in; package-manager distribution otherwise; documented verify steps; an architecture test keeps the apply code unreachable from the daemon/UI/tray | SEC-8, ADR-0021 |
| T12 | SQLite/config tampering by local malware | 1 | Treated as same-privilege attacker (out of scope), but config is validated strictly and never executes strings (no shell interpolation) | – |
| T13 | Hooks/post-sync command abuse | 1 | Feature off by default, config-file-only (not UI-editable), documented risk | FR-S10 |
| T14 | Resource exhaustion by huge repos | 3 | Size caps, timeouts, bounded workers, disk-space precheck | NFR-1 |
| T15 | Supply of false-positive AV flags causing user distrust | 4 | See [DISTRIBUTION](DISTRIBUTION.md) | – |

## Out of scope
Fully compromised user account/OS; physical access; provider-side vulnerabilities.

## Secure development practices
- Threat model revisited every minor release and on any new provider/auth type.
- Mandatory review for changes in `gitx`, `secrets`, `ui` (CODEOWNERS).
- Fuzzing: config parser, ref-name handling, header parsers, path sanitiser.
- Security tests in CI: canary-token log scan; hostile-repo fixture (malicious hooks/config/symlinks) must not execute anything.
- Release checklist includes SBOM diff and dependency review.

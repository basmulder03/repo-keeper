# AGENTS.md: instructions for AI coding agents

Project: a lightweight local daemon + small web UI that syncs git repos from many SCM providers. Read `docs/REQUIREMENTS.md`, `docs/ARCHITECTURE.md`, `docs/THREAT-MODEL.md` before changing code.

## Commands
- `make build` · `make test` (race + cover) · `make lint` · `make security` · `make gen` (schema + docs)
- A change is not done until `make test lint security` passes.

## Hard rules (safety)
1. **Never lose user work.** Do not weaken `internal/cleanup` safety predicate, FF-only logic, or dirty-tree checks. Unknown ⇒ fail closed. Changes there need property tests.
2. **No secrets** in code, logs, argv, env, URLs, config, fixtures, or commits. Use `secrets.Token`. Tests use canary values.
3. **Git runs only via `internal/gitx`** (hooks off, env scrubbed). Never `sh -c`, never string-concatenated commands.
4. **All outbound calls go through `httpx` + `ratelimit`.** No direct `http.Get`, no new endpoints outside official provider docs.
5. **UI is loopback-only**; never add CORS, external assets, inline scripts, or disable CSRF/Host checks.
6. Tests never touch the real `$HOME`, keychain, network, or real provider; use `t.TempDir()`, fakes, `httptest`.
7. Do not add dependencies without justification (license, maintenance, size); prefer stdlib.
8. Never use UPX/obfuscation. No self-update or download-and-execute behaviour, with one narrow exception (ADR-0021): `repo-keeper update --apply`, run by the user, on a plain tarball install only, after signature and checksum verification. No daemon, scheduler, UI, tray, service, timer, config option or environment variable may ever apply an update; update *checks* run by default (opt-out) and only notify.

## Style
- Go idioms; small interfaces at the consumer; `context.Context` first; wrap errors with `%w`; `slog` only; inject `Clock`.
- **Comments:** concise, one line, *why* not *what*; every exported symbol has a one-line doc comment. No decorative or multi-paragraph comments.
- Table-driven tests named `Test<Unit>_<Scenario>_<Expected>`; no `time.Sleep`.
- SPDX header on every source file.

## Workflow
- Small PRs, Conventional Commits, one concern each; update docs/ADR/threat-model if behaviour or surface changes.
- When a provider's ToS/limits are involved, update `docs/PROVIDERS.md` (*last verified* date) and use the compliance checklist.
- If requirements are ambiguous, add to `docs/OPEN-QUESTIONS.md` rather than guessing about safety-relevant behaviour.
- Don't run destructive git commands outside temp dirs.

## Suggested specialised agent roles
| Role | Focus | Reads |
|---|---|---|
| `git-engine` | `gitx`, `sync`, `cleanup` | ARCHITECTURE §5, THREAT-MODEL T6–T8 |
| `provider-dev` | one provider + contract tests | PROVIDERS.md |
| `ui-dev` | `ui`, a11y, CSRF | ARCHITECTURE §7, T1–T2 |
| `security-reviewer` | review diffs against threat model, run `make security` | THREAT-MODEL, SECURITY |
| `release-eng` | GoReleaser, signing, AV hygiene | DISTRIBUTION |
| `docs-writer` | user docs, config reference | all |

# Implementation plan

## Repository layout
```
cmd/repo-keeper/           main(): CLI (cobra-free: stdlib flag + small subcommand router)
internal/{config,secrets,provider,httpx,ratelimit,gitx,sync,cleanup,sched,store,ui,svc,obs}/
internal/provider/{github,gitlab,bitbucket,azdo,gitea,generic}/
internal/ui/{templates,static}/    embedded via go:embed
schema/config.schema.json          generated from Go types
testdata/{fixtures,repos}/
packaging/{systemd,launchd,windows,nix,brew,scoop,winget,deb,rpm}/
docs/ docs/adr/
.github/{workflows,ISSUE_TEMPLATE,CODEOWNERS,dependabot.yml}
Makefile  .golangci.yml  .goreleaser.yaml  go.mod
```

## Engineering conventions
- **Comments:** one line, explain *why*, not *what*. Every exported identifier gets a one-line doc comment starting with its name. No banner/ASCII/comment blocks > 3 lines.
- Errors wrapped with `%w`, typed sentinels for control flow; `context.Context` first arg; no globals; `slog` only.
- Small interfaces defined at the consumer. No package > ~1 500 LOC without reason.
- Conventional Commits; squash-merge; signed commits encouraged.
- SPDX header on each file: `// SPDX-License-Identifier: Apache-2.0`.

## Work breakdown (each item = code + tests + docs)

### Phase 0: Foundations (M0)
1. `go mod init`, Makefile (`build test lint security fmt gen`), golangci config, CI (lint, test matrix, govulncheck, CodeQL).
2. Repo hygiene: CONTRIBUTING, CODE_OF_CONDUCT (Contributor Covenant), issue/PR templates, CODEOWNERS, Dependabot, Scorecard action, DCO/sign-off policy.
3. `obs` (slog + redaction), `secrets.Token`, `Clock` abstraction.

### Phase 1: Local git engine (M1)
4. `gitx`: hardened runner (env scrub, config overrides, timeouts, askpass), parse porcelain v2/for-each-ref.
5. Default-branch detection; fetch/prune; FF logic; preflight (in-progress ops, locks, dirty).
6. `cleanup` predicate + trash/restore + audit journal + executor. Property + integration tests.
7. `store` + migrations; `sync` state machine using fakes.
8. CLI: `sync <path>`, `cleanup --dry-run`, `restore`, `doctor` (works on local repos with no provider).

### Phase 2: Rate limiting, scheduler, GitHub (M2–M3)
9. `httpx` (UA, retry, ETag cache, header parsing), `ratelimit` (bucket, cooldown, breaker).
10. Provider interface + contract test suite shared by all providers.
11. GitHub (token + device flow with BYO client id) → discovery, merged PRs, quota.
12. `secrets` keyring + fallback file; account management CLI.
13. Scheduler, worker pool, per-host limits, quiet hours, wake handling; `daemon` command.

### Phase 3: UI, shipped with GitHub in M3 (not deferred)
14. HTTP server, session/CSRF/Host checks, templates, htmx pages (dashboard, repo, accounts, rules, cleanup + audit journal, limits, logs) and a **debug page** (event stream, redacted git command log, force-run, state inspector, diagnostics bundle).
15. Config editing with schema validation + atomic save/history; hot-reload.
16. Playwright smoke + axe a11y in CI.

### Phase 4: Extensibility proof + more providers (M4, M6)
17. GitLab first (nested groups, proves the interface), then Azure DevOps (org/project) → Bitbucket Cloud → Gitea/Forgejo → generic.
18. Per-provider PROVIDERS.md verification + compliance checklist.

### Phase 5: Linux packaging and hardening (M5, M7)
19. systemd user unit, Nix flake + Home-Manager module, `.deb`/`.rpm`, container; launchd/Task Scheduler deferred.
20. GoReleaser, cosign, SBOM, SLSA, reproducibility check. Windows/macOS signing + VirusTotal/Defender submission deferred to their releases.
21. Fuzzing, perf benchmarks, hostile-repo suite, external security review/self-audit.
22. Docs: user guide, per-provider setup, troubleshooting, config reference (generated), man page.

### Phase 6: 1.0
23. Beta feedback, bugfix, API/config freeze, 1.0 release.

## Definition of done (per PR)
Tests added & green (incl. race), lint/security gates pass, docs/config reference updated, ADR if a decision changed, no new unexplained dependency, threat-model row updated if surface changed.

## Risks
| Risk | Mitigation |
|---|---|
| Provider ToS/limit changes | Compliance checklist, last-verified dates, header-driven limits not hard-coded |
| Deleting user work | Fail-closed predicate, trash refs, dry-run default, property tests |
| AV false positives | See DISTRIBUTION |
| Scope creep (becoming a git GUI) | Non-goals list; roadmap gate |
| Git behaviour differences across versions/OS | Version floor, CI matrix, `doctor` |
| Keychain unavailability on headless Linux | Encrypted-file fallback, documented |

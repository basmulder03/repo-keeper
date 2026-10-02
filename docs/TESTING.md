# Testing strategy

Tests are written **with** the code (same PR), never after. A PR without tests for new behaviour is not mergeable.

## Pyramid
| Layer | Tooling | Scope |
|---|---|---|
| Unit | `testing`, `testify`, table-driven, fake `Clock`/`Git`/`Provider`/`Store` | Pure logic: cleanup predicate, default-branch detection, scheduler, rate limiter, config validation, redaction |
| Property | `rapid` / `testing/quick` | Cleanup safety invariant: *never* selects current/default/worktree/unmerged/young branches for any generated repo graph |
| Git integration | Real `git` in `t.TempDir()` with a local bare remote | Clone, fetch, FF, diverged, dirty, rename `master→main`, force-pushed remote, squash-merge, worktrees, locked index, submodules |
| Provider contract | `httptest` + recorded fixtures (sanitised) | Pagination, ETag/304, 429/Retry-After, GitHub secondary limits, auth failure, renamed/deleted repos; UA header assertion |
| Security | Custom | Hostile-repo fixture (malicious hooks, `core.fsmonitor`, symlinks, `..` names) executes nothing; canary token never appears in logs/argv/env/DB; UI CSRF/Host/Origin tests |
| Fuzz | `go test -fuzz` | Config parser, ref-name & path sanitiser, rate-limit header parsers |
| API/UI | `httptest` + Playwright (smoke, a11y via axe) | Dashboard, edit config round-trip, review cleanup, token-less access denied |
| E2E | Script spinning fake providers + real git | Full cycle across 2 providers with injected rate limits; kill -9 mid-sync resumes cleanly |
| Cross-OS | CI matrix linux/macOS/windows | Path, locking, keychain (mock), service install in dry-run |
| Performance | Benchmarks + 500-repo synthetic run | NFR-1 budgets enforced via regression threshold |

## Quality gates (CI, required)
`go vet`, `staticcheck`, `golangci-lint`, `gosec`, `govulncheck`, `go test -race -cover ./...`, license check, `go mod tidy` diff, docs link check, generated-docs-up-to-date, coverage ≥ 85 % (`internal/cleanup` 100 %), fuzz seed corpus run, build reproducibility.

## Conventions
- Test names: `Test<Unit>_<Scenario>_<Expected>`.
- No network in unit tests; no test touches the real keychain, `$HOME`, or real provider.
- Fixtures sanitised and versioned in `testdata/`; recorded with a script, not by hand.
- Time is injected (`Clock`); no `time.Sleep` in tests.
- Every bug fix lands with a regression test.

# Performance

Budgets come from [REQUIREMENTS](REQUIREMENTS.md) (NFR-1, NFR-3). `make perf` (`scripts/perf.sh`) builds a synthetic fleet of local repositories, runs the real binary against it and fails if a budget is missed. Everything happens in a temp dir with isolated HOME, XDG and git config.

## Result (500 repositories, Linux, x86-64, 4 workers)

| Metric | Measured | Budget |
|---|---|---|
| Startup until the UI is listening | **68 ms** | < 300 ms |
| First full cycle (all 500 cloned repos synced) | 152 s | none (dominated by the 250 ms first-sync stagger: 500 × 0.25 s = 125 s) |
| Peak RSS during the cycle | **23 MB** | < 50 MB |
| Idle RSS | **23 MB** | < 50 MB |
| Idle CPU (30 s window) | **0.000 %** of one core | < 0.5 % |
| Dashboard response with 500 repos | **16 ms** | < 500 ms |
| Dashboard fragment refresh | 21 ms | none |
| Dashboard HTML size | 304 KB | none |
| `state.db` | 264 KB | none |

"Idle" is the window right after the first cycle, when every repository's next sync is about 30 minutes away. During continuous operation the cost is the git subprocesses of whichever repositories are due (one `ls-remote`, usually no fetch because the remote digest is unchanged).

## What the benchmark found and changed

1. **First-sync stagger of 2 s per repository** meant a 500-repo fleet needed ~17 minutes before the last repository's first sync. Reduced to 250 ms; real hosts are additionally paced by the rate limiter, so this only avoids a burst of process spawns on the local machine.
2. **Local remotes were being rate limited like a remote host** (1 request/s). They now bypass the host limiter (there is no server to protect); the worker pool still bounds concurrency.

## Known characteristics and limits

- The dashboard renders every repository on one page (304 KB for 500). It stays fast, but at several thousand repositories pagination will be needed (not implemented).
- Hosted platforms are the real bottleneck: with the default pacing (1 request/s per host, burst 5) a first sync of 500 GitHub repositories takes on the order of ten minutes by design.
- Numbers were taken on one machine; absolute values will differ, the budgets leave an order of magnitude of headroom.
- Re-run `make perf` after changes to the scheduler, store, UI templates or git runner.

# Decisions from maintainer review (resolved)

| # | Topic | Decision |
|---|---|---|
| 1 | Name | **repo-keeper** (avoids the `git-sync` name collision and `git sync` subcommand clash) |
| 2 | Language | Go |
| 3 | Git engine | System `git` CLI |
| 4 | UI | Embedded localhost web UI |
| 5 | Cleanup | User-configurable (`off`/`dry-run`/`auto`, global + per repo); safeguards always on (never dirty, never current/default/unmerged); **every removal tracked in an audit journal** |
| 6 | Remote branches | **Never deleted.** Local-only mutation; no write scopes requested |
| 7 | Providers | GitHub first; provider interface must make adding others easy (proved by M4) |
| 8 | Windows signing | Deferred until a Windows release; start with Linux/NixOS |
| 9 | OAuth apps | **Bring-your-own client ID** by default (device flow, no secret to ship); project-registered app optional later |
| 10 | Clone layout | Tree with N-level namespace (`provider/ns…/repo`; covers GitLab subgroups, Azure DevOps org/project) |
| 11 | Headless | Daemon + CLI + container supported, **UI ships from the first version** (with debug options) |
| 12 | Governance | DCO |
| 13 | Fetch scope | All branches (per-repo override) |
| 14 | Read-only repos | Included, subject to include/exclude rules |
| 15 | Folder rename | Done (`repo-keeper`) |
| 16 | Min git version | **2.34** (recommended ≥ 2.39), checked by `doctor` |
| 17 | UI access | `repo-keeper ui` (free port, one-time login URL) + optional tray helper, see ADR-0013 |

Folded into REQUIREMENTS (FR-S1, S6, C4, C6, C8, U1, O1, CMP-4), ROADMAP, ARCHITECTURE and ADR-0010..0012.

## Still open
None. Remaining choices are implementation details tracked in issues.

# Beta testing guide (0.1.0-beta.1)

Thank you for testing. repo-keeper is designed so that a mistake costs little: it only ever changes **local** clones, cleanup is **dry-run** until you switch it on, and every deletion is recoverable. Still, start small.

## What is safe, what to watch

| Safe by design | Watch these |
|---|---|
| Fast-forward only; dirty trees are skipped | First real run against a big account (hundreds of repos) |
| Cleanup is `dry-run` by default | Switching cleanup to `auto` (do this last, on one repo first) |
| Remote branches are never deleted | Anything involving a private CA, self-hosted GitLab, SSH remotes |
| Deleted branches go to a trash ref for 30 days | Behaviour with unusual repos (LFS, worktrees, submodules, huge repos) |

## 1. Install

**Nix / NixOS (your main path)**

```sh
# try it, nothing installed
nix run path:/home/you/Development/github/you/repo-keeper -- version

# install into your profile
nix profile install path:/home/you/Development/github/you/repo-keeper

# or as a Home Manager service: see docs/INSTALL.md (use the same path: URL as the flake input)
```

**Debian / Fedora / tarball:** see [INSTALL.md](INSTALL.md). Verify `checksums.txt` with `sha256sum --check` (signing starts with the first published release).

Check: `repo-keeper version` prints `0.1.0-beta.1`, and `repo-keeper doctor` is all `ok`.

## 2. Phase one: look, don't touch (15 minutes)

```sh
repo-keeper init                              # starter config; cleanup is dry-run
# GitHub: use a fine-grained, read-only token (Contents, Metadata, Pull requests: read)
repo-keeper accounts add personal --token-stdin --include 'you/*' < token.txt
$EDITOR ~/.config/repo-keeper/config.toml     # set [general] root = "/path/for/clones"
repo-keeper accounts check personal           # login, scopes, warnings, API quota
repo-keeper discover                          # what WOULD be cloned: nothing changes yet
```

No keychain? Use `--token-file /path/to/token` (mode 600) or set `[general] secrets = "file"` with `REPO_KEEPER_PASSPHRASE_FILE`.

## 3. Phase two: sync one small thing

Pick a narrow `include` (one org or a handful of repos), start the daemon (`repo-keeper daemon`, or the service) and open the UI with `repo-keeper ui`.

Things to verify:
- [ ] repositories appear under `<root>/github/<owner>/<repo>` (GitLab: nested groups keep their depth)
- [ ] the dashboard shows each as "up to date" after the first cycle; "Sync now" works
- [ ] a teammate-style push (push from another clone) shows up locally within one interval, on a clean checkout of the default branch
- [ ] a dirty checkout is reported "skipped-dirty" and left alone
- [ ] a repo whose default branch you renamed upstream is detected (UI shows it, origin/HEAD updates)
- [ ] the **Cleanup** page lists merged branches as *would-delete* (including ones merged by squash on GitHub/GitLab)
- [ ] `repo-keeper status` and the tray (if you run it) agree with the UI

## 4. Phase three: cleanup for real (only after phase two looks right)

1. In the UI, open one repo, review its cleanup table, and use **Delete safe branches now** on that single repo.
2. Check `repo-keeper audit` and the repo page: the branch is in the trash and the journal.
3. Practise the undo: **Restore** in the UI, or `repo-keeper restore <repo-path> <branch>`.
4. Only then set `[cleanup] mode = "auto"` (globally or per repo with `cleanup = "auto"`).

Safeguards you can rely on: never the default or current branch, never a branch checked out in a worktree, never a branch younger than `min_age` (7 days), never a branch that was never pushed (unless you opt in), never with a dirty tree, and never anything unless it is provably merged.

## 5. If something goes wrong

| Problem | What to do |
|---|---|
| Deleted a branch you wanted | `repo-keeper restore <repo> <branch>` (30 days), or `git branch <name> <sha>` using the sha in `repo-keeper audit` |
| Want it to stop | `systemctl --user stop repo-keeper` (or Ctrl-C); pause from the tray/`/api/pause`; remove the `[[account]]`/`[[repo]]` blocks. Clones are never deleted by repo-keeper |
| Token leaked/rotated | `repo-keeper accounts rm <name>`, revoke it at the provider, add a new one |
| A repo is stuck "needs attention" | The repo page shows the reason (`unsafe-config`, `path-occupied`, auth, rate limit...); fix it and press **Sync now** |
| Totally reset | stop the daemon, delete `~/.local/state/repo-keeper` (state and audit journal) and the config; your clones remain |

## 6. Reporting what you find

Open an issue (template: *Bug report*) with:
- `repo-keeper version`, OS, git version, platform (GitHub/GitLab, cloud or self-hosted)
- what you expected and what happened
- the **diagnostics bundle** (UI → Debug → *Download diagnostics bundle*): state, events, recent git commands and the config, scrubbed of secrets. Skim it before attaching; repository names and paths are included.

Security problems: do **not** open a public issue; follow [SECURITY.md](../SECURITY.md).

## What we most want to learn

1. Does discovery match what your account really contains (private, org, forks, archived, nested groups)?
2. Any API rate-limit trouble on large accounts (UI → Accounts shows the quota)?
3. Are squash-merged branches detected correctly, and is anything flagged that should not be?
4. Does the service/tray/Home Manager setup behave on your desktop?
5. Anything confusing in the UI or messages.

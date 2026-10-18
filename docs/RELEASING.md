# Releasing

One version, one place: the `VERSION` file. The Nix flake reads it, the release workflow refuses to run when anything disagrees, and the binary's own version comes from the git tag through GoReleaser (so it can only match if the tag matches `VERSION`).

1. Keep user-visible changes under `## Unreleased` in `CHANGELOG.md` while you work.
2. Run the **prepare-release** workflow (Actions tab, or `gh workflow run prepare-release.yml -f version=0.1.0-beta.6`). It runs `scripts/release-prep.sh` (sets `VERSION`, turns `## Unreleased` into `## 0.1.0-beta.6`, checks consistency; it refuses an empty Unreleased section), pushes `chore/release-0.1.0-beta.6`, opens the PR and dispatches `ci` on the branch (a PR opened with `GITHUB_TOKEN` starts no checks of its own). Doing it by hand is the same: `make release-prep NEW=0.1.0-beta.6`, commit as `chore: release 0.1.0-beta.6`, PR.
3. Let CI pass and merge the PR.
4. The **tag-release** workflow sees `VERSION` change on `main`, runs `scripts/check-version.sh`, pushes the annotated tag `v$(cat VERSION)` (skipped when it exists) and dispatches `release` on that tag. Tags pushed with `GITHUB_TOKEN` start nothing, hence the dispatch; running on the tag keeps the cosign identity `release.yml@refs/tags/vX`. A manual `git tag -a vX -m X && git push origin vX` still works.
5. The `release` workflow first runs `scripts/check-version.sh vX`: the tag must be `v$(cat VERSION)`, the newest changelog heading must be that version (no `Unreleased` left), and `flake.nix` must not contain a hardcoded version. Then GoReleaser builds, signs and attests a **draft** prerelease. Review it and publish by hand. The scoop and winget manifests are in the run's `package-manifests` artifact (docs/DISTRIBUTION.md).

Needs the repository setting "Allow GitHub Actions to create and approve pull requests" (Settings, Actions, General).

After you publish the draft, the **smoke-install** workflow installs the release on clean machines through `install.sh`, the verified tarball, `.deb`, `.rpm`, Scoop and `nix run`, and opens an issue when a channel fails. Run it by hand with `gh workflow run smoke-install.yml -f tag=vX`. It is not a required check. winget is covered by the validation bot on the `winget-pkgs` pull request.

## Flagging a security fix

The background update check marks a release as urgent when its notes contain a `### Security` heading (any level), a `- Security:` list item, or a CVE identifier. Write those notes in the GitHub release body (the changelog section is the natural source), so every installation that has the check turned on is told, in `status`, `doctor` and the web interface, that this release is worth updating to promptly.

`make version` runs the same consistency check locally; CI runs it on every pull request.

# Releasing

One version, one place: the `VERSION` file. The Nix flake reads it, the release workflow refuses to run when anything disagrees, and the binary's own version comes from the git tag through GoReleaser (so it can only match if the tag matches `VERSION`).

1. Keep user-visible changes under `## Unreleased` in `CHANGELOG.md` while you work.
2. Prepare: `make release-prep NEW=0.1.0-beta.6` (or `scripts/release-prep.sh 0.1.0-beta.6`). It sets `VERSION`, turns `## Unreleased` into `## 0.1.0-beta.6`, and runs the consistency check. It refuses an empty Unreleased section.
3. Commit as `chore: release 0.1.0-beta.6`, open the PR, let CI pass, merge.
4. Tag the merge commit on `main` and push the tag: `git tag -a v0.1.0-beta.6 -m 0.1.0-beta.6 && git push origin v0.1.0-beta.6`.
5. The `release` workflow first runs `scripts/check-version.sh vX`: the tag must be `v$(cat VERSION)`, the newest changelog heading must be that version (no `Unreleased` left), and `flake.nix` must not contain a hardcoded version. Then GoReleaser builds, signs and attests a **draft** prerelease. Review it and publish by hand.

## Flagging a security fix

The background update check marks a release as urgent when its notes contain a `### Security` heading (any level), a `- Security:` list item, or a CVE identifier. Write those notes in the GitHub release body (the changelog section is the natural source), so every installation that has the check turned on is told, in `status`, `doctor` and the web interface, that this release is worth updating to promptly.

`make version` runs the same consistency check locally; CI runs it on every pull request.

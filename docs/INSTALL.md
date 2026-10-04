# Installing repo-keeper

repo-keeper runs as a per-user background service (no root, no system service). Linux is the supported platform; Windows and macOS builds are not published until they can be code-signed.

## Quick install

Every route below verifies what it installs. None needs root except the distribution packages.

**Nix** (try it, or install it; add `/v0.1.0-beta.5`-style tags to pin a release):

```sh
nix run github:basmulder03/repo-keeper -- version
nix profile add github:basmulder03/repo-keeper
```

**Any Linux, amd64 or arm64** (verified tarball installer, installs to `~/.local/bin`):

```sh
curl -fsSLo install.sh https://basmulder03.github.io/repo-keeper/install.sh
sha256sum install.sh        # compare with the checksum on the Install page of the site
sh install.sh
```

The script downloads one pinned release, verifies the cosign signature of `checksums.txt` (identity pinned to this repository's release workflow *and* to that exact tag, so an older signed release cannot be replayed) and the SHA-256 of the archive, and only then installs. If any check fails nothing is installed. It needs `curl`, `tar` and `cosign`; without cosign it refuses unless you pass `--skip-signature`, which still checks the SHA-256 but says loudly that the signature was not checked. Options: `--version X.Y.Z`, `--prefix DIR` (units are only installed for the default `~/.local`), `--no-units`. systemd user units are copied but never enabled for you.

The shorter `curl -fsSL https://basmulder03.github.io/repo-keeper/install.sh | sh` works too, but then the script itself arrives unverified; the read-first form above lets you check it against the published checksum. macOS and Windows have no installer yet (builds are not published until they can be signed).

Debian, Ubuntu and Fedora packages are in the next sections; the Install page of the documentation site shows ready-to-paste commands pinned to the newest published release.

## NixOS / Nix

```nix
# flake.nix inputs
inputs.repo-keeper.url = "github:basmulder03/repo-keeper";
```

**Home Manager** (recommended; declarative config, user service, optional tray):

```nix
{ inputs, ... }: {
  imports = [ inputs.repo-keeper.homeManagerModules.default ];
  services.repo-keeper = {
    enable = true;
    tray.enable = true;
    settings = {
      general = { root = "/home/you/code"; interval = "30m"; };
      cleanup.mode = "dry-run";
      account = [{
        name = "personal"; provider = "github";
        token_file = config.age.secrets.github-token.path; # agenix / sops-nix; never the Nix store
        include = [ "you/*" "your-org/*" ];
      }];
    };
  };
}
```

With `settings` set, `~/.config/repo-keeper/config.toml` is generated read-only and the UI's forms are read-only by design (change the Nix, rebuild). Leave `settings` unset to manage the file yourself or through the UI.

**NixOS** (installs the package and the user units; each user opts in):

```nix
imports = [ inputs.repo-keeper.nixosModules.default ];
programs.repo-keeper = { enable = true; tray.enable = true; autostart = true; };
```

Try it without installing: `nix run github:basmulder03/repo-keeper -- doctor`.

## Debian / Ubuntu / Fedora

Download the `.deb` or `.rpm` from the release page, verify it (below), then:

```sh
sudo apt install ./repo-keeper_*_linux_amd64.deb     # or: sudo dnf install ./repo-keeper-*.rpm
repo-keeper init
systemctl --user enable --now repo-keeper            # optional tray: repo-keeper-tray
repo-keeper ui
```

The unit is sandboxed: it may write only its own state and `~/code`, `~/src`, `~/projects`, `~/git`. Cloning elsewhere? `systemctl --user edit repo-keeper` and add `[Service]` / `ReadWritePaths=/path/to/clones`.

## Tarball

```sh
tar xzf repo-keeper_*_linux_amd64.tar.gz
install -Dm755 repo-keeper repo-keeper-tray ~/.local/bin/
install -Dm644 systemd/*.service -t ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now repo-keeper
```

## Verifying a release

Every release contains `checksums.txt`, its cosign signature (`.sig`) and certificate (`.pem`), an SPDX SBOM per archive, and GitHub build-provenance attestations.

```sh
cosign verify-blob checksums.txt \
  --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/basmulder03/repo-keeper/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing checksums.txt
gh attestation verify repo-keeper_*_linux_amd64.tar.gz --repo basmulder03/repo-keeper
```

## Updating

repo-keeper checks about once a day whether a newer release exists (one anonymous request to the GitHub releases API; turn it off with `[update] check = false`) and tells you in `repo-keeper status`, `repo-keeper doctor` and the web interface, flagging releases that contain a security fix. It never installs anything by itself.

```sh
repo-keeper update            # same as --check: what exists, and the evidence to verify it
repo-keeper update --apply    # download, verify, ask, install (tarball installs only)
repo-keeper update --rollback # put back the version kept before the last update
```

`update` prints the release URL and notes, the exact asset URL, the expected SHA-256, the certificate identity and issuer the signature was checked against, the source commit and the build's workflow run, and the commands to repeat every check yourself. `--apply` needs `cosign`, a terminal and your explicit "y" (the default is No; there is no flag, setting or environment variable that skips the question), refuses unless the signature and the SHA-256 both pass, keeps the previous binary next to the new one, and never restarts the daemon for you: run `repo-keeper restart` when you are ready. `--version X` installs exactly that release, which is also how you downgrade.

Installs owned by a package manager do not update themselves: Nix (`nix profile upgrade repo-keeper`, or bump your flake input / Home Manager), `.deb`/`.rpm` (install the newer package), Homebrew, containers. `update` recognises them, says so and prints the right command.

## First run

1. `repo-keeper init` writes a starter config.
2. `repo-keeper accounts add personal --token-stdin < token.txt` (or `--token-file`/`--token-env` without a keychain).
3. Set `[general] root = "/path/to/clones"`, check with `repo-keeper discover`.
4. Start the service, open the interface with `repo-keeper ui`.

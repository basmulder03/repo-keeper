# Installing repo-keeper

repo-keeper runs as a per-user background service (no root, no system service). Linux is the supported platform; Windows and macOS builds are not published until they can be code-signed.

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

With `settings` set, `~/.config/repo-keeper/config.toml` is generated read-only and the UI's config editor is disabled by design (change the Nix, rebuild). Leave `settings` unset to manage the file yourself or through the UI.

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

## First run

1. `repo-keeper init` writes a starter config.
2. `repo-keeper accounts add personal --token-stdin < token.txt` (or `--token-file`/`--token-env` without a keychain).
3. Set `[general] root = "/path/to/clones"`, check with `repo-keeper discover`.
4. Start the service, open the interface with `repo-keeper ui`.

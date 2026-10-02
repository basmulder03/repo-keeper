{
  description = "repo-keeper: keeps local clones of your remote repositories in sync";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    # Only used by `nix flake check` to evaluate the Home Manager module for real.
    home-manager = {
      url = "github:nix-community/home-manager";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, home-manager }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "repo-keeper";
          version = "0.1.0-beta.1";
          src = ./.;
          vendorHash = "sha256-RYbPB9cNhC836bnmoTWCgQfpWrRoaz0V/wqKq4hmS18=";
          env.CGO_ENABLED = 0;
          subPackages = [ "cmd/repo-keeper" "cmd/repo-keeper-tray" ];
          ldflags = [ "-s" "-w" "-X main.version=0.1.0-beta.1" "-X main.commit=${self.shortRev or self.dirtyShortRev or "dev"}" ];
          nativeCheckInputs = [ pkgs.git ];
          meta = {
            description = "Keeps local clones of your remote repositories in sync";
            license = pkgs.lib.licenses.asl20;
            mainProgram = "repo-keeper";
          };
        };
      });

      apps = forAll (pkgs: {
        default = {
          type = "app";
          program = "${self.packages.${pkgs.stdenv.hostPlatform.system}.default}/bin/repo-keeper";
          meta.description = "repo-keeper command line";
        };
      });

      homeModules.default = import ./nix/home-manager.nix self;
      homeManagerModules.default = self.homeModules.default; # older name, still what most configs import
      nixosModules.default = import ./nix/nixos.nix self;

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [ go gnumake git golangci-lint govulncheck gosec goreleaser ];
        };
      });

      checks = forAll (pkgs:
        let
          system = pkgs.stdenv.hostPlatform.system;
          hm = home-manager.lib.homeManagerConfiguration {
            inherit pkgs;
            modules = [
              self.homeManagerModules.default
              {
                home = { username = "tester"; homeDirectory = "/home/tester"; stateVersion = "24.11"; };
                services.repo-keeper = {
                  enable = true;
                  tray.enable = true;
                  logLevel = "debug";
                  environmentFile = "/run/secrets/rk.env";
                  extraReadWritePaths = [ "/mnt/clones" ];
                  settings = {
                    general = { root = "/home/tester/code"; interval = "45m"; };
                    cleanup.mode = "dry-run";
                    account = [{ name = "gh"; provider = "github"; token_file = "/run/secrets/gh"; include = [ "me/*" ]; }];
                    repo = [{ path = "/home/tester/code/x"; cleanup = "auto"; }];
                  };
                };
              }
            ];
          };
          nixos = nixpkgs.lib.nixosSystem {
            inherit system;
            modules = [
              self.nixosModules.default
              { boot.isContainer = true; system.stateVersion = "24.11"; programs.repo-keeper = { enable = true; tray.enable = true; autostart = true; }; }
            ];
          };
          nixosUnit = nixos.config.systemd.user.units."repo-keeper.service".text;
          binary = self.packages.${system}.default;
        in
        {
          package = binary;

          # The generated config must be accepted by repo-keeper's own validator, and the units must carry the
          # sandbox, the exact binary, the clone root and git on PATH.
          home-manager-module = pkgs.runCommand "hm-module-check" { nativeBuildInputs = [ pkgs.gnugrep ]; } ''
            files=${hm.activationPackage}/home-files
            cfg=$files/.config/repo-keeper/config.toml
            unit=$files/.config/systemd/user/repo-keeper.service
            tray=$files/.config/systemd/user/repo-keeper-tray.service
            ${binary}/bin/repo-keeper config validate --config "$cfg"
            grep -q 'token_file = "/run/secrets/gh"' "$cfg"
            grep -q 'interval = "45m"' "$cfg"
            grep -q 'ExecStart=${binary}/bin/repo-keeper daemon --log-level debug' "$unit"
            grep -q '^ProtectSystem=strict' "$unit"
            grep -q '^NoNewPrivileges=true' "$unit"
            grep -q "^MemoryDenyWriteExecute=true" "$unit"
            grep -q "^UMask=0077" "$unit"
            grep -q "^SystemCallFilter=~@privileged" "$unit"
            grep -q "^CapabilityBoundingSet=$" "$unit"
            grep -q '^ReadWritePaths=/home/tester/code' "$unit"
            grep -q '^ReadWritePaths=/mnt/clones' "$unit"
            grep -q '^RuntimeDirectory=repo-keeper' "$unit"
            grep -q '^EnvironmentFile=/run/secrets/rk.env' "$unit"
            grep -q 'PATH=.*git' "$unit"
            grep -q 'WantedBy=default.target' "$unit"
            grep -q 'ExecStart=${binary}/bin/repo-keeper-tray' "$tray"
            grep -q 'WantedBy=graphical-session.target' "$tray"
            if grep -rq 'ghp_\|glpat' $files; then echo "token-looking string in generated files"; exit 1; fi
            touch $out
          '';

          nixos-module = pkgs.runCommand "nixos-module-check" { nativeBuildInputs = [ pkgs.gnugrep ]; } ''
            cat > unit <<'UNIT'
            ${nixosUnit}
            UNIT
            grep -q 'ExecStart=${binary}/bin/repo-keeper daemon' unit
            grep -q 'ProtectSystem=strict' unit
            grep -q 'ReadWritePaths=-%h/code' unit
            grep -q 'WantedBy=default.target' unit
            grep -q 'git' unit
            touch $out
          '';
        });
    };
}

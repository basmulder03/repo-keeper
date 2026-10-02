{
  description = "repo-keeper: keeps local clones of your remote repositories in sync";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "repo-keeper";
          version = self.shortRev or self.dirtyShortRev or "dev";
          src = ./.;
          vendorHash = "sha256-8F+y7ZFCew6sLn5QP/ytSikaHV3ENw6tUzQy1kRfGL0="; # refresh with lib.fakeHash after changing go.mod
          env.CGO_ENABLED = 0;
          subPackages = [ "cmd/repo-keeper" ];
          ldflags = [ "-s" "-w" "-X main.version=${self.shortRev or "dev"}" ];
          nativeCheckInputs = [ pkgs.git ];
          meta = {
            description = "Keeps local clones of your remote repositories in sync";
            license = pkgs.lib.licenses.asl20;
            mainProgram = "repo-keeper";
          };
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [ go gnumake git golangci-lint govulncheck gosec ];
        };
      });
    };
}

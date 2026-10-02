# NixOS module: installs the package and defines the user units; each user's config stays in ~/.config/repo-keeper.
self:
{ config, lib, pkgs, ... }:
let
  cfg = config.programs.repo-keeper;
  common = import ./common.nix { inherit lib pkgs; };
in
{
  options.programs.repo-keeper = {
    enable = lib.mkEnableOption "repo-keeper (package and systemd user units)";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "inputs.repo-keeper.packages.\${system}.default";
    };

    autostart = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Start the daemon for every user at login. Off by default: users opt in with `systemctl --user enable --now repo-keeper`.";
    };

    logLevel = lib.mkOption {
      type = lib.types.enum [ "debug" "info" "warn" "error" ];
      default = "info";
    };

    extraReadWritePaths = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Additional directories the sandboxed daemon may write to (default: ~/code ~/src ~/projects ~/git if they exist).";
    };

    tray.enable = lib.mkEnableOption "the optional tray icon user unit";
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];

    systemd.user.services.repo-keeper = {
      description = "repo-keeper: keep local clones in sync";
      after = [ "network-online.target" ];
      path = [ pkgs.git pkgs.openssh ];
      wantedBy = lib.optional cfg.autostart "default.target";
      serviceConfig = common.hardening // {
        ExecStart = "${cfg.package}/bin/repo-keeper ${common.daemonArgs { inherit (cfg) logLevel; noUI = false; }}";
        Restart = "on-failure";
        RestartSec = 30;
        StateDirectory = "repo-keeper";
        RuntimeDirectory = "repo-keeper";
        ReadWritePaths = common.writablePaths { settingsRoot = null; extra = cfg.extraReadWritePaths; };
      };
    };

    systemd.user.services.repo-keeper-tray = lib.mkIf cfg.tray.enable {
      description = "repo-keeper tray icon";
      partOf = [ "graphical-session.target" ];
      after = [ "graphical-session.target" ];
      wantedBy = lib.optional cfg.autostart "graphical-session.target";
      serviceConfig = common.hardening // {
        ExecStart = "${cfg.package}/bin/repo-keeper-tray";
        Restart = "on-failure";
        RestartSec = 10;
      };
    };
  };
}

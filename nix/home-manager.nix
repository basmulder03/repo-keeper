# Home Manager module: declarative config, user service, optional tray.
self:
{ config, lib, pkgs, ... }:
let
  cfg = config.services.repo-keeper;
  common = import ./common.nix { inherit lib pkgs; };
  toml = pkgs.formats.toml { };
  root = lib.attrByPath [ "general" "root" ] null cfg.settings;
in
{
  options.services.repo-keeper = {
    enable = lib.mkEnableOption "repo-keeper, a background service that keeps local clones of your remote repositories in sync";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "inputs.repo-keeper.packages.\${system}.default";
      description = "The repo-keeper package (provides repo-keeper and repo-keeper-tray).";
    };

    settings = lib.mkOption {
      type = lib.types.nullOr toml.type;
      default = null;
      example = lib.literalExpression ''
        {
          general = { root = "/home/you/code"; interval = "30m"; };
          cleanup.mode = "dry-run";
          account = [ {
            name = "personal"; provider = "github";
            token_file = "/run/agenix/github-token";
            include = [ "you/*" ];
          } ];
        }
      '';
      description = ''
        Contents of `~/.config/repo-keeper/config.toml`. When set, the file is generated read-only and the
        web UI's config editor is disabled; when null the file is yours (edit it by hand or in the UI).
        Never put tokens here: this attribute set ends up in the world-readable Nix store. Use `token_file`
        (agenix, sops-nix, systemd credentials) or `token_env` with `environmentFile`.
      '';
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "systemd EnvironmentFile for `token_env` accounts (KEY=value lines). Keep it out of the Nix store.";
    };

    logLevel = lib.mkOption {
      type = lib.types.enum [ "debug" "info" "warn" "error" ];
      default = "info";
    };

    disableUI = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Start the daemon with --no-ui.";
    };

    extraReadWritePaths = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Additional directories the sandboxed daemon may write to (clone roots outside `general.root`).";
    };

    tray.enable = lib.mkEnableOption "the optional tray icon (needs a StatusNotifier host: KDE, waybar, GNOME AppIndicator)";
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.settings == null || !(lib.hasInfix "/nix/store" (builtins.toJSON cfg.settings));
        message = "services.repo-keeper.settings must not reference store paths for secrets; use an absolute token_file outside the store.";
      }
    ];

    home.packages = [ cfg.package ];

    xdg.configFile."repo-keeper/config.toml" = lib.mkIf (cfg.settings != null) {
      source = toml.generate "repo-keeper-config.toml" cfg.settings;
    };

    systemd.user.services.repo-keeper = {
      Unit = {
        Description = "repo-keeper: keep local clones in sync";
        After = [ "network-online.target" ];
      };
      Service = common.hardening // {
        ExecStart = "${cfg.package}/bin/repo-keeper ${common.daemonArgs { inherit (cfg) logLevel; noUI = cfg.disableUI; }}";
        Restart = "on-failure";
        RestartSec = 30;
        StateDirectory = "repo-keeper";
        RuntimeDirectory = "repo-keeper";
        Environment = [ "PATH=${common.servicePath}" ];
        ReadWritePaths = common.writablePaths { settingsRoot = root; extra = cfg.extraReadWritePaths; };
      } // lib.optionalAttrs (cfg.environmentFile != null) { EnvironmentFile = toString cfg.environmentFile; };
      Install.WantedBy = [ "default.target" ];
    };

    systemd.user.services.repo-keeper-tray = lib.mkIf cfg.tray.enable {
      Unit = {
        Description = "repo-keeper tray icon";
        PartOf = [ "graphical-session.target" ];
        After = [ "graphical-session.target" ];
      };
      Service = common.hardening // {
        ExecStart = "${cfg.package}/bin/repo-keeper-tray";
        Restart = "on-failure";
        RestartSec = 10;
      };
      Install.WantedBy = [ "graphical-session.target" ];
    };
  };
}

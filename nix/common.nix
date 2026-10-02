# Shared by the Home Manager and NixOS modules.
{ lib, pkgs }:
rec {
  # Where the sandbox may write besides its own state dirs. The package units use the same optional defaults.
  defaultWritable = [ "-%h/code" "-%h/src" "-%h/projects" "-%h/git" ];

  # systemd hardening: the daemon only needs its state/runtime dirs, the clone folders and the network.
  hardening = {
    NoNewPrivileges = true;
    PrivateTmp = true;
    ProtectSystem = "strict";
    ProtectHome = "read-only";
    ProtectKernelTunables = true;
    ProtectControlGroups = true;
    RestrictSUIDSGID = true;
    RestrictNamespaces = true;
    SystemCallArchitectures = "native";
    RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
    LockPersonality = true;
    UMask = "0077";
    PrivateDevices = true;
    ProtectClock = true;
    ProtectHostname = true;
    ProtectKernelLogs = true;
    ProtectKernelModules = true;
    ProtectProc = "invisible";
    ProcSubset = "pid";
    CapabilityBoundingSet = "";
    RestrictRealtime = true;
    MemoryDenyWriteExecute = true;
    SystemCallFilter = [ "@system-service" "~@privileged" ];
  };

  # git for syncing, openssh for ssh remotes; nothing else is on the service's PATH.
  servicePath = lib.makeBinPath [ pkgs.git pkgs.openssh ];

  writablePaths = { settingsRoot, extra }:
    (lib.optional (settingsRoot != null) settingsRoot) ++ defaultWritable ++ extra;

  daemonArgs = { logLevel, noUI }:
    "daemon --log-level ${logLevel}" + lib.optionalString noUI " --no-ui";
}

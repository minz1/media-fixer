{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.media-agent;
in {
  options.services.media-agent = {
    enable = lib.mkEnableOption "media-agent sidecar for minz-media-0";

    package = lib.mkPackageOption pkgs "media-agent" {};

    addr = lib.mkOption {
      type = lib.types.str;
      default = ":9191";
      description = "Listen address for the HTTP server.";
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        Path to a file containing secret environment variables loaded by systemd.
        Expected variables:
          MEDIA_AGENT_API_KEY

        With sops-nix, set this to config.sops.secrets."media-agent-env".path.
      '';
    };

    diskMounts = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = ["/mnt/decypharr" "/var/cache/decypharr" "/data"];
      description = ''
        Mount points reported in GET /disk responses, and the allowlist every
        path-taking operation (/ls, /dd-test) is restricted to. A path that
        does not resolve inside one of these roots is refused, so removing a
        root here disables agent access to it.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.media-agent = {
      description = "media-agent sidecar — dd tests and service restarts for media-fixer";
      wantedBy = ["multi-user.target"];
      after = ["network-online.target"];
      wants = ["network-online.target"];

      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} -addr ${cfg.addr} ${lib.concatMapStringsSep " " (m: "-disk-mount ${lib.escapeShellArg m}") cfg.diskMounts}";
        Restart = "on-failure";
        RestartSec = "5s";

        # Needs to run as root to call systemctl restart and open arbitrary paths.
        User = "root";
        Group = "root";

        PrivateTmp = true;
        ProtectHome = true;
        NoNewPrivileges = true;
        RestrictAddressFamilies = ["AF_INET" "AF_INET6" "AF_UNIX"];
        RestrictNamespaces = true;
        LockPersonality = true;
        SystemCallFilter = ["@system-service" "~@privileged"];
      } // lib.optionalAttrs (cfg.environmentFile != null) {
        EnvironmentFile = cfg.environmentFile;
      };
    };
  };
}

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
      default = ["/mnt" "/var"];
      description = "Mount points to report in GET /disk responses.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.media-agent = {
      description = "media-agent sidecar — dd tests and service restarts for media-fixer";
      wantedBy = ["multi-user.target"];
      after = ["network-online.target"];
      wants = ["network-online.target"];

      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} -addr ${cfg.addr}";
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

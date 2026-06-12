{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.media-fixer;
  settingsFormat = pkgs.formats.toml {};

  configFile = settingsFormat.generate "media-fixer.toml" {
    server = {
      addr = cfg.addr;
      base_url = cfg.baseURL;
    };
    db = {
      path = "/var/lib/media-fixer/media-fixer.db";
    };
    discord = {
      guild_id = cfg.discord.guildID;
      owner_id = cfg.discord.ownerID;
      report_channel = cfg.discord.reportChannel;
    };
    llm = {
      base_url = cfg.llm.baseURL;
      model = cfg.llm.model;
    };
    decypharr = {
      url = cfg.decypharr.url;
    };
    jellyfin = {
      url = cfg.jellyfin.url;
    };
    sonarr = {
      url = cfg.sonarr.url;
    };
    radarr = {
      url = cfg.radarr.url;
    };
    loki = {
      url = cfg.loki.url;
    };
    media = {
      host = cfg.media.host;
      port = cfg.media.port;
      user = cfg.media.user;
      ssh_key_path = cfg.media.sshKeyPath;
    };
  };
in {
  options.services.media-fixer = {
    enable = lib.mkEnableOption "media-fixer self-healing media stack manager";

    package = lib.mkPackageOption pkgs "media-fixer" {};

    addr = lib.mkOption {
      type = lib.types.str;
      default = ":8080";
      description = "Listen address for the HTTP server.";
    };

    baseURL = lib.mkOption {
      type = lib.types.str;
      default = "/media";
      description = "URL base path for the dashboard.";
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        Path to a file containing secret environment variables loaded by systemd.
        Expected variables (at minimum):
          MEDIA_FIXER_DISCORD_TOKEN
          MEDIA_FIXER_LLM_API_KEY
          MEDIA_FIXER_DECYPHARR_API_TOKEN
          MEDIA_FIXER_JELLYFIN_API_KEY
          MEDIA_FIXER_SONARR_API_KEY
          MEDIA_FIXER_RADARR_API_KEY

        With sops-nix, set this to config.sops.secrets."media-fixer-env".path.
      '';
    };

    discord = {
      guildID = lib.mkOption {
        type = lib.types.str;
        description = "Discord guild (server) ID where /report is registered.";
      };
      ownerID = lib.mkOption {
        type = lib.types.str;
        description = "Discord user ID to DM for escalations and approvals.";
      };
      reportChannel = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Optional channel ID for report confirmations.";
      };
    };

    llm = {
      baseURL = lib.mkOption {
        type = lib.types.str;
        default = "https://openrouter.ai/api/v1";
        description = "OpenAI-compatible API base URL (OpenRouter, ecoGPT, etc.).";
      };
      model = lib.mkOption {
        type = lib.types.str;
        default = "anthropic/claude-sonnet-4-6";
        description = "Model name to pass to the LLM API.";
      };
    };

    decypharr = {
      url = lib.mkOption {
        type = lib.types.str;
        description = "Base URL of the decypharr HTTP API.";
        example = "http://10.0.0.2:8282";
      };
    };

    jellyfin = {
      url = lib.mkOption {
        type = lib.types.str;
        description = "Base URL of the Jellyfin server.";
        example = "http://10.0.0.2:8096";
      };
    };

    sonarr = {
      url = lib.mkOption {
        type = lib.types.str;
        description = "Base URL of the Sonarr server.";
        example = "http://10.0.0.2:8989";
      };
    };

    radarr = {
      url = lib.mkOption {
        type = lib.types.str;
        description = "Base URL of the Radarr server.";
        example = "http://10.0.0.2:7878";
      };
    };

    loki = {
      url = lib.mkOption {
        type = lib.types.str;
        description = "Base URL of the Loki server.";
        example = "http://obs-0:3100";
      };
    };

    media = {
      host = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "WireGuard IP of minz-media-0 for SSH-based diagnostics and restarts.";
        example = "10.100.0.2";
      };
      port = lib.mkOption {
        type = lib.types.port;
        default = 22;
        description = "SSH port on the media host.";
      };
      user = lib.mkOption {
        type = lib.types.str;
        default = "root";
        description = "SSH user on the media host.";
      };
      sshKeyPath = lib.mkOption {
        type = lib.types.str;
        default = "/var/lib/media-fixer/media-host-key";
        description = ''
          Path to the SSH private key for connecting to the media host.
          With DynamicUser the service state dir is /var/lib/media-fixer.
          Generate with: ssh-keygen -t ed25519 -f /var/lib/media-fixer/media-host-key
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.media-fixer = {
      description = "media-fixer self-healing media stack manager";
      wantedBy = ["multi-user.target"];
      after = ["network-online.target"];
      wants = ["network-online.target"];

      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} -config ${configFile}";
        Restart = "on-failure";
        RestartSec = "5s";

        # Security hardening.
        DynamicUser = true;
        StateDirectory = "media-fixer";
        StateDirectoryMode = "0750";
        PrivateTmp = true;
        ProtectHome = true;
        ProtectSystem = "strict";
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        RestrictAddressFamilies = ["AF_INET" "AF_INET6" "AF_UNIX"];
        RestrictNamespaces = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallFilter = ["@system-service" "~@privileged"];
      } // lib.optionalAttrs (cfg.environmentFile != null) {
        EnvironmentFile = cfg.environmentFile;
      };
    };
  };
}

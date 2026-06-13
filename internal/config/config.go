package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server     ServerConfig     `toml:"server"`
	DB         DBConfig         `toml:"db"`
	Discord    DiscordConfig    `toml:"discord"`
	LLM        LLMConfig        `toml:"llm"`
	ControlLLM *LLMConfig       `toml:"control_llm"`
	Decypharr  DecypharrConfig  `toml:"decypharr"`
	Jellyfin   JellyfinConfig   `toml:"jellyfin"`
	Sonarr     ArrConfig        `toml:"sonarr"`
	Radarr     ArrConfig        `toml:"radarr"`
	Loki       LokiConfig       `toml:"loki"`
	MediaAgent MediaAgentConfig `toml:"media_agent"`
}

type ServerConfig struct {
	Addr    string `toml:"addr"`
	BaseURL string `toml:"base_url"`
}

type DBConfig struct {
	Path string `toml:"path"`
}

type DiscordConfig struct {
	Token         string `toml:"token"`
	GuildID       string `toml:"guild_id"`
	OwnerID       string `toml:"owner_id"`
	ReportChannel string `toml:"report_channel"`
}

type LLMConfig struct {
	BaseURL string `toml:"base_url"`
	APIKey  string `toml:"api_key"`
	Model   string `toml:"model"`
}

type DecypharrConfig struct {
	URL      string `toml:"url"`
	APIToken string `toml:"api_token"`
}

type JellyfinConfig struct {
	URL    string `toml:"url"`
	APIKey string `toml:"api_key"`
}

type ArrConfig struct {
	URL    string `toml:"url"`
	APIKey string `toml:"api_key"`
}

type LokiConfig struct {
	URL     string `toml:"url"`
	TLSCert string `toml:"tls_cert"` // path to PEM client cert
	TLSKey  string `toml:"tls_key"`  // path to PEM client key
}

// MediaAgentConfig holds connection details for the media-agent sidecar on minz-media-0.
type MediaAgentConfig struct {
	URL    string `toml:"url"`
	APIKey string `toml:"api_key"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Addr:    ":8080",
			BaseURL: "/media",
		},
		DB: DBConfig{
			Path: "/var/lib/media-fixer/media-fixer.db",
		},
	}

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}

	applyEnvOverrides(cfg)
	applyControlLLMDefaults(cfg)

	return cfg, nil
}

// applyEnvOverrides overlays secrets from environment variables so that
// systemd EnvironmentFile (sops-nix) can inject them without them
// appearing in the world-readable TOML file.
func applyEnvOverrides(cfg *Config) {
	envOverride("MEDIA_FIXER_DISCORD_TOKEN", &cfg.Discord.Token)
	envOverride("MEDIA_FIXER_LLM_API_KEY", &cfg.LLM.APIKey)
	envOverride("MEDIA_FIXER_DECYPHARR_API_TOKEN", &cfg.Decypharr.APIToken)
	envOverride("MEDIA_FIXER_JELLYFIN_API_KEY", &cfg.Jellyfin.APIKey)
	envOverride("MEDIA_FIXER_SONARR_API_KEY", &cfg.Sonarr.APIKey)
	envOverride("MEDIA_FIXER_RADARR_API_KEY", &cfg.Radarr.APIKey)
	envOverride("MEDIA_FIXER_MEDIA_AGENT_API_KEY", &cfg.MediaAgent.APIKey)
	envOverride("MEDIA_FIXER_LOKI_TLS_CERT", &cfg.Loki.TLSCert)
	envOverride("MEDIA_FIXER_LOKI_TLS_KEY", &cfg.Loki.TLSKey)

	if v := os.Getenv("MEDIA_FIXER_CONTROL_LLM_API_KEY"); v != "" {
		if cfg.ControlLLM == nil {
			cfg.ControlLLM = &LLMConfig{}
		}
		cfg.ControlLLM.APIKey = v
	}
}

// applyControlLLMDefaults fills unset control_llm fields from the main [llm] block.
func applyControlLLMDefaults(cfg *Config) {
	if cfg.ControlLLM == nil {
		return
	}
	if cfg.ControlLLM.BaseURL == "" {
		cfg.ControlLLM.BaseURL = cfg.LLM.BaseURL
	}
	if cfg.ControlLLM.APIKey == "" {
		cfg.ControlLLM.APIKey = cfg.LLM.APIKey
	}
	if cfg.ControlLLM.Model == "" {
		cfg.ControlLLM.Model = cfg.LLM.Model
	}
}

func envOverride(key string, target *string) {
	if v := os.Getenv(key); v != "" {
		*target = v
	}
}

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
	Decypharr  DecypharrConfig  `toml:"decypharr"`
	Jellyfin   JellyfinConfig   `toml:"jellyfin"`
	Sonarr     ArrConfig        `toml:"sonarr"`
	Radarr     ArrConfig        `toml:"radarr"`
	Loki       LokiConfig       `toml:"loki"`
	Media      MediaHostConfig  `toml:"media"`
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
	URL string `toml:"url"`
}

// MediaHostConfig holds SSH details for the media server (minz-media-0),
// used for dd readability tests and service restarts over WireGuard.
type MediaHostConfig struct {
	Host       string `toml:"host"`
	Port       int    `toml:"port"`
	User       string `toml:"user"`
	SSHKeyPath string `toml:"ssh_key_path"`
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
		Media: MediaHostConfig{
			Port: 22,
			User: "root",
		},
	}

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}

	// Allow secrets to be overridden via environment variables so that
	// systemd EnvironmentFile (sops-nix) can inject them without them
	// appearing in the world-readable TOML file.
	if v := os.Getenv("MEDIA_FIXER_DISCORD_TOKEN"); v != "" {
		cfg.Discord.Token = v
	}
	if v := os.Getenv("MEDIA_FIXER_LLM_API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := os.Getenv("MEDIA_FIXER_DECYPHARR_API_TOKEN"); v != "" {
		cfg.Decypharr.APIToken = v
	}
	if v := os.Getenv("MEDIA_FIXER_JELLYFIN_API_KEY"); v != "" {
		cfg.Jellyfin.APIKey = v
	}
	if v := os.Getenv("MEDIA_FIXER_SONARR_API_KEY"); v != "" {
		cfg.Sonarr.APIKey = v
	}
	if v := os.Getenv("MEDIA_FIXER_RADARR_API_KEY"); v != "" {
		cfg.Radarr.APIKey = v
	}

	return cfg, nil
}

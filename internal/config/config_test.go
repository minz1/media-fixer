package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/config"
)

func TestLoad_PublicURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty", "", false},
		{"https with base path", "https://admin.minz1.com/media", false},
		{"http", "http://h/x", false},
		{"ftp scheme", "ftp://h/x", true},
		{"relative", "/media", true},
		{"no host", "https://", true},
		{"trailing slash", "https://h/media/", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "c.toml")
			body := "[server]\npublic_url = \"" + tt.value + "\"\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := config.Load(path)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "public_url") {
					t.Fatalf("err = %v, want error naming public_url", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server.PublicURL != tt.value {
				t.Fatalf("PublicURL = %q, want %q", cfg.Server.PublicURL, tt.value)
			}
		})
	}
}

func TestC9_ConfigBazarr(t *testing.T) {
	t.Setenv("MEDIA_FIXER_BAZARR_API_KEY", "key")
	t.Setenv("MEDIA_FIXER_BAZARR_WEBHOOK_SECRET", "sec")
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte("[bazarr]\nurl = \"http://b/bazarr\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := config.BazarrConfig{URL: "http://b/bazarr", APIKey: "key", WebhookSecret: "sec"}
	if cfg.Bazarr != want {
		t.Fatalf("bazarr = %+v, want %+v", cfg.Bazarr, want)
	}
}

func TestRS2_ResyncMaxGBDefault(t *testing.T) {
	t.Parallel()
	if got := (config.BazarrConfig{}).ResyncMaxBytes(); got != 20<<30 {
		t.Errorf("default = %d", got)
	}
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte("[bazarr]\nresync_max_gb = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Bazarr.ResyncMaxBytes(); got != 5<<30 {
		t.Errorf("configured = %d", got)
	}
}

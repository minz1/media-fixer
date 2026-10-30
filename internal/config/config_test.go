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

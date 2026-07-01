package agent_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
)

func TestFixLokiUnitSelector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "regex bare names get service suffix",
			input: `{unit=~"jellyfin|decypharr"}`,
			want:  `{unit=~"jellyfin\.service|decypharr\.service"}`,
		},
		{
			name:  "exact bare name gets service suffix",
			input: `{unit="jellyfin"}`,
			want:  `{unit="jellyfin.service"}`,
		},
		{
			name:  "regex already correct dot-escaped unchanged",
			input: `{unit=~"jellyfin\.service|decypharr\.service"}`,
			want:  `{unit=~"jellyfin\.service|decypharr\.service"}`,
		},
		{
			name:  "regex already correct unescaped dot unchanged",
			input: `{unit=~"jellyfin.service|decypharr.service"}`,
			want:  `{unit=~"jellyfin.service|decypharr.service"}`,
		},
		{
			name:  "exact already correct unchanged",
			input: `{unit="jellyfin.service"}`,
			want:  `{unit="jellyfin.service"}`,
		},
		{
			name:  "mixed: only bare name gets fixed",
			input: `{unit=~"jellyfin|decypharr.service"}`,
			want:  `{unit=~"jellyfin\.service|decypharr.service"}`,
		},
		{
			name:  "non-unit selector unchanged",
			input: `{job="systemd-journal"}`,
			want:  `{job="systemd-journal"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := agent.FixLokiUnitSelector(tc.input)
			if got != tc.want {
				t.Errorf("fixLokiUnitSelector(%q)\n got  %q\n want %q", tc.input, got, tc.want)
			}
		})
	}
}

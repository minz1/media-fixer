package client_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

func TestD11_3_EveryClientConfiguredMeansURLSet(t *testing.T) {
	t.Parallel()
	loki, err := client.NewLoki("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	setLoki, err := client.NewLoki("http://loki", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"arr nil", (*client.ArrClient)(nil).Configured(), false},
		{"arr empty", client.NewArr("", "k").Configured(), false},
		{"arr set", client.NewArr("http://a", "k").Configured(), true},
		{"decypharr nil", (*client.DecypharrClient)(nil).Configured(), false},
		{"decypharr empty", client.NewDecypharr("", "k").Configured(), false},
		{"decypharr set", client.NewDecypharr("http://d", "k").Configured(), true},
		{"jellyfin nil", (*client.JellyfinClient)(nil).Configured(), false},
		{"jellyfin empty", client.NewJellyfin("", "k").Configured(), false},
		{"jellyfin set", client.NewJellyfin("http://j", "k").Configured(), true},
		{"loki nil", (*client.LokiClient)(nil).Configured(), false},
		{"loki empty", loki.Configured(), false},
		{"loki set", setLoki.Configured(), true},
		{"media-agent nil", (*client.MediaAgentClient)(nil).Configured(), false},
		{"media-agent empty", client.NewMediaAgent("", "k").Configured(), false},
		{"media-agent set", client.NewMediaAgent("http://m", "k").Configured(), true},
		{"seerr nil", (*client.SeerrClient)(nil).Configured(), false},
		{"seerr empty", client.NewSeerr("", "k").Configured(), false},
		{"seerr set", client.NewSeerr("http://s", "k").Configured(), true},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: Configured() = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

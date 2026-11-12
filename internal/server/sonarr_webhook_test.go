package server_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/server"
)

func sonarrFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sonarr", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func postSonarr(t *testing.T, ts *httptest.Server, body []byte, auth func(*http.Request)) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/ingest/sonarr",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != nil {
		auth(req)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func bearer(secret string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func basic(user, secret string) func(*http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(user, secret) }
}

func TestE1_AuthMissingSecret503(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	if code := postSonarr(t, ts, sonarrFixture(t, "test"), bearer("")); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
}

func TestE1_AuthWrongSecret401(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	srv.SetSonarrWebhookSecret("hook")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, auth := range []func(*http.Request){nil, bearer("nope"), basic("sonarr", "nope"), bearer(testWebhookSecret)} {
		if code := postSonarr(t, ts, sonarrFixture(t, "test"), auth); code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", code)
		}
	}
}

func TestE1_AuthBasicAndBearerAccepted(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	srv.SetSonarrWebhookSecret("hook")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, auth := range []func(*http.Request){bearer("hook"), basic("sonarr", "hook"), basic("", "hook")} {
		if code := postSonarr(t, ts, sonarrFixture(t, "test"), auth); code != http.StatusNoContent {
			t.Errorf("status %d, want 204", code)
		}
	}
}

func TestE1_ParsePayloadFromFixture(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fixture, kind string
		episodes      [][2]int
	}{
		{"grab", "grab", [][2]int{{2, 3}}},
		{"import", "import", [][2]int{{2, 3}}},
		{"import_complete", "import", [][2]int{{2, 3}, {2, 4}}},
		{"manual_interaction", "manual-interaction", [][2]int{{2, 3}}},
		{"test", "test", [][2]int{{1, 1}}},
		{"health", "other", nil},
	}
	for _, tc := range cases {
		ev, err := server.ParseSonarrForTest(sonarrFixture(t, tc.fixture))
		if err != nil {
			t.Fatalf("%s: %v", tc.fixture, err)
		}
		if ev.Kind != tc.kind || !slices.Equal(ev.Episodes, tc.episodes) {
			t.Errorf("%s: %+v, want kind %s episodes %v", tc.fixture, ev, tc.kind, tc.episodes)
		}
		if tc.kind != "other" && tc.kind != "test" && (ev.Title != "Andor" || ev.DownloadID != "ABCDEF0123456789") {
			t.Errorf("%s: title %q download %q", tc.fixture, ev.Title, ev.DownloadID)
		}
	}
	if _, err := server.ParseSonarrForTest([]byte(`{"eventType":`)); err == nil {
		t.Error("a truncated body parsed")
	}
}

func TestE1_EachRoutingPathWakesItsConsumer(t *testing.T) {
	t.Parallel()
	want := map[string][]string{
		"grab":               {"grab-wait"},
		"import":             {"download", "grab-wait"},
		"manual-interaction": {"download", "stuck-queue"},
		"test":               nil,
		"other":              nil,
	}
	for kind, consumers := range want {
		if got := server.SonarrConsumersForTest(kind); !slices.Equal(got, consumers) {
			t.Errorf("%s wakes %v, want %v", kind, got, consumers)
		}
	}

	for _, tc := range []struct {
		fixture string
		runs    bool
	}{{"manual_interaction", true}, {"grab", false}} {
		env := newStuckEnv(t)
		env.srv.SetSonarrWebhookSecret("hook")
		ts := httptest.NewServer(env.srv.Handler())
		if code := postSonarr(t, ts, sonarrFixture(t, tc.fixture), bearer("hook")); code != http.StatusNoContent {
			t.Fatalf("%s: status %d, want 204", tc.fixture, code)
		}
		ts.Close()
		env.sch.Wait()
		if ran := !env.sch.StartedAt().IsZero(); ran != tc.runs {
			t.Errorf("%s: stuck queue ran %v, want %v", tc.fixture, ran, tc.runs)
		}
	}
}

func TestE1_StuckQueueWakeWithoutScheduler(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	srv.SetSonarrWebhookSecret("hook")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, f := range []string{"grab", "import", "manual_interaction"} {
		if code := postSonarr(t, ts, sonarrFixture(t, f), bearer("hook")); code != http.StatusNoContent {
			t.Errorf("%s: status %d, want 204", f, code)
		}
	}
}

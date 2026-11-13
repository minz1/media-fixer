package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

const contractDir = "testdata/decypharr"

func contractFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractDir, name))
	if err != nil {
		t.Fatalf("contract fixture: %v", err)
	}
	return b
}

func requireKeys(t *testing.T, fixture string, obj map[string]json.RawMessage, keys ...string) {
	t.Helper()
	for _, k := range keys {
		v, ok := obj[k]
		if !ok || string(v) == "null" {
			t.Errorf("%s: contract field %q missing", fixture, k)
		}
	}
}

func TestDecypharrContract_FixturesCarryEveryReliedField(t *testing.T) {
	t.Parallel()

	var list map[string]json.RawMessage
	if err := json.Unmarshal(contractFixture(t, "torrents.json"), &list); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "torrents.json", list, "torrents", "total")
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(list["torrents"], &entries); err != nil || len(entries) == 0 {
		t.Fatalf("torrents.json: torrents must be a non-empty array: %v", err)
	}
	requireKeys(t, "torrents.json entry", entries[0],
		"name", "info_hash", "category", "state", "size", "progress", "added_on", "bad", "active_provider", "magnet")

	var run map[string]json.RawMessage
	if err := json.Unmarshal(contractFixture(t, "repair_run.json"), &run); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "repair_run.json", run, "run_id")

	var status map[string]json.RawMessage
	if err := json.Unmarshal(contractFixture(t, "repair_status_running.json"), &status); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "repair_status_running.json", status, "active_run")
	var active map[string]json.RawMessage
	if err := json.Unmarshal(status["active_run"], &active); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "repair_status_running.json active_run", active, "id", "stage")

	var health []map[string]json.RawMessage
	if err := json.Unmarshal(contractFixture(t, "repair_health.json"), &health); err != nil || len(health) == 0 {
		t.Fatalf("repair_health.json must be a non-empty array: %v", err)
	}
	requireKeys(t, "repair_health.json entry", health[0], "entry_name", "status")
}

type contractCall struct {
	method, path, query string
	body                map[string]any
}

func contractServer(t *testing.T, calls chan<- contractCall) *httptest.Server {
	t.Helper()
	serve := map[string]string{
		"GET /api/torrents":      "torrents.json",
		"POST /api/repair/run":   "repair_run.json",
		"GET /api/repair/status": "repair_status_running.json",
		"GET /api/repair/health": "repair_health.json",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s %s: want Bearer auth", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls <- contractCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: body}
		if name, ok := serve[r.Method+" "+r.URL.Path]; ok {
			_, _ = w.Write(contractFixture(t, name))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDecypharrContract_ClientReadsTheRecordedShapes(t *testing.T) {
	t.Parallel()
	calls := make(chan contractCall, 16)
	c := client.NewDecypharr(contractServer(t, calls).URL, "tok")
	ctx := context.Background()

	torrents, err := c.ListTorrents(ctx, "", "")
	if err != nil || len(torrents) == 0 {
		t.Fatalf("ListTorrents: %v, %d entries", err, len(torrents))
	}
	e := torrents[0]
	if e.Name == "" || e.InfoHash == "" || e.Category == "" || e.State == "" || e.Size == 0 ||
		e.AddedOn.IsZero() || e.ActiveProvider == "" || e.Magnet == "" {
		t.Errorf("entry decoded with a field missing: %+v", e)
	}
	found, err := c.TorrentByHash(ctx, e.InfoHash)
	if err != nil || !found {
		t.Errorf("TorrentByHash(%s) = %v, %v", e.InfoHash, found, err)
	}

	refreshID, err := c.RefreshLinks(ctx)
	if err != nil || refreshID == "" {
		t.Errorf("RefreshLinks: %q, %v", refreshID, err)
	}
	sweepID, err := c.RunRepairSweep(ctx)
	if err != nil || sweepID == "" {
		t.Errorf("RunRepairSweep: %q, %v", sweepID, err)
	}
	_, statusErr := c.RepairStatus(ctx)
	_, healthErr := c.RepairHealth(ctx)
	cleanupErr := c.MountCacheCleanup(ctx)
	recheckErr := c.RecheckEntry(ctx, "Show S01", true)
	deleteErr := c.DeleteTorrent(ctx, "sonarr", "abc", true)
	for _, callErr := range []error{statusErr, healthErr, cleanupErr, recheckErr, deleteErr} {
		if callErr != nil {
			t.Error(callErr)
		}
	}
	close(calls)

	var got []contractCall
	for call := range calls {
		got = append(got, call)
	}
	sweepBoth := map[string]any{"auto_repair": true, "unrestrict_link": true}
	want := []contractCall{
		{method: http.MethodGet, path: "/api/torrents", query: "limit=100"},
		{method: http.MethodGet, path: "/api/torrents", query: "limit=100&page=1&search=" + e.InfoHash},
		{method: http.MethodPost, path: "/api/repair/run", body: sweepBoth},
		{method: http.MethodPost, path: "/api/repair/run", body: map[string]any{"auto_repair": true}},
		{method: http.MethodGet, path: "/api/repair/status"},
		{method: http.MethodGet, path: "/api/repair/health"},
		{method: http.MethodPost, path: "/api/mount/cache/cleanup"},
		{method: http.MethodPost, path: "/api/repair/health/Show S01/check", query: "fix=true"},
		{method: http.MethodDelete, path: "/api/torrents/sonarr/abc", query: "removeFromDebrid=true"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calls differ from the contract:\ngot  %+v\nwant %+v", got, want)
	}
}

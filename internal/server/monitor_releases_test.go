package server_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const (
	releasesPath = "/media/monitoring/7/releases"
	grabPath     = "/media/monitoring/7/grab"
	rQuality     = `{"quality":{"name":"WEBDL-1080p"},"revision":{"version":1}}`
	rLanguages   = `[{"id":1,"name":"English"}]`
)

func release(guid, title string, mapped int, rejections ...string) map[string]any {
	m := make([]map[string]any, mapped)
	for i := range m {
		m[i] = map[string]any{"id": i + 1}
	}
	return map[string]any{
		"guid": guid, "title": title, "indexerId": 3, "indexer": "Nyaa", "size": 2 << 30, "fullSeason": false,
		"rejections": rejections, "mappedEpisodeInfo": m,
		"quality": json.RawMessage(rQuality), "languages": json.RawMessage(rLanguages),
	}
}

func TestMonitorReleases_Table(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.son.setReleases(release("g1", "Seven.S01E01", 1, "Not an upgrade"), release("g2", "Seven.S01.Batch", 0))
	rec := do(t, e.srv, http.MethodPost, releasesPath, url.Values{"season": {"1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	mustContain(t, body,
		`hx-post="/media/monitoring/7/grab"`, `hx-target="#monitor-show"`, `name="guid" value="g1"`,
		`name="guid" value="g2"`, "Seven.S01E01", "Nyaa", "Not an upgrade", "btn btn-danger", "overflow-x-auto",
		"Sonarr rejects it: Not an upgrade.",
		"it will be queued for the 2 missing aired episodes of season 1.",
		`hx-disable="find button"`,
	)
	if strings.Count(body, `name="override"`) != 1 || strings.Count(body, `name="quality"`) != 1 ||
		strings.Count(body, `name="languages"`) != 1 {
		t.Errorf("override fields only belong on the unmapped release:\n%s", body)
	}
}

func TestMonitorReleases_Empty(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	rec := do(t, e.srv, http.MethodPost, releasesPath, url.Values{"season": {"1"}})
	mustContain(t, rec.Body.String(), "No releases found for season 1.")
}

func TestMonitorShow_FindButton(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	rec := do(t, e.srv, http.MethodGet, "/media/monitoring/7", nil)
	mustContain(t, rec.Body.String(),
		`hx-post="/media/monitoring/7/releases"`, `name="season" value="1"`, "btn btn-secondary", "Find releases",
		"Search every indexer once for Seven season 1? This doesn't count against the sweep budget.",
		`id="releases-1"`,
	)
}

func TestMonitorReleases_UnknownSeries(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	for _, p := range []string{"/media/monitoring/99/releases", "/media/monitoring/99/grab"} {
		if rec := do(t, e.srv, http.MethodPost, p, url.Values{"season": {"1"}}); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	if len(e.son.grabPosts()) != 0 {
		t.Error("a grab was sent for an unknown series")
	}
}

func TestMonitorGrab_Notice(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	rec := do(t, e.srv, http.MethodPost, grabPath, url.Values{
		"season": {"1"}, "guid": {"g1"}, "indexer_id": {"3"}, "title": {"Seven.S01E01"},
	})
	mustContain(t, rec.Body.String(), "Grabbed. Once it finishes downloading", `id="monitor-show"`)
	if g := e.son.grabPosts(); len(g) != 1 || !strings.Contains(g[0], `"guid":"g1"`) {
		t.Fatalf("grabs = %v, want one for g1", g)
	}
}

func TestMonitorGrab_OverrideSendsMissing(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	do(t, e.srv, http.MethodPost, grabPath, url.Values{
		"season": {"1"}, "guid": {"g2"}, "indexer_id": {"3"}, "override": {"1"},
		"quality": {rQuality}, "languages": {rLanguages},
	})
	g := e.son.grabPosts()
	if len(g) != 1 || !strings.Contains(g[0], `"episodeIds":[1,3]`) ||
		!strings.Contains(g[0], `"shouldOverride":true`) {
		t.Fatalf("grabs = %v, want an override for episodes 1 and 3", g)
	}
}

func TestMonitorGrab_ExpiredNotice(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.son.failGrab(http.StatusNotFound, "Couldn't find requested release in cache, try searching again")
	rec := do(t, e.srv, http.MethodPost, grabPath, url.Values{"season": {"1"}, "guid": {"g1"}, "indexer_id": {"3"}})
	mustContain(t, rec.Body.String(), "the search expired; find releases again", "notice-err")
}

func TestMonitorGrab_BadReleaseRefused(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	for _, v := range []url.Values{
		{"season": {"1"}, "guid": {""}, "indexer_id": {"3"}},
		{"season": {"1"}, "guid": {"g1"}, "indexer_id": {"x"}},
		{"season": {"1"}, "guid": {"g1"}, "indexer_id": {"0"}},
	} {
		rec := do(t, e.srv, http.MethodPost, grabPath, v)
		mustContain(t, rec.Body.String(), "invalid release", "notice-err")
	}
	if g := e.son.grabPosts(); len(g) != 0 {
		t.Fatalf("grabs = %v, want none", g)
	}
}

func TestMonitorReleases_BadSeason(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	for _, p := range []string{releasesPath, grabPath} {
		rec := do(t, e.srv, http.MethodPost, p, url.Values{"season": {"abc"}, "guid": {"g"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d, want 200", p, rec.Code)
		}
		mustContain(t, rec.Body.String(), "invalid season", "notice-err")
	}
	if e.son.releaseGets() != 0 || len(e.son.grabPosts()) != 0 {
		t.Error("a bad season reached Sonarr's release API")
	}
}

func TestMonitorReleases_CrossOrigin(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	hdr := map[string]string{"Origin": "https://evil.example"}
	for _, p := range []string{releasesPath, grabPath} {
		rec := doWith(t, e.srv, http.MethodPost, p, url.Values{"season": {"1"}, "guid": {"g"}}, hdr)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", p, rec.Code)
		}
	}
	if e.son.releaseGets() != 0 || len(e.son.grabPosts()) != 0 {
		t.Error("a cross-origin request did work")
	}
}

package server_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/server"
)

const (
	proposePath = "/media/monitoring/7/import/propose"
	importPath  = "/media/monitoring/7/import"
)

var fpRe = regexp.MustCompile(`name="targets_fp" value="([^"]+)"`)

func (e *bEnv) importSetup() {
	e.son.setQueue(liveRec(1, 7, "d1", "Seven.S01", "no files found"))
	e.son.setCandidates(
		candidate("/dl/Seven.S01/a.mkv"),
		candidate("/dl/Seven.S01/b.mkv", "Not an upgrade"),
		candidate("/dl/Seven.S01/sample.mkv"),
	)
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1]},{"file":"f2","episode_ids":[3]},` +
		`{"file":"f3","skip":"sample"}]}`
}

func propose(t *testing.T, e *bEnv) string {
	t.Helper()
	rec := do(t, e.srv, http.MethodPost, proposePath, url.Values{"download_id": {"d1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestMonitorPropose_RendersForm(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	body := propose(t, e)
	mustContain(t, body,
		`hx-post="/media/monitoring/7/import"`, `hx-target="#monitor-show"`, `name="download_id" value="d1"`,
		`name="row" value="1|/dl/Seven.S01/a.mkv"`, `name="row" value="3|/dl/Seven.S01/b.mkv"`,
		"Not an upgrade", "sample", "btn btn-danger", "Import ticked files",
		"Import the ticked files of Seven.S01 into Seven? Sonarr moves them into the library.",
		"overflow-x-auto", `hx-disable="find button"`,
	)
	if !fpRe.MatchString(body) {
		t.Errorf("no targets_fp field:\n%s", body)
	}
	if strings.Contains(body, `|/dl/Seven.S01/sample.mkv`) {
		t.Error("a skipped row has a checkbox")
	}
	if !strings.Contains(body, `value="1|/dl/Seven.S01/a.mkv" checked`) ||
		strings.Contains(body, `value="3|/dl/Seven.S01/b.mkv" checked`) {
		t.Errorf("only the unrejected row is ticked:\n%s", body)
	}
}

func TestMonitorPropose_EscapesUntrustedText(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	e.son.setCandidates(candidate("/dl/Seven.S01/<script>x</script>.mkv", hostile))
	e.llm.reply = `{"files":[{"file":"f1","skip":"<b>no</b>"}]}`
	body := propose(t, e)
	if strings.Contains(body, "<script>x") || strings.Contains(body, "<b>no") ||
		strings.Contains(body, "<script>alert") {
		t.Errorf("untrusted text unescaped:\n%s", body)
	}
}

func TestMonitorPropose_Error(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	e.son.setCandidates()
	body := propose(t, e)
	mustContain(t, body, "notice-err", "Sonarr found no files to import")
	if strings.Contains(body, "Import ticked files") {
		t.Error("an error answer carries the form")
	}
}

func TestMonitorPropose_NotStuck(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	e.son.setQueue()
	mustContain(t, propose(t, e), "notice-err")
	if e.llm.count() != 0 {
		t.Error("the agent was called for a download that isn't stuck")
	}
}

func TestMonitorPropose_BadSeriesID(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	before := e.son.requests()
	for _, p := range []string{"/media/monitoring/abc/import/propose", "/media/monitoring/0/import/propose"} {
		if rec := do(t, e.srv, http.MethodPost, p, url.Values{"download_id": {"d1"}}); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	if e.son.requests() != before {
		t.Error("Sonarr was hit for a bad id")
	}
}

func TestMonitorPropose_UnknownSeries(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	for _, p := range []string{"/media/monitoring/99/import/propose", "/media/monitoring/99/import"} {
		if rec := do(t, e.srv, http.MethodPost, p, url.Values{"download_id": {"d1"}}); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	if e.llm.count() != 0 {
		t.Error("the agent was called for an unknown show")
	}
}

func TestMonitorPropose_CrossOrigin(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	hdr := map[string]string{"Origin": "https://evil.example"}
	for _, p := range []string{proposePath, importPath} {
		rec := doWith(t, e.srv, http.MethodPost, p, url.Values{"download_id": {"d1"}}, hdr)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", p, rec.Code)
		}
	}
	if e.llm.count() != 0 || len(e.son.commandPosts()) != 0 {
		t.Error("a cross-origin request did work")
	}
}

func TestMonitorImport_PostsMapping(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	fp := fpRe.FindStringSubmatch(propose(t, e))[1]
	rec := do(t, e.srv, http.MethodPost, importPath, url.Values{
		"download_id": {"d1"}, "targets_fp": {fp},
		"row": {"1|/dl/Seven.S01/a.mkv", "3|/dl/Seven.S01/b.mkv"},
	})
	mustContain(t, rec.Body.String(), "Import started for 2 files")
	posts := e.son.commandPosts()
	if len(posts) != 1 || !strings.Contains(posts[0], "ManualImport") ||
		!strings.Contains(posts[0], "/dl/Seven.S01/a.mkv") || !strings.Contains(posts[0], "/dl/Seven.S01/b.mkv") {
		t.Errorf("command posts = %v", posts)
	}
}

func TestMonitorImport_NoRows(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	fp := fpRe.FindStringSubmatch(propose(t, e))[1]
	rec := do(t, e.srv, http.MethodPost, importPath, url.Values{"download_id": {"d1"}, "targets_fp": {fp}})
	mustContain(t, rec.Body.String(), "Tick at least one file.")
	if len(e.son.commandPosts()) != 0 {
		t.Error("Sonarr was told to import nothing")
	}
}

func TestMonitorImport_BadRow(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	fp := fpRe.FindStringSubmatch(propose(t, e))[1]
	for _, row := range []string{"nopipe", "x|/dl/a.mkv", "1,|/dl/a.mkv", "0|/dl/a.mkv", "|/dl/a.mkv"} {
		rec := do(t, e.srv, http.MethodPost, importPath, url.Values{
			"download_id": {"d1"}, "targets_fp": {fp}, "row": {row},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: %d", row, rec.Code)
		}
		mustContain(t, rec.Body.String(), "invalid row")
	}
	if len(e.son.commandPosts()) != 0 {
		t.Error("a bad row reached Sonarr")
	}
}

func TestMonitorImport_Refused(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	rec := do(t, e.srv, http.MethodPost, importPath, url.Values{
		"download_id": {"d1"}, "targets_fp": {"stale"}, "row": {"1|/dl/Seven.S01/a.mkv"},
	})
	mustContain(t, rec.Body.String(), "this download changed; propose again")
	if len(e.son.commandPosts()) != 0 {
		t.Error("a stale mapping reached Sonarr")
	}
}

func TestMonitorImport_StuckQueueMissing(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	e.srv.SetStuckQueue(nil)
	rec := do(t, e.srv, http.MethodPost, importPath, url.Values{
		"download_id": {"d1"}, "targets_fp": {"x"}, "row": {"1|/dl/a.mkv"},
	})
	mustContain(t, rec.Body.String(), "The stuck queue isn&#39;t running.")
}

func TestParseRows(t *testing.T) {
	t.Parallel()
	rows, err := server.ParseRows([]string{"1,2|/dl/a|b.mkv", "3|/dl/c.mkv"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, err = %v", rows, err)
	}
	if rows[0].Path != "/dl/a|b.mkv" || len(rows[0].EpisodeIDs) != 2 || rows[0].EpisodeIDs[1] != 2 {
		t.Errorf("row 0 = %+v", rows[0])
	}
}

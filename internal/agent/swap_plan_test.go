package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
	"github.com/minz1/mediafixer/internal/operation"
)

const (
	swapRoot    = "/data/downloads/sonarr"
	swapRelease = "Show.S01E06.AnoZu"
	swapOldPath = "/data/tv/Show/Show.S01E06.BiOMA.mkv"
	swapNewPath = swapRoot + "/" + swapRelease + "/Show.S01E06.AnoZu.mkv"
)

type swapWorld struct {
	mu       sync.Mutex
	hits     int
	writes   []string
	files    []client.EpisodeFile
	epFileID int
	folder   []map[string]any
	history  []map[string]any
	probe    mediaagentapi.ProbeResult
	jfPath   string
}

func (w *swapWorld) candidate() map[string]any {
	return map[string]any{
		"path":              swapNewPath,
		"size":              2000,
		"releaseGroup":      "AnoZu",
		"seriesId":          5,
		"seasonNumber":      1,
		"episodes":          []map[string]any{{"id": 66, "seasonNumber": 1, "episodeNumber": 6}},
		"quality":           map[string]any{"quality": map[string]any{"name": "WEBDL-1080p"}},
		"customFormatScore": 120,
		"rejections": []map[string]any{
			{"reason": "Not a quality revision upgrade for existing episode file(s)"},
		},
	}
}

func newSwapWorld() *swapWorld {
	w := &swapWorld{epFileID: 9, jfPath: "/jf/tv/Show/Show.S01E06.BiOMA.mkv"}
	w.files = []client.EpisodeFile{{ID: 9, SeriesID: 5, Path: swapOldPath, Size: 1000, ReleaseGroup: "BiOMA"}}
	w.folder = []map[string]any{w.candidate()}
	old := time.Now().Add(-100 * time.Hour).UTC().Format(time.RFC3339)
	older := time.Now().Add(-300 * time.Hour).UTC().Format(time.RFC3339)
	w.history = []map[string]any{
		{"id": 100, "episodeId": 66, "seriesId": 5, "eventType": "grabbed", "downloadId": "AAAA",
			"sourceTitle": "Show.S01E06.BiOMA", "date": older},
		{"id": 101, "episodeId": 66, "seriesId": 5, "eventType": "downloadFolderImported", "downloadId": "AAAA",
			"sourceTitle": "Show.S01E06.BiOMA", "date": older, "data": map[string]any{"importedPath": swapOldPath}},
		{"id": 102, "episodeId": 66, "seriesId": 5, "eventType": "grabbed", "downloadId": "BBBB",
			"sourceTitle": swapRelease, "date": old},
	}
	w.probe = mediaagentapi.ProbeResult{Streams: []mediaagentapi.ProbeStream{
		{Index: 0, CodecType: "video", CodecName: "h264"},
		{Index: 1, CodecType: "subtitle", CodecName: "ass", Language: "eng", Title: "Full"},
		{Index: 2, CodecType: "subtitle", CodecName: "subrip", Language: "por"},
	}}
	return w
}

func (w *swapWorld) dispatcher(t *testing.T) *agent.Dispatcher {
	t.Helper()
	reply := func(rw http.ResponseWriter, v any) { _ = json.NewEncoder(rw).Encode(v) }
	sonarr := http.NewServeMux()
	sonarr.HandleFunc("/api/v3/series", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, []client.Series{{ID: 5, Title: "Show"}})
	})
	sonarr.HandleFunc("/api/v3/episode", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, []client.Episode{
			{
				ID:            66,
				SeriesID:      5,
				Title:         "Six",
				SeasonNumber:  1,
				EpisodeNumber: 6,
				EpisodeFileID: w.epFileID,
				HasFile:       w.epFileID > 0,
			},
			{ID: 67, SeriesID: 5, SeasonNumber: 1, EpisodeNumber: 7, EpisodeFileID: 10, HasFile: true},
		})
	})
	sonarr.HandleFunc("/api/v3/episodefile", func(rw http.ResponseWriter, _ *http.Request) { reply(rw, w.files) })
	sonarr.HandleFunc("/api/v3/history/series", func(rw http.ResponseWriter, _ *http.Request) { reply(rw, w.history) })
	sonarr.HandleFunc("/api/v3/manualimport", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("seriesId") != "" || r.URL.Query().Get("folder") != swapRoot+"/"+swapRelease {
			http.Error(rw, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		reply(rw, w.folder)
	})
	sonarr.HandleFunc("/api/v3/queue", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, map[string]any{"records": []any{}})
	})
	sonarr.HandleFunc("/api/v3/config/mediamanagement", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, map[string]string{"recycleBin": "/data/recycle"})
	})
	sonarr.HandleFunc("/api/v3/config/downloadclient", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, client.DownloadClientConfig{AutoRedownloadFailed: true})
	})
	jf := http.NewServeMux()
	jf.HandleFunc(
		"/Users",
		func(rw http.ResponseWriter, _ *http.Request) { reply(rw, []map[string]string{{"Id": "admin"}}) },
	)
	jf.HandleFunc("/Items", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, client.ItemsResponse{Items: []client.JellyfinItem{{ID: "jf-series", Name: "Show", Type: "Series"}}})
	})
	jf.HandleFunc("/Shows/jf-series/Episodes", func(rw http.ResponseWriter, _ *http.Request) {
		one, six := 1, 6
		reply(rw, client.ItemsResponse{Items: []client.JellyfinItem{
			{ID: "jf-ep6", Type: "Episode", Path: w.jfPath, IndexNumber: &six, ParentIndexNumber: &one},
		}})
	})
	ma := http.NewServeMux()
	ma.HandleFunc("/probe", func(rw http.ResponseWriter, _ *http.Request) { reply(rw, w.probe) })
	serve := func(h http.Handler) string {
		s := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			w.mu.Lock()
			w.hits++
			if r.Method != http.MethodGet && r.URL.Path != "/probe" {
				w.writes = append(w.writes, r.Method+" "+r.URL.Path)
			}
			w.mu.Unlock()
			h.ServeHTTP(rw, r)
		}))
		t.Cleanup(s.Close)
		return s.URL
	}
	return &agent.Dispatcher{
		Sonarr:     client.NewArr(serve(sonarr), "k"),
		Jellyfin:   client.NewJellyfin(serve(jf), "k"),
		MediaAgent: client.NewMediaAgent(serve(ma), ""),
	}
}

func swapArgs(over map[string]any) map[string]any {
	a := map[string]any{"title": "Show", "season": 1, "episode": 6, "release": swapRelease, "language": "en"}
	for k, v := range over {
		if v == nil {
			delete(a, k)
			continue
		}
		a[k] = v
	}
	return a
}

func TestD4_PlanSwap(t *testing.T) {
	t.Parallel()
	unknownCodec := func(w *swapWorld) {
		w.probe.Streams[2].CodecName = ""
	}
	cases := []struct {
		name  string
		what  string
		args  map[string]any
		setup func(*swapWorld)
		want  string
		http  bool
	}{
		{name: "happy path", args: swapArgs(nil), http: true},
		{name: "no file on the episode", args: swapArgs(nil), http: true, want: "nothing to swap",
			setup: func(w *swapWorld) { w.epFileID = 0 }},
		{
			name:  "folder empty",
			args:  swapArgs(nil),
			http:  true,
			want:  "not cached under " + swapRoot + "/" + swapRelease,
			setup: func(w *swapWorld) { w.folder = nil },
		},
		{name: "guard refusal shows the parse", args: swapArgs(nil), http: true, want: "S01E07",
			setup: func(w *swapWorld) {
				c := w.candidate()
				c["episodes"] = []map[string]any{{"id": 67, "seasonNumber": 1, "episodeNumber": 7}}
				w.folder = []map[string]any{c}
			}},
		{name: "unknown subtitle codec", args: swapArgs(nil), http: true, want: "unknown", setup: unknownCodec},
		{name: "wanted language missing", args: swapArgs(map[string]any{"language": "fr"}), http: true,
			want: "no fre subtitle"},
		{name: "no language", args: swapArgs(map[string]any{"language": nil}), want: "language"},
		{name: "jellyfin base name differs", args: swapArgs(nil), http: true, want: "disagree",
			setup: func(w *swapWorld) { w.jfPath = "/jf/tv/Show/other.mkv" }},
		{name: "stuck_read refused", what: agent.WhatStuckRead, args: swapArgs(nil), want: "stuck_read"},
		{name: "release with slash", args: swapArgs(map[string]any{"release": "a/b"}), want: "release"},
		{name: "release dotdot", args: swapArgs(map[string]any{"release": ".."}), want: "release"},
		{name: "release traversal", args: swapArgs(map[string]any{"release": "x..y"}), want: "release"},
		{name: "release NUL", args: swapArgs(map[string]any{"release": "a\x00b"}), want: "release"},
		{name: "release empty", args: swapArgs(map[string]any{"release": ""}), want: "release"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runSwapCase(t, tc.what, tc.args, tc.setup, tc.want, tc.http)
		})
	}
}

func runSwapCase(t *testing.T, what string, args map[string]any, setup func(*swapWorld), want string, http bool) {
	t.Helper()
	w := newSwapWorld()
	if setup != nil {
		setup(w)
	}
	if what == "" {
		what = agent.WhatSubtitles
	}
	res, err := w.dispatcher(t).CallScoped(context.Background(), agent.Scope{What: what}, "sonarr_swap_plan", args)
	if len(w.writes) != 0 {
		t.Errorf("write calls: %v", w.writes)
	}
	if !http && w.hits != 0 {
		t.Errorf("%d HTTP calls before validation", w.hits)
	}
	if want != "" {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	assertSwapPlan(t, res)
}

func assertSwapPlan(t *testing.T, res any) {
	t.Helper()
	p, ok := res.(*client.SwapPlan)
	if !ok {
		t.Fatalf("result %T", res)
	}
	if p.What != agent.WhatSubtitles || p.Language != "eng" || p.Title != "Show" || p.SeriesID != 5 ||
		p.Season != 1 || p.Episode != 6 || p.EpisodeID != 66 || p.EpisodeTitle != "Six" {
		t.Errorf("header = %+v", p)
	}
	assertSwapFiles(t, p)
	assertSwapRest(t, p)
}

func assertSwapFiles(t *testing.T, p *client.SwapPlan) {
	t.Helper()
	if p.Current.Path != swapOldPath || p.Current.Size != 1000 || p.Current.ReleaseGroup != "BiOMA" ||
		p.Current.FileID != 9 || p.Current.SourceTitle != "Show.S01E06.BiOMA" || p.Current.DownloadID != "AAAA" ||
		p.Current.PackSize != 1 || p.Current.GrabbedAt.IsZero() {
		t.Errorf("current = %+v", p.Current)
	}
	c := p.Candidate
	if c.Folder != swapRoot+"/"+swapRelease || c.Path != swapNewPath || c.Size != 2000 || c.ReleaseGroup != "AnoZu" ||
		c.Quality != "WEBDL-1080p" || c.CustomFormatScore != 120 || c.GraceHours != 168 || c.DownloadID != "BBBB" ||
		c.GrabAgeHours < 99 || c.GrabAgeHours > 101 || len(c.Rejections) != 1 ||
		c.Rejections[0] != "Not a quality revision upgrade for existing episode file(s)" {
		t.Errorf("candidate = %+v", c)
	}
	if len(p.Mapping) != 1 || p.Mapping[0].ID != 66 || p.GuardVerdict != "ok" {
		t.Errorf("mapping = %+v verdict %q", p.Mapping, p.GuardVerdict)
	}
	if len(p.Probe.Subtitles) != 2 || p.Probe.Wanted.Index != 1 || p.Probe.Wanted.Codec != "ass" {
		t.Errorf("probe = %+v", p.Probe)
	}
}

func assertSwapRest(t *testing.T, p *client.SwapPlan) {
	t.Helper()
	if p.Jellyfin.SeriesID != "jf-series" || p.Jellyfin.ItemID != "jf-ep6" || p.Jellyfin.Path == "" {
		t.Errorf("jellyfin = %+v", p.Jellyfin)
	}
	if p.Blocklist.Skip || p.Blocklist.HistoryID != 100 || p.Blocklist.DownloadID != "AAAA" {
		t.Errorf("blocklist = %+v", p.Blocklist)
	}
	if p.RecycleBin != "/data/recycle" || !p.AutoRedownloadOn {
		t.Errorf("recycle %q auto %v", p.RecycleBin, p.AutoRedownloadOn)
	}
	if len(p.Steps) != 3 || p.Steps[0].Kind != operation.StepImport || p.Steps[0].Path != swapNewPath ||
		p.Steps[0].FileID != 9 || p.Steps[0].Size != 2000 || p.Steps[0].MediaID != 5 || p.Steps[0].Season != 1 ||
		p.Steps[0].Episode != 6 || len(p.Steps[0].EpisodeIDs) != 1 || p.Steps[0].EpisodeIDs[0] != 66 ||
		p.Steps[1].Kind != operation.StepJellyfinNotify || p.Steps[1].Path != p.Jellyfin.Path ||
		p.Steps[1].Name != "jf-series" || p.Steps[2].Kind != operation.StepBlocklist || p.Steps[2].HistoryID != 100 {
		t.Errorf("steps = %+v", p.Steps)
	}
}

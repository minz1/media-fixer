package babysit_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/babysit"
	"github.com/minz1/mediafixer/internal/client"
)

const (
	qualityJSON   = `{"quality":{"name":"WEBDL-1080p"},"revision":{"version":1}}`
	languagesJSON = `[{"id":1,"name":"English"}]`
)

func relRec(guid, title string, full bool, mapped int, rejections ...string) map[string]any {
	m := make([]map[string]any, mapped)
	for i := range m {
		m[i] = map[string]any{"id": i + 1}
	}
	return map[string]any{
		"guid": guid, "title": title, "indexerId": 3, "indexer": "Nyaa", "fullSeason": full, "size": 1 << 30,
		"rejections": rejections, "mappedEpisodeInfo": m,
		"quality": json.RawMessage(qualityJSON), "languages": json.RawMessage(languagesJSON),
	}
}

func (e *env) releaseShow() {
	e.addSeries(7, "Seven")
	unaired := now0().AddDate(0, 0, 10)
	future := aired(3, 1, 3, false)
	future.AirDateUtc = &unaired
	e.son.episodes[7] = []client.Episode{aired(1, 1, 1, true), aired(2, 1, 2, false), future}
}

func TestFindReleases_Sorted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.releaseShow()
	e.son.releases = []map[string]any{
		relRec("a", "single", false, 1), relRec("b", "pair", false, 2), relRec("c", "full", true, 0),
	}
	f, err := e.svc.FindReleases(context.Background(), 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range f.Releases {
		got = append(got, r.Title)
	}
	if want := []string{"pair", "full", "single"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if len(f.Missing) != 1 || f.Missing[0].ID != 2 || f.Season != 1 {
		t.Fatalf("missing = %+v season %d, want only episode 2 of season 1", f.Missing, f.Season)
	}
}

func TestFindReleases_RejectedListed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.releaseShow()
	e.son.releases = []map[string]any{relRec("a", "bad", false, 1, "Not an upgrade")}
	f, err := e.svc.FindReleases(context.Background(), 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Releases) != 1 || len(f.Releases[0].Rejections) != 1 {
		t.Fatalf("releases = %+v, want the rejected one listed", f.Releases)
	}
}

func TestFindReleases_Running(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.releaseShow()
	e.son.relEnter, e.son.relRelease = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.FindReleases(context.Background(), 7, 1)
		done <- err
	}()
	<-e.son.relEnter
	_, err := e.svc.FindReleases(context.Background(), 7, 2)
	if !errors.Is(err, babysit.ErrFindRunning) {
		t.Fatalf("second call err = %v, want ErrFindRunning", err)
	}
	close(e.son.relRelease)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGrab_Plain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.releaseShow()
	notice, err := e.svc.Grab(
		context.Background(),
		7,
		babysit.GrabRequest{Season: 1, GUID: "g", IndexerID: 3, Title: "t"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notice, "Grabbed.") || !strings.Contains(notice, "Stuck downloads") {
		t.Fatalf("notice = %q", notice)
	}
	if len(e.son.grabs) != 1 || len(e.son.grabs[0]) != 2 || e.son.grabs[0]["guid"] != "g" {
		t.Fatalf("grabs = %+v, want only guid and indexerId", e.son.grabs)
	}
}

func TestGrab_OverrideSendsOnlyMissing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.releaseShow()
	g := babysit.GrabRequest{
		Season: 1, GUID: "g", IndexerID: 3, Override: true,
		Quality: json.RawMessage(qualityJSON), Languages: json.RawMessage(languagesJSON),
	}
	if _, err := e.svc.Grab(context.Background(), 7, g); err != nil {
		t.Fatal(err)
	}
	got := e.son.grabs[0]
	ids, _ := got["episodeIds"].([]any)
	if got["shouldOverride"] != true || got["seriesId"] != float64(7) || len(ids) != 1 || ids[0] != float64(2) {
		t.Fatalf("body = %+v, want shouldOverride, seriesId 7 and episodeIds [2] (aired, no file)", got)
	}
}

func TestGrab_OverrideNothingMissing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addSeries(7, "Seven")
	e.son.episodes[7] = []client.Episode{aired(1, 1, 1, true)}
	g := babysit.GrabRequest{
		Season: 1, GUID: "g", Override: true,
		Quality: json.RawMessage(qualityJSON), Languages: json.RawMessage(languagesJSON),
	}
	_, err := e.svc.Grab(context.Background(), 7, g)
	if err == nil || !strings.Contains(err.Error(), "nothing missing in season 1") {
		t.Fatalf("err = %v, want nothing missing in season 1", err)
	}
	if len(e.son.grabs) != 0 {
		t.Fatalf("grabs = %+v, want none", e.son.grabs)
	}
}

func TestGrab_BadQualityShape(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]babysit.GrabRequest{
		"quality array":      {Quality: json.RawMessage(`[1]`), Languages: json.RawMessage(languagesJSON)},
		"languages object":   {Quality: json.RawMessage(qualityJSON), Languages: json.RawMessage(`{}`)},
		"quality not json":   {Quality: json.RawMessage(`{`), Languages: json.RawMessage(languagesJSON)},
		"quality too large":  {Quality: json.RawMessage(`{"a":"` + strings.Repeat("x", 5000) + `"}`), Languages: json.RawMessage(languagesJSON)},
		"languages too long": {Quality: json.RawMessage(qualityJSON), Languages: json.RawMessage(`["` + strings.Repeat("x", 5000) + `"]`)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.releaseShow()
			g.Season, g.GUID, g.Override = 1, "g", true
			_, err := e.svc.Grab(context.Background(), 7, g)
			if err == nil || !strings.Contains(err.Error(), "invalid release data") {
				t.Fatalf("err = %v, want invalid release data", err)
			}
			if len(e.son.grabs) != 0 {
				t.Fatalf("grabs = %+v, want none", e.son.grabs)
			}
		})
	}
}

func TestGrab_Expired(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.releaseShow()
	e.son.grabStatus, e.son.grabBody = 404, "Couldn't find requested release in cache, try searching again"
	_, err := e.svc.Grab(context.Background(), 7, babysit.GrabRequest{Season: 1, GUID: "g"})
	if err == nil || err.Error() != "the search expired; find releases again" {
		t.Fatalf("err = %v, want the expired message", err)
	}
}

func TestGrab_UnparseableIs404Message(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.releaseShow()
	const msg = "Unable to parse episodes in the release, will need to be manually provided"
	e.son.grabStatus, e.son.grabBody = 404, msg
	_, err := e.svc.Grab(context.Background(), 7, babysit.GrabRequest{Season: 1, GUID: "g"})
	if err == nil || !strings.Contains(err.Error(), msg) || strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want Sonarr's own message", err)
	}
}

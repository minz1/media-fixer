package agent_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
)

type fakeLine struct {
	unit string
	at   time.Time
	msg  string
}

type contextLoki struct {
	mu      sync.Mutex
	matches []fakeLine
	stream  []fakeLine
	forward []url.Values
}

var exactUnitSelector = regexp.MustCompile(`^\{unit="([^"]*)"\}$`)

func encodeLokiLines(w http.ResponseWriter, lines []fakeLine) {
	streams := map[string][][]string{}
	var order []string
	for _, l := range lines {
		if _, ok := streams[l.unit]; !ok {
			order = append(order, l.unit)
		}
		streams[l.unit] = append(streams[l.unit], []string{strconv.FormatInt(l.at.UnixNano(), 10), l.msg})
	}
	result := make([]any, 0, len(order))
	for _, u := range order {
		labels := map[string]string{"job": "systemd-journal"}
		if u != "" {
			labels["unit"] = u
		}
		result = append(result, map[string]any{"stream": labels, "values": streams[u]})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"result": result}})
}

func (f *contextLoki) serve(t *testing.T) *client.LokiClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("direction") != "forward" {
			encodeLokiLines(w, f.matches)
			return
		}
		f.mu.Lock()
		f.forward = append(f.forward, q)
		f.mu.Unlock()
		startNS, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		unit := ""
		exact := false
		if m := exactUnitSelector.FindStringSubmatch(q.Get("query")); m != nil {
			unit, exact = m[1], true
		}
		var out []fakeLine
		for _, l := range f.stream {
			if (exact && l.unit != unit) || l.at.UnixNano() < startNS {
				continue
			}
			out = append(out, l)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
		if len(out) > limit {
			out = out[:limit]
		}
		encodeLokiLines(w, out)
	}))
	t.Cleanup(srv.Close)
	loki, err := client.NewLoki(srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return loki
}

func runLokiContext(t *testing.T, f *contextLoki, at time.Time, args map[string]any) *agent.LokiQueryOutput {
	t.Helper()
	disp := &agent.Dispatcher{Loki: f.serve(t)}
	base := map[string]any{"units": `{unit=~"jellyfin.service|decypharr.service"}`, "minutes_back": float64(60)}
	maps.Copy(base, args)
	res, err := disp.CallScoped(context.Background(), agent.Scope{ReportedAt: at}, "loki_query", base)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := res.(*agent.LokiQueryOutput)
	if !ok {
		t.Fatalf("result is %T, want *agent.LokiQueryOutput", res)
	}
	return out
}

func TestG3_ContextAfterReturnsFfmpegBlock(t *testing.T) {
	t.Parallel()
	t0 := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	match := fakeLine{"jellyfin.service", t0, "[ERR] Overgeared S01E06: Subtitle extraction failed " +
		"/var/cache/jellyfin/subtitles/57/57ea761f-1ffe-c2e4-6626-d9556e9b280a/5.vtt"}
	ffmpeg := []string{
		"Unknown/unsupported AVCodecID S_TEXT/WEBVTT.",
		"Could not find codec parameters for stream 5 (Subtitle: none): unknown codec",
		"Consider increasing the value for the 'analyzeduration' and 'probesize' options",
		"Stream mapping:",
		"  Stream #0:5 -> #0:0 (copy)",
		"Subtitle encoding currently only possible from text to text or bitmap to bitmap",
	}
	f := &contextLoki{matches: []fakeLine{match}, stream: []fakeLine{match}}
	for i, m := range ffmpeg {
		f.stream = append(f.stream, fakeLine{"jellyfin.service", t0.Add(time.Duration(i+1) * time.Millisecond), m})
	}
	f.stream = append(f.stream, fakeLine{"decypharr.service", t0.Add(2 * time.Millisecond), "decypharr: read ok"})

	out := runLokiContext(t, f, t0, map[string]any{"filters": []any{"Overgeared"}, "context_after": float64(20)})

	if len(out.Lines) != 1+len(ffmpeg) {
		t.Fatalf("lines = %+v, want the match and %d context lines", out.Lines, len(ffmpeg))
	}
	if out.Lines[0].Message != match.msg || out.Lines[0].Context {
		t.Errorf("first line = %+v, want the match, not context", out.Lines[0])
	}
	for i, m := range ffmpeg {
		l := out.Lines[i+1]
		if l.Message != m || !l.Context {
			t.Errorf("line %d = %+v, want context %q", i+1, l, m)
		}
	}
	if len(f.forward) != 1 {
		t.Fatalf("follow-ups = %d, want 1", len(f.forward))
	}
	q := f.forward[0]
	if q.Get("query") != `{unit="jellyfin.service"}` || q.Get("limit") != "21" {
		t.Errorf(
			"follow-up = query %q limit %q, want {unit=\"jellyfin.service\"} limit 21",
			q.Get("query"),
			q.Get("limit"),
		)
	}
}

func TestG3_ContextAfterCapsAndSkipsOverlap(t *testing.T) {
	t.Parallel()
	t0 := time.Now().Add(-20 * time.Minute).Truncate(time.Second)
	const unit = "jellyfin.service"
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	f := &contextLoki{
		matches: []fakeLine{{unit, at(0), "err a"}, {unit, at(1000), "err b"}, {unit, at(2000), "err c"}},
		stream: []fakeLine{
			{unit, at(0), "err a"}, {unit, at(500), "ctx a1"}, {unit, at(1000), "err b"}, {unit, at(1500), "ctx b1"},
			{unit, at(2000), "err c"}, {unit, at(2500), "ctx c1"},
		},
	}
	out := runLokiContext(t, f, t0, map[string]any{"context_after": float64(3)})
	if len(f.forward) != 2 {
		t.Fatalf("follow-ups = %d, want 2 (the match at t+1s is inside the first window)", len(f.forward))
	}
	seen := map[string]bool{}
	for _, l := range out.Lines {
		if seen[l.Message] {
			t.Errorf("line %q appears twice: %+v", l.Message, out.Lines)
		}
		seen[l.Message] = true
	}
	if !seen["ctx c1"] || !seen["ctx b1"] {
		t.Errorf("context lines missing: %+v", out.Lines)
	}

	spread := &contextLoki{}
	for i := range 8 {
		spread.matches = append(
			spread.matches,
			fakeLine{unit, t0.Add(time.Duration(i) * 10 * time.Second), "err " + strconv.Itoa(i)},
		)
	}
	runLokiContext(t, spread, t0, map[string]any{"context_after": float64(2)})
	if len(spread.forward) != 5 {
		t.Fatalf("follow-ups = %d, want 5", len(spread.forward))
	}
	for _, q := range spread.forward {
		ns, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		if time.Unix(0, ns).Before(t0.Add(30 * time.Second)) {
			t.Errorf("follow-up from %v: not one of the 5 newest matches", time.Unix(0, ns))
		}
	}

	noUnit := &contextLoki{matches: []fakeLine{{"", t0, "err"}}}
	runLokiContext(t, noUnit, t0, map[string]any{"context_after": float64(5)})
	if len(noUnit.forward) != 0 {
		t.Errorf("a line with no unit got %d follow-ups", len(noUnit.forward))
	}

	clamp := &contextLoki{matches: []fakeLine{{unit, t0, "err"}}}
	runLokiContext(t, clamp, t0, map[string]any{"context_after": float64(50)})
	if len(clamp.forward) != 1 || clamp.forward[0].Get("limit") != "31" {
		t.Errorf("context_after 50: follow-ups %v, want one with limit 31", clamp.forward)
	}

	zero := &contextLoki{matches: []fakeLine{{unit, t0, "err"}}}
	runLokiContext(t, zero, t0, map[string]any{"context_after": float64(0)})
	if len(zero.forward) != 0 {
		t.Errorf("context_after 0 ran %d follow-ups", len(zero.forward))
	}
}

func TestG3_LokiSchemaDeclaresContextAfter(t *testing.T) {
	t.Parallel()
	for _, d := range agent.ToolDefsForTest() {
		if d.Function.Name != "loki_query" {
			continue
		}
		raw, err := json.Marshal(d.Function.Parameters)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if err = json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		if p, ok := schema.Properties["context_after"]; !ok || p.Type != "number" {
			t.Errorf("context_after schema = %+v, want a number property", p)
		}
		return
	}
	t.Fatal("loki_query tool not found")
}

func TestG4_LokiResultCarriesItemIDs(t *testing.T) {
	t.Parallel()
	t0 := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond)
	f := &contextLoki{matches: []fakeLine{
		{"jellyfin.service", t0, "extract /cache/subtitles/57/57ea761f-1ffe-c2e4-6626-d9556e9b280a/3.ass failed"},
		{"jellyfin.service", t0.Add(time.Second), "again /cache/subtitles/57/57EA761F1FFEC2E46626D9556E9B280A/3.ass"},
		{"jellyfin.service", t0.Add(2 * time.Second), "no path here /subtitles/zz/nothex/"},
	}}
	out := runLokiContext(t, f, t0, nil)
	want := []string{"57ea761f1ffec2e46626d9556e9b280a"}
	if len(out.ItemIDs) != 1 || out.ItemIDs[0] != want[0] {
		t.Errorf("item_ids = %v, want %v", out.ItemIDs, want)
	}
}

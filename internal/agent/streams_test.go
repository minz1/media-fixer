package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

func fileSubs(codecs map[int]string, to int) []mediaagentapi.ProbeStream {
	out := []mediaagentapi.ProbeStream{
		{Index: 0, CodecType: "video", CodecName: "h264"},
		{Index: 1, CodecType: "audio", CodecName: "aac"},
		{Index: 2, CodecType: "audio", CodecName: "aac"},
	}
	for i := 3; i <= to; i++ {
		codec, ok := codecs[i]
		if !ok {
			codec = "ass"
		}
		out = append(out, mediaagentapi.ProbeStream{Index: i, CodecType: "subtitle", CodecName: codec})
	}
	return out
}

func jfStreams(last int, lang map[int]string) []client.MediaStream {
	out := []client.MediaStream{
		{Index: 0, Type: "Video"},
		{Index: 1, Type: "Audio"},
		{Index: 2, Type: "Audio"},
	}
	for i := 3; i <= last; i++ {
		l := lang[i]
		if l == "" {
			l = "por"
		}
		out = append(out, client.MediaStream{Index: i, Type: "Subtitle", Language: l})
	}
	return out
}

func TestB4_AlignOvergeared(t *testing.T) {
	t.Parallel()
	file := fileSubs(map[int]string{5: "", 7: ""}, 22)
	file[6].Language = "eng"
	jf := jfStreams(20, map[int]string{5: "eng"})
	got := agent.AlignStreamsForTest(file, jf, "eng")
	var s5, s3, s4 agent.AlignedStream
	for _, s := range got.Streams {
		switch s.JellyfinIndex {
		case 3:
			s3 = s
		case 4:
			s4 = s
		case 5:
			s5 = s
		}
	}
	if s5.FileIndex != 6 || s5.FfmpegMapsTo != 5 || s5.FfmpegMapsToCodec != "unknown" || !s5.Drift {
		t.Errorf("jellyfin 5 = %+v", s5)
	}
	if s3.Drift || s4.Drift {
		t.Errorf("jellyfin 3/4 drift: %+v %+v", s3, s4)
	}
	if !got.IndexDrift || !slices.Equal(got.UnknownCodecTracks, []int{5, 7}) || !got.WantedLanguagePresent {
		t.Errorf("alignment = %+v", got)
	}
}

func TestB4_AlignUnknownBeforeGood(t *testing.T) {
	t.Parallel()
	file := []mediaagentapi.ProbeStream{
		{Index: 0, CodecType: "video", CodecName: "h264"},
		{Index: 1, CodecType: "audio", CodecName: "aac"},
		{Index: 2, CodecType: "subtitle"},
		{Index: 3, CodecType: "subtitle", CodecName: "ass", Language: "eng"},
	}
	jf := []client.MediaStream{
		{Index: 0, Type: "Video"},
		{Index: 1, Type: "Audio"},
		{Index: 2, Type: "Subtitle", Language: "eng"},
	}
	got := agent.AlignStreamsForTest(file, jf, "eng")
	s := got.Streams[0]
	if s.FileIndex != 3 || s.FfmpegMapsTo != 2 || !s.Drift {
		t.Errorf("stream = %+v", s)
	}
}

func TestB4_AlignClean(t *testing.T) {
	t.Parallel()
	file := fileSubs(nil, 4)
	jf := append(jfStreams(4, nil), client.MediaStream{Index: 5, Type: "Subtitle", Language: "eng", IsExternal: true})
	got := agent.AlignStreamsForTest(file, jf, "jpn")
	if got.IndexDrift || len(got.UnknownCodecTracks) != 0 || len(got.Streams) != 2 {
		t.Errorf("alignment = %+v", got)
	}
	if got.WantedLanguagePresent {
		t.Errorf("jpn present: %+v", got)
	}
}

func TestB4_AlignMoreJellyfinThanFile(t *testing.T) {
	t.Parallel()
	file := fileSubs(nil, 4)
	jf := jfStreams(6, nil)
	got := agent.AlignStreamsForTest(file, jf, "eng")
	if len(got.Streams) != 4 {
		t.Fatalf("streams = %d", len(got.Streams))
	}
	for _, s := range got.Streams[2:] {
		if s.FileIndex != -1 {
			t.Errorf("extra row = %+v", s)
		}
	}
}

func TestB4_AlignExternalListedFirst(t *testing.T) {
	t.Parallel()
	file := fileSubs(nil, 4)
	jf := []client.MediaStream{
		{Index: 0, Type: "Subtitle", Language: "eng", IsExternal: true},
		{Index: 1, Type: "Video"},
		{Index: 2, Type: "Audio"},
		{Index: 3, Type: "Audio"},
		{Index: 4, Type: "Subtitle", Language: "por"},
		{Index: 5, Type: "Subtitle", Language: "eng"},
	}
	got := agent.AlignStreamsForTest(file, jf, "eng")
	if got.IndexDrift {
		t.Errorf("drift: %+v", got)
	}
	if got.Streams[0].FfmpegMapsTo != 3 || got.Streams[1].FfmpegMapsTo != 4 {
		t.Errorf("maps = %+v", got.Streams)
	}
}

func TestB4_LangCodes(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"english", "eng", "en"} {
		i1, i2, ok := agent.LangCodesForTest(in)
		if !ok || i1 != "en" || i2 != "eng" {
			t.Errorf("%s = %q %q %v", in, i1, i2, ok)
		}
	}
	for _, in := range []string{"fra", "fr"} {
		i1, i2, ok := agent.LangCodesForTest(in)
		if !ok || i1 != "fr" || i2 != "fre" {
			t.Errorf("%s = %q %q %v", in, i1, i2, ok)
		}
	}
	if _, _, ok := agent.LangCodesForTest("xx"); ok {
		t.Error("xx ok")
	}
}

func TestB4_StreamAlignmentTool(t *testing.T) {
	t.Parallel()
	var probes atomic.Int64
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users" {
			_ = json.NewEncoder(w).Encode([]map[string]string{{"Id": "admin-1"}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/PlaybackInfo") && strings.Contains(r.URL.Path, "noitem") {
			_ = json.NewEncoder(w).Encode(client.PlaybackInfoResult{})
			return
		}
		_ = json.NewEncoder(w).Encode(client.PlaybackInfoResult{MediaSources: []client.MediaSource{{
			ID: "src1", Path: "/mnt/x.mkv", MediaStreams: jfStreams(4, map[int]string{4: "eng"}),
		}}})
	}))
	t.Cleanup(jf.Close)
	ma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		streams := fileSubs(nil, 4)
		streams[4].Language = "eng"
		_ = json.NewEncoder(w).Encode(mediaagentapi.ProbeResult{Streams: streams})
	}))
	t.Cleanup(ma.Close)
	disp := &agent.Dispatcher{Jellyfin: client.NewJellyfin(jf.URL, "k"), MediaAgent: client.NewMediaAgent(ma.URL, "")}
	call := func(what, item, lang string) (any, error) {
		return disp.CallScoped(context.Background(), agent.Scope{What: what}, "stream_alignment",
			map[string]any{"item_id": item, "language": lang})
	}
	if _, err := call(agent.WhatStuckRead, "ep1", "en"); err == nil ||
		!strings.Contains(err.Error(), "stream_alignment is disabled for stuck_read incidents") {
		t.Errorf("stuck_read err = %v", err)
	}
	_, err := call(agent.WhatSubtitles, "ep1", "xx")
	if err == nil || !strings.Contains(err.Error(), "unknown language") {
		t.Errorf("bad language err = %v", err)
	}
	if _, err = call(agent.WhatSubtitles, "noitem", "en"); err == nil || !strings.Contains(err.Error(), "noitem") {
		t.Errorf("no source err = %v", err)
	}
	if probes.Load() != 0 {
		t.Errorf("probes before success = %d", probes.Load())
	}
	res, err := call(agent.WhatSubtitles, "ep1", "english")
	if err != nil {
		t.Fatal(err)
	}
	al, ok := res.(agent.Alignment)
	if !ok || al.Path != "/mnt/x.mkv" || !al.WantedLanguagePresent || len(al.Streams) != 2 {
		t.Errorf("alignment = %#v", res)
	}
}

func englishLines(n int) string {
	var b strings.Builder
	for range n {
		b.WriteString("I think that this is the one we are looking for\n")
	}
	return b.String()
}

func TestB5_DetectLanguage(t *testing.T) {
	t.Parallel()
	if got := agent.DetectLanguageForTest(englishLines(20)); got != "eng" {
		t.Errorf("english = %q", got)
	}
	ja := strings.Repeat("これはテストです 私は学生です\n", 20)
	if got := agent.DetectLanguageForTest(ja); got != "" {
		t.Errorf("japanese = %q", got)
	}
	pt := strings.Repeat("Eu não sei o que você quer fazer com isso agora\n", 20)
	if got := agent.DetectLanguageForTest(pt); got != "" {
		t.Errorf("portuguese = %q", got)
	}
	if got := agent.DetectLanguageForTest(englishLines(2)); got != "" {
		t.Errorf("two lines = %q", got)
	}
}

func srtBody(cues int) string {
	var b strings.Builder
	for i := 1; i <= cues; i++ {
		fmt.Fprintf(
			&b, "%d\n00:00:%02d,000 --> 00:00:%02d,500\n<i>I think that this is the one we are %d</i>\n\n",
			i, i, i, i,
		)
	}
	return b.String()
}

func TestB5_CueLines(t *testing.T) {
	t.Parallel()
	got := agent.CueLinesForTest(srtBody(4), 3)
	if len(got) != 3 || strings.ContainsAny(got[0], "<>") || strings.Contains(got[0], "-->") {
		t.Errorf("cue lines = %q", got)
	}
}

type extractFake struct {
	srv  *httptest.Server
	subs atomic.Int64
}

func newExtractFake(t *testing.T, size int64, external bool, slow time.Duration) *extractFake {
	t.Helper()
	f := &extractFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users" {
			_ = json.NewEncoder(w).Encode([]map[string]string{{"Id": "admin-1"}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/PlaybackInfo") {
			_ = json.NewEncoder(w).Encode(client.PlaybackInfoResult{MediaSources: []client.MediaSource{{
				ID: "src1", Path: "/mnt/x.mkv", Size: size,
				MediaStreams: []client.MediaStream{
					{Index: 0, Type: "Video"},
					{Index: 5, Type: "Subtitle", Language: "eng", IsTextSubtitleStream: true, IsExternal: external},
				},
			}}})
			return
		}
		f.subs.Add(1)
		select {
		case <-time.After(slow):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(srtBody(4)))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *extractFake) dispatcher() *agent.Dispatcher {
	return &agent.Dispatcher{Jellyfin: client.NewJellyfin(f.srv.URL, "k")}
}

func extract(ctx context.Context, d *agent.Dispatcher) (any, error) {
	return d.CallScoped(ctx, agent.Scope{What: agent.WhatSubtitles}, "subtitle_extract_test",
		map[string]any{"item_id": "ep1", "stream_index": 5})
}

func TestB5_ExtractTestReportsCues(t *testing.T) {
	t.Parallel()
	f := newExtractFake(t, 1<<30, false, 0)
	res, err := extract(context.Background(), f.dispatcher())
	if err != nil {
		t.Fatal(err)
	}
	et, ok := res.(*agent.ExtractTest)
	if !ok || et.Status != 200 || et.Cues != 4 || len(et.FirstLines) != 3 ||
		et.DetectedLanguage != "eng" || et.StreamLanguage != "eng" {
		t.Errorf("result = %+v", res)
	}
}

func TestB5_ExtractTestTimeout(t *testing.T) {
	t.Parallel()
	f := newExtractFake(t, 1<<30, false, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := extract(ctx, f.dispatcher())
	if err != nil {
		t.Fatal(err)
	}
	if et, ok := res.(*agent.ExtractTest); !ok || !et.TimedOut {
		t.Errorf("result = %+v", res)
	}
}

func TestB5_ExtractTestRefusesHugeFile(t *testing.T) {
	t.Parallel()
	f := newExtractFake(t, 21<<30, false, 0)
	if _, err := extract(context.Background(), f.dispatcher()); !errors.Is(err, agent.ErrSubtitleFileTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if f.subs.Load() != 0 {
		t.Errorf("subtitle requests = %d", f.subs.Load())
	}
	ext := newExtractFake(t, 21<<30, true, 0)
	if _, err := extract(context.Background(), ext.dispatcher()); err != nil {
		t.Fatalf("external: %v", err)
	}
	if ext.subs.Load() != 1 {
		t.Errorf("external requests = %d", ext.subs.Load())
	}
}

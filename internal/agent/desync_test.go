package agent_test

import (
	"encoding/json"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func TestF40_DesyncReportDetection(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]bool{
		"subs are out of sync":                     true,
		"Subtitles OUT-OF-SYNC by 2 seconds":       true,
		"the subtitles are not in sync":            true,
		"subs late by a second":                    true,
		"subtitles come in too early":              true,
		"subtitle is about 3s behind the dialogue": true,
		"subs are ahead of the audio":              true,
		"desynced subs on ep 4":                    true,
		"subtitle timing is off":                   true,
		"no english subtitles":                     false,
		"subtitles missing":                        false,
		"subs show garbage characters":             false,
		"":                                         false,
	} {
		if got := agent.IsDesyncReport(text); got != want {
			t.Errorf("IsDesyncReport(%q) = %v, want %v", text, got, want)
		}
	}
}

func alignReply(id string, al agent.Alignment) (openai.ChatCompletionMessage, openai.ChatCompletionMessage) {
	fn := openai.FunctionCall{Name: "stream_alignment", Arguments: `{"item_id":"ep1","language":"en"}`}
	tc := openai.ToolCall{ID: id, Type: openai.ToolTypeFunction, Function: fn}
	body, _ := json.Marshal(al)
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{tc}},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: string(body)}
}

func desyncAlignment() agent.Alignment {
	return agent.Alignment{
		Path: "/data/library/Show/S01E02.mkv", WantedLanguagePresent: true, UnknownCodecTracks: []int{},
		Streams: []agent.AlignedStream{{
			JellyfinIndex: 2, Language: "eng", Title: "English", FileIndex: 2, FfmpegMapsTo: 2,
			FfmpegMapsToCodec: "subrip",
		}},
	}
}

func TestF40_DesyncReportEscalatesWithStreams(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{"{}"}})
	for _, c := range []struct {
		name    string
		what    string
		details string
		desync  bool
	}{
		{"subtitles out of sync", agent.WhatSubtitles, "the subs are out of sync by 2s", true},
		{"subtitles missing", agent.WhatSubtitles, "no english subs", false},
		{"audio out of sync", "cant_play", "audio out of sync", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inc := &db.Incident{
				Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: c.what, Title: "Show " + c.name,
				Details: c.details,
			}
			lifecycletest.Incident(t, database, inc)
			stale := desyncAlignment()
			stale.Path = "/stale.mkv"
			c1, r1 := alignReply("a1", stale)
			c2, r2 := alignReply("a2", desyncAlignment())
			c3, r3 := alignReply("a3", agent.Alignment{})
			r3.Content = `{"error":"jellyfin down"}`
			msgs := []openai.ChatCompletionMessage{c1, r1, c2, r2, c3, r3}
			forged := map[string]any{"subtitle_desync": !c.desync, "subtitle_streams": map[string]any{"path": "/x"}}
			res, err := ag.ConcludeForTest(t.Context(), inc, completeCall(forged), &stubGate{}, &msgs)
			if err != nil || res == nil {
				t.Fatalf("conclude: %v %v", res, err)
			}
			checkDesync(t, res, c.desync)
		})
	}
}

func checkDesync(t *testing.T, res *agent.DiagnosticResult, desync bool) {
	t.Helper()
	if res.SubtitleDesync != desync {
		t.Fatalf("SubtitleDesync = %v, want %v", res.SubtitleDesync, desync)
	}
	if !desync {
		if res.SubtitleStreams != nil || res.Outcome.Kind == agent.DiagnosisManual {
			t.Errorf("non-desync result changed: streams %v outcome %v", res.SubtitleStreams, res.Outcome)
		}
		return
	}
	if res.Outcome.Kind != agent.DiagnosisManual || !res.RequiresApproval ||
		res.EscalateAction != agent.EscalateManualInvestigation || res.VerifyAfterSeconds != 0 {
		t.Errorf("desync not escalated: %+v", res)
	}
	if res.SubtitleStreams == nil || res.SubtitleStreams.Path != "/data/library/Show/S01E02.mkv" {
		t.Errorf("SubtitleStreams = %+v, want the last successful alignment", res.SubtitleStreams)
	}
	if err := agent.CheckDiagnosis(res); err != nil {
		t.Errorf("CheckDiagnosis: %v", err)
	}
}

func TestF40_PromptCoversDesync(t *testing.T) {
	t.Parallel()
	if !strings.Contains(agent.SystemPromptForTest(), "desync report") {
		t.Error("prompt has no desync rule")
	}
}

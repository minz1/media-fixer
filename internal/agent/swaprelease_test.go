package agent_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
)

func swapParams() map[string]any {
	return map[string]any{
		"title": "Show", "season": float64(1), "episode": float64(6), "release": "Show.S01E06.1080p", "language": "en",
	}
}

func swapPlanFixture(what, language string) client.SwapPlan {
	return client.SwapPlan{
		What: what, Language: language, Title: "Show", SeriesID: 5, Season: 1, Episode: 6, EpisodeID: 66,
		Steps: []operation.Step{
			{
				Kind: operation.StepImport, App: "sonarr", Path: "/data/downloads/sonarr/R/e.mkv", Size: 100, FileID: 9,
				MediaID: 5, EpisodeIDs: []int{66}, Season: 1, Episode: 6,
			},
			{
				Kind:    operation.StepJellyfinNotify,
				App:     "jellyfin",
				Path:    "/media/tv/e.mkv",
				Name:    "jf-1",
				Season:  1,
				Episode: 6,
			},
		},
	}
}

func TestD7_SwapIsPlanKind(t *testing.T) {
	t.Parallel()
	if !slices.Contains(agent.EscalateActionEnumForTest(), agent.EscalateSwapRelease) {
		t.Fatalf("enum %v lacks %q", agent.EscalateActionEnumForTest(), agent.EscalateSwapRelease)
	}
	got, err := agent.Classify(withParams(diag(true, agent.EscalateSwapRelease, 0, "x"), swapParams()), nil)
	if err != nil || got.Kind != agent.DiagnosisNeedsApproval || got.Plan != agent.EscalateSwapRelease {
		t.Fatalf("with params: %+v, %v", got, err)
	}
	if _, err = agent.Classify(diag(true, agent.EscalateSwapRelease, 0, "x"), nil); err == nil ||
		!strings.Contains(err.Error(), "escalate_params is required for swap_release") {
		t.Fatalf("without params: err = %v", err)
	}
	if _, err = agent.Classify(
		withParams(diag(false, agent.EscalateSwapRelease, 0, "x"), swapParams()),
		nil,
	); err == nil {
		t.Fatal("swap_release without requires_approval was accepted")
	}
}

func TestD7_OperationForSwap(t *testing.T) {
	t.Parallel()
	ag := operationAgent(&agent.Dispatcher{})
	result := &agent.DiagnosticResult{EscalateAction: agent.EscalateSwapRelease}
	plan := swapPlanFixture(agent.WhatSubtitles, "eng")
	b := swapJSON(t, plan)
	spec, err := ag.OperationFor(operationSubject(), result, b)
	if err != nil || spec.Kind != operation.KindSwapRelease || len(spec.Steps) != 2 ||
		string(spec.Source) != string(b) {
		t.Fatalf("spec = %+v, err = %v", spec, err)
	}
	for name, bad := range map[string]client.SwapPlan{
		"cant_play":   swapPlanFixture("cant_play", "eng"),
		"no language": swapPlanFixture(agent.WhatSubtitles, ""),
	} {
		if _, err = ag.OperationFor(operationSubject(), result, swapJSON(t, bad)); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	if _, err = ag.PlanEscalation(t.Context(), &agent.DiagnosticResult{
		EscalateAction: agent.EscalateSwapRelease, EscalateParams: swapParams(),
	}, "cant_play"); err == nil || !strings.Contains(err.Error(), "subtitles") {
		t.Errorf("PlanEscalation on a non-subtitles incident: err = %v", err)
	}
}

func TestD7_SwapNeverAutonomous(t *testing.T) {
	t.Parallel()
	for _, spec := range agent.ToolRegistry() {
		if spec.Name == agent.EscalateSwapRelease && spec.Risk == agent.RiskWrite {
			t.Errorf("%s is a write tool", spec.Name)
		}
	}
	if agent.IsAutonomousActionForTest(agent.EscalateSwapRelease) {
		t.Error("swap_release is autonomous")
	}
	if agent.UserPhrase(agent.EscalateSwapRelease) == "" {
		t.Error("swap_release has no progress phrase")
	}
}

func swapJSON(t *testing.T, p client.SwapPlan) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

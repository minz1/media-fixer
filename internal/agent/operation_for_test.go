package agent_test

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

func operationSubject() operation.Subject {
	return operation.Subject{Kind: operation.SubjectIncident, ID: "inc-1"}
}

func operationAgent(d *agent.Dispatcher) *agent.Agent {
	return agent.New(nil, "stub", d, nil, nil, slog.New(slog.DiscardHandler))
}

func TestOperationFor_PremiseRefusalIsError(t *testing.T) {
	t.Parallel()
	result := &agent.DiagnosticResult{EscalateAction: agent.EscalateRemoveAndSearch}
	readable := &client.ReadabilityProbe{Checked: true, Probed: 1, ReadableFile: "/data/tv/ep.mkv", BytesRead: 1024}
	ag := operationAgent(&agent.Dispatcher{Sonarr: client.NewArr("http://sonarr.invalid", "k")})
	_, err := ag.OperationFor(operationSubject(), result, planJSON(t, client.ReplacePlan{
		MediaType: "tv", Title: "Show", Scope: "episode",
		Reason: client.ReplaceReasonUnreadable, Readability: readable,
	}))
	if err == nil || !strings.Contains(err.Error(), "read back fine") {
		t.Fatalf("err = %v, want the premise refusal", err)
	}
}

func TestOperationFor_NoClientIsError(t *testing.T) {
	t.Parallel()
	ag := operationAgent(&agent.Dispatcher{})
	replace := &agent.DiagnosticResult{EscalateAction: agent.EscalateRemoveAndSearch}
	for _, mt := range []string{"tv", "movie"} {
		_, err := ag.OperationFor(operationSubject(), replace, planJSON(t, client.ReplacePlan{
			MediaType: mt, Title: "Show", Scope: "episode",
		}))
		if err == nil || !strings.Contains(err.Error(), "no client configured") {
			t.Errorf("%s: err = %v, want no client configured", mt, err)
		}
	}
	readd := &agent.DiagnosticResult{EscalateAction: agent.EscalateDeleteTorrentReadd}
	b, err := json.Marshal(client.ReaddPlan{Name: "n", InfoHash: "h", Magnet: "magnet:?xt=1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ag.OperationFor(operationSubject(), readd, b); err == nil ||
		!strings.Contains(err.Error(), "decypharr not configured") {
		t.Errorf("readd: err = %v, want decypharr not configured", err)
	}
}

func TestOperationFor_DecodesStoredPlanExactly(t *testing.T) {
	t.Parallel()
	ag := operationAgent(&agent.Dispatcher{
		Sonarr:    client.NewArr("http://sonarr.invalid", "k"),
		Decypharr: client.NewDecypharr("http://decypharr.invalid", "k"),
	})

	plan := client.ReplacePlan{
		MediaType: "tv", Title: "Show", Scope: "season", MediaID: 7, SeasonNumber: 2,
		Files:  []client.ReplaceFile{{ID: 11, Path: "/tv/a.mkv"}, {ID: 12, Path: "/tv/b.mkv"}},
		Reason: client.ReplaceReasonWrongContent,
	}
	stored := planJSON(t, plan)
	got, err := ag.OperationFor(operationSubject(), &agent.DiagnosticResult{
		EscalateAction: agent.EscalateRemoveAndSearch,
	}, stored)
	if err != nil {
		t.Fatalf("OperationFor: %v", err)
	}
	var decoded client.ReplacePlan
	if err = json.Unmarshal(stored, &decoded); err != nil {
		t.Fatal(err)
	}
	want, err := opexec.ReplaceSpec(operationSubject(), &decoded, stored)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replace spec differs\n got %+v\nwant %+v", got, want)
	}

	readdPlan := client.ReaddPlan{Name: "n", InfoHash: "h", Category: "tv", Magnet: "magnet:?xt=1"}
	readdStored, err := json.Marshal(readdPlan)
	if err != nil {
		t.Fatal(err)
	}
	gotReadd, err := ag.OperationFor(operationSubject(), &agent.DiagnosticResult{
		EscalateAction: agent.EscalateDeleteTorrentReadd,
	}, readdStored)
	if err != nil {
		t.Fatalf("OperationFor readd: %v", err)
	}
	wantReadd, err := opexec.ReaddSpec(operationSubject(), &readdPlan, readdStored)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotReadd, wantReadd) {
		t.Errorf("readd spec differs\n got %+v\nwant %+v", gotReadd, wantReadd)
	}
}

func TestOperationFor_TooManyStepsIsBuildError(t *testing.T) {
	t.Parallel()
	ag := operationAgent(&agent.Dispatcher{Sonarr: client.NewArr("http://sonarr.invalid", "k")})
	plan := client.ReplacePlan{MediaType: "tv", Title: "Show", Scope: "season", MediaID: 7, SeasonNumber: 2}
	plan.Files = make([]client.ReplaceFile, operation.MaxSteps())
	for i := range plan.Files {
		plan.Files[i] = client.ReplaceFile{ID: i + 1, Path: "/tv/x.mkv"}
	}
	_, err := ag.OperationFor(operationSubject(), &agent.DiagnosticResult{
		EscalateAction: agent.EscalateRemoveAndSearch,
	}, planJSON(t, plan))
	if err == nil || !strings.Contains(err.Error(), "narrow the scope") {
		t.Fatalf("err = %v, want the step-cap refusal", err)
	}
}

func TestS1_ResyncOperationForRefused(t *testing.T) {
	t.Parallel()
	ag := operationAgent(&agent.Dispatcher{})
	_, err := ag.OperationFor(operationSubject(), &agent.DiagnosticResult{
		EscalateAction: lifecycle.EscalateResyncSubtitle,
	}, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "no automated plan") {
		t.Fatalf("err = %v, want the no-plan refusal", err)
	}
}

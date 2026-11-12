package incident_test

import (
	"context"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

const (
	candidatesField = "Subtitle candidates"
	downloadedField = "Subtitles downloaded"
)

func notFound(cands ...client.BazarrCandidate) *agent.BazarrSearchResult {
	return &agent.BazarrSearchResult{Summary: "nothing", Candidates: cands}
}

func twoCandidates() *agent.BazarrSearchResult {
	return notFound(
		client.BazarrCandidate{Provider: "opensubtitles", Release: "Show.S01E01.WEB", Score: 91},
		client.BazarrCandidate{Provider: "subf2m", Release: "Show.S01E01.720p", Score: 74},
	)
}

func subsResult(search *agent.BazarrSearchResult) *agent.DiagnosticResult {
	return &agent.DiagnosticResult{RootCause: "no english track", PrimaryReason: "r", SubtitleSearch: search}
}

func fieldNamed(fields []card.Field, name string) (card.Field, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f, true
		}
	}
	return card.Field{}, false
}

func noticeField(n lifecycle.Notice, name string) (string, bool) {
	for _, f := range n.Fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

func TestC12_CardListsCandidates(t *testing.T) {
	t.Parallel()
	subs := &db.Incident{ID: "abc", What: "subtitles", Title: "Show"}
	f, ok := fieldNamed(incident.SurfaceCardForTest(subs, subsResult(twoCandidates()), "").Fields, candidatesField)
	if !ok {
		t.Fatal("no candidates field")
	}
	want := "1. opensubtitles · Show.S01E01.WEB · 91\n2. subf2m · Show.S01E01.720p · 74\nPick one in Bazarr, then Rerun"
	if f.Value != want {
		t.Errorf("value = %q, want %q", f.Value, want)
	}

	cant := &db.Incident{ID: "abc", What: "cant_play", Title: "Show"}
	for name, c := range map[string]card.Card{
		"cant_play":  incident.SurfaceCardForTest(cant, subsResult(twoCandidates()), ""),
		"nil search": incident.SurfaceCardForTest(subs, subsResult(nil), ""),
		"empty":      incident.SurfaceCardForTest(subs, subsResult(notFound()), ""),
	} {
		for _, n := range []string{candidatesField, downloadedField} {
			if _, has := fieldNamed(c.Fields, n); has {
				t.Errorf("%s card has %q", name, n)
			}
		}
	}

	found := &agent.BazarrSearchResult{Found: true, Path: "/tv/Show/Show.S01E01.en.srt"}
	f, ok = fieldNamed(incident.SurfaceCardForTest(subs, subsResult(found), "").Fields, downloadedField)
	if !ok || f.Value != "Show.S01E01.en.srt" {
		t.Errorf("downloaded field = %+v, %v", f, ok)
	}
}

func TestC12_FieldOnEveryOwnerCard(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	finding := subsResult(twoCandidates())
	subs := &db.Incident{ID: "abc", What: "subtitles", Title: "Show", Finding: finding}
	v, ok := noticeField(svc.NotVerifiedForTest(subs), candidatesField)
	if !ok || !strings.Contains(v, "opensubtitles") {
		t.Errorf("not-verified card field = %q, %v", v, ok)
	}
	cant := &db.Incident{ID: "abc", What: "cant_play", Title: "Show", Finding: finding}
	if _, has := noticeField(svc.NotVerifiedForTest(cant), candidatesField); has {
		t.Error("cant_play not-verified card has the field")
	}

	for _, what := range []string{"subtitles", "cant_play"} {
		for _, verdict := range []string{
			`{"verdict":"escalate_to_owner","reason":"odd"}`,
			`{"verdict":"suggest_alternative","alternative_action":"x","reason":"y"}`,
		} {
			c := controlReviewCard(t, what, verdict)
			if _, has := fieldNamed(c.Fields, candidatesField); has != (what == "subtitles") {
				t.Errorf("%s %s card: candidates field present = %v", what, verdict[:20], has)
			}
		}
	}
}

func controlReviewCard(t *testing.T, what, verdict string) card.Card {
	t.Helper()
	low := classified(&agent.DiagnosticResult{
		RootCause: "x", Confidence: "low", PrimaryAction: "run-a", PrimaryReason: "x",
		SubtitleSearch: twoCandidates(),
	}, nil)
	ag := newGateAgent(func(context.Context, agent.Gate) agent.RunExit {
		return agent.RunExit{State: agent.RunConcluded, Finding: low}
	})
	env := newRoutingEnv(t, ag, controlServer(t, nil, verdict), func(tm *incident.Timers) {
		tm.RetryFirstMinutes, tm.RetrySecondMinutes = 1, 1
	})
	if _, err := env.svc.Handle(t.Context(), &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "u-alice", What: what, Title: "Show",
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyWithin(t, loadedWaitTimeout, "the owner card", func() bool { return len(env.notif.ownerCards()) > 0 })
	return env.notif.ownerCards()[0]
}

func TestC12_ReporterNoteNamesDownload(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	found := subsResult(&agent.BazarrSearchResult{Found: true, Path: "/tv/Show/Show.S01E01.en.srt"})
	got := svc.FixedNoteForTest(&db.Incident{What: "subtitles", Title: "Show", Finding: found})
	if !strings.Contains(got, "Subtitles downloaded: Show.S01E01.en.srt") {
		t.Errorf("note = %q", got)
	}
	plain := svc.FixedNoteForTest(&db.Incident{What: "subtitles", Title: "Show", Finding: subsResult(nil)})
	if strings.Contains(plain, downloadedField) {
		t.Errorf("note without a search = %q", plain)
	}
	other := svc.FixedNoteForTest(&db.Incident{What: "cant_play", Title: "Show", Finding: found})
	if strings.Contains(other, downloadedField) {
		t.Errorf("cant_play note = %q", other)
	}
}

func TestC12_CardCap(t *testing.T) {
	t.Parallel()
	var cands []client.BazarrCandidate
	for range 5 {
		cands = append(cands, client.BazarrCandidate{Provider: "p", Release: strings.Repeat("r", 400), Score: 80})
	}
	subs := &db.Incident{ID: "abc", What: "subtitles", Title: "Show"}
	f, ok := fieldNamed(incident.SurfaceCardForTest(subs, subsResult(notFound(cands...)), "").Fields, candidatesField)
	if !ok {
		t.Fatal("no candidates field")
	}
	if len(f.Value) > 1024 || !strings.HasSuffix(f.Value, "Pick one in Bazarr, then Rerun") {
		t.Errorf("len %d, value %q", len(f.Value), f.Value[max(0, len(f.Value)-40):])
	}
}

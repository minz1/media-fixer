package incident_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func swapCardPlan() *client.SwapPlan {
	return &client.SwapPlan{
		What: agent.WhatSubtitles, Language: "eng", Title: "Show", Season: 1, Episode: 6, EpisodeTitle: "Pilot",
		Current: client.SwapCurrent{
			SwapFile: client.SwapFile{
				Path:         "/data/tv/Show/e6.mkv",
				Size:         100,
				ReleaseGroup: "OLD",
				Quality:      "WEBDL-1080p",
			},
			SourceTitle: "Show.S01E06.OLD",
			DownloadID:  "abc",
			PackSize:    1,
		},
		Candidate: client.SwapCandidate{
			SwapFile: client.SwapFile{
				Path:         "/data/downloads/sonarr/New/e6.mkv",
				Size:         200,
				ReleaseGroup: "NEW",
				Quality:      "Bluray-1080p",
			},
			Folder:            "/data/downloads/sonarr/New",
			CustomFormatScore: 40,
			GrabAgeHours:      20,
			GraceHours:        168,
			Rejections:        []string{"Not a quality revision upgrade for existing episode file(s)"},
		},
		Mapping: []client.CandidateEpisode{{SeasonNumber: 1, EpisodeNumber: 6}}, GuardVerdict: "ok",
		Probe: client.SwapProbe{
			Subtitles: []client.SwapSubtitle{{Index: 2, Language: "eng", Codec: "subrip"}},
			Wanted:    client.SwapSubtitle{Index: 2, Language: "eng", Codec: "subrip"},
		},
		Blocklist:  client.SwapBlocklist{SourceTitle: "Show.S01E06.OLD"},
		RecycleBin: "/recycle", AutoRedownloadOn: true,
	}
}

func TestD7_SwapCardSections(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	inc := cardIncident(t, d, db.StatusManualTestNeeded, agent.EscalateSwapRelease)
	fp := storePlan(t, d, inc.ID, swapCardPlan())

	up := renderInc(t, svc, inc.ID)
	var vals []string
	for _, s := range up.Sections {
		vals = append(vals, s.Name+": "+s.Value)
	}
	joined := strings.Join(vals, "\n")
	want := []string{
		"S01E06", "eng", "Sonarr deletes this file as part of the import", "/recycle", "NEW", "40", "168",
		"Not a quality revision upgrade", "guard", "subrip", "new Jellyfin item", "Show.S01E06.OLD",
		"automatic search for S01E06", "Not automatic",
	}
	at := 0
	for _, w := range want {
		i := strings.Index(joined[at:], w)
		if i < 0 {
			t.Fatalf("section text lacks %q in order after offset %d:\n%s", w, at, joined)
		}
		at += i
	}
	if len(up.Sections) > 15 {
		t.Errorf("%d sections, want at most 15", len(up.Sections))
	}
	i := slices.IndexFunc(up.Buttons, func(b card.Button) bool { return strings.HasPrefix(b.ID, "mf1:inc:approve:") })
	if i < 0 {
		t.Fatalf("buttons = %+v, want Approve", up.Buttons)
	}
	if a, _ := card.Decode(up.Buttons[i].ID); a.Args[len(a.Args)-1] != fp {
		t.Errorf("approve args = %v, want fp %s last", a.Args, fp)
	}

	res := clickInc(t, svc, "approve", inc.ID, db.StatusManualTestNeeded, fp)
	if len(res.Confirm) != 2 || res.Confirm[0].Label != "Yes, swap the release" {
		t.Errorf("confirm = %+v, want the swap label", res.Confirm)
	}
}

func TestD7_SwapCardNoRecycleBin(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	inc := cardIncident(t, d, db.StatusManualTestNeeded, agent.EscalateSwapRelease)
	p := swapCardPlan()
	p.RecycleBin = ""
	storePlan(t, d, inc.ID, p)
	up := renderInc(t, svc, inc.ID)
	for _, s := range up.Sections {
		if strings.Contains(s.Value, "No recycle bin: Sonarr deletes this file for good on import") {
			return
		}
	}
	t.Fatalf("sections = %+v, want the no recycle bin warning", up.Sections)
}

func TestD7_SwapCardTooLong(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	inc := cardIncident(t, d, db.StatusManualTestNeeded, agent.EscalateSwapRelease)
	p := swapCardPlan()
	for i := range 80 {
		p.Candidate.Rejections = append(
			p.Candidate.Rejections,
			fmt.Sprintf("rejection %d %s", i, strings.Repeat("x", 60)),
		)
	}
	storePlan(t, d, inc.ID, p)
	up := renderInc(t, svc, inc.ID)
	if len(up.Sections) != 1 || up.Sections[0].Value != "Plan too long for Discord; review it on the dashboard" {
		t.Errorf("sections = %+v, want only the too-long line", up.Sections)
	}
}

func TestD7_SwapPartialLine(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("SwapPartial"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonPlanPartial, Finding: lifecycletest.Escalatable(),
	})
	if err := env.db.LogAction(t.Context(), &db.ActionLog{
		IncidentID: s.ID, Action: agent.EscalateSwapRelease, TriggeredBy: "owner", Status: db.ActionFailed,
		Result: `{"imported":true,"notified":false,"blocklisted":false}`,
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range env.render(t, s.ID).Sections {
		if f.Name == "Result" {
			got = append(got, f.Value)
		}
	}
	if len(got) != 1 || got[0] != "Partial: imported, Jellyfin not told" {
		t.Fatalf("partial lines %q", got)
	}
}

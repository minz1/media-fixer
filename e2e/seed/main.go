package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

const (
	novaID      = 2
	linkID      = 3
	samuraiID   = 4
	indexer     = "Nyaa"
	linkRel     = "Link.Click.S01.1080p.WEB"
	day         = 24 * time.Hour
	stuckStreak = 4
)

func main() {
	path := flag.String("db", "", "sqlite path")
	late := flag.Bool("late", false, "seed only the investigating incident, once startup recovery has run")
	flag.Parse()
	if err := run(*path, *late); err != nil {
		log.Fatal(err)
	}
}

func run(path string, late bool) error {
	d, err := db.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()
	if late {
		ids, statuses, lateErr := seedInvestigating(ctx, d)
		if lateErr != nil {
			return lateErr
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"ids": ids, "statuses": statuses})
	}
	ids, statuses, err := seedIncidents(ctx, d)
	if err != nil {
		return err
	}
	if err = seedSweep(ctx, d); err != nil {
		return err
	}
	if err = seedTargets(ctx, d); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"ids": ids, "statuses": statuses})
}

func seedIncidents(ctx context.Context, d *db.DB) (map[string]string, map[string]string, error) {
	finding := map[string]any{
		"root_cause": "Import blocked: sample detection failed on a 1080p release.", "confidence": "high",
		"primary_action": "retry_import", "primary_reason": "The file is intact; the importer only needs a nudge.",
	}
	specs := []struct {
		title  string
		status db.IncidentStatus
		key    string
	}{
		{"Link Click S01E02 will not play", db.StatusVerifying, "incident"},
		{"NOVA S01E01 stuck at importing", db.StatusManualTestNeeded, ""},
		{"American Horror Story S01E03 skips", db.StatusResolved, ""},
		{"Samurai Champloo S01E05 missing audio", db.StatusBlocked, "blocked"},
	}
	ids, statuses := map[string]string{}, map[string]string{}
	for _, sp := range specs {
		inc := &db.Incident{
			Status: db.StatusOpen, Source: "discord", ReportedBy: "viewer", What: "playback",
			Title: sp.title, Details: "Reported from the e2e seed.",
		}
		if err := d.CreateIncident(ctx, inc); err != nil {
			return nil, nil, err
		}
		if err := d.SetIncidentFinding(ctx, inc.ID, finding, nil); err != nil {
			return nil, nil, err
		}
		if err := d.UpdateIncidentStatus(ctx, inc.ID, sp.status); err != nil {
			return nil, nil, err
		}
		if sp.status == db.StatusBlocked {
			if err := d.SetAutonomousLocked(ctx, inc.ID, true); err != nil {
				return nil, nil, err
			}
		}
		statuses[sp.title] = string(sp.status)
		if sp.status == db.StatusBlocked {
			if err := seedConversation(ctx, journal.New(d), inc.ID, sp.title); err != nil {
				return nil, nil, err
			}
		}
		if sp.key != "" {
			ids[sp.key] = inc.ID
		}
	}
	return ids, statuses, nil
}

func seedSweep(ctx context.Context, d *db.DB) error {
	start := time.Now().Add(-3 * time.Hour)
	one, lookups, ms := 1, 12, 340
	run, err := d.CreateSweepRun(ctx, "schedule", false, start)
	if err != nil {
		return err
	}
	items := []db.SweepItem{
		{
			Target: "s4:1", SeriesID: samuraiID, SeriesTitle: "Samurai Champloo",
			ReleaseTitle: "Samurai.Champloo.S01.1080p.BluRay", Result: "grabbed",
		},
		{Target: "s3:1", SeriesID: linkID, SeriesTitle: "Link Click", ReleaseTitle: linkRel, Result: "refused"},
		{Target: "s2:1", SeriesID: novaID, SeriesTitle: "NOVA", Result: "no_pack"},
	}
	for i := range items {
		it := &items[i]
		it.RunID, it.Season, it.Indexer = run, &one, indexer
		it.At, it.LookupResults, it.LookupMS = start.Add(time.Duration(i)*time.Minute), &lookups, &ms
		if err = d.AddSweepItem(ctx, it); err != nil {
			return err
		}
	}
	refused := &db.SweepRefused{
		TitleKey: "link click s01", ReleaseTitle: linkRel, Indexer: indexer,
		RefusedAt: start.Add(time.Minute), Reason: "release contains a sample file",
	}
	if err = d.AddSweepRefused(ctx, refused); err != nil {
		return err
	}
	counts := db.SweepCounts{
		Grabs: 1, Lookups: len(items), Searched: len(items), Refused: 1, NoPack: 1,
	}
	if err = d.FinishSweepRun(ctx, run, "finished", "", counts, start.Add(time.Hour)); err != nil {
		return err
	}
	bs, err := d.CreateSweepRun(ctx, db.BabysitTrigger+" Link Click", false, start.Add(time.Hour))
	if err != nil {
		return err
	}
	return d.FinishSweepRun(ctx, bs, "finished", "", db.SweepCounts{}, start.Add(time.Hour+time.Minute))
}

func seedTargets(ctx context.Context, d *db.DB) error {
	now := time.Now()
	targets := []db.SweepTarget{
		{Target: "s3:1", SeriesID: linkID, SeriesTitle: "Link Click", Streak: stuckStreak},
		{Target: "s4:1", SeriesID: samuraiID, SeriesTitle: "Samurai Champloo", Streak: 1},
		{Target: "s2:1", SeriesID: novaID, SeriesTitle: "NOVA"},
	}
	for i, t := range targets {
		t.Season, t.Missing, t.Attempts = 1, i+1, t.Streak
		t.LastAttempt, t.NextEligible = now.Add(-3*time.Hour), now.Add(day)
		if err := d.UpsertSweepTarget(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

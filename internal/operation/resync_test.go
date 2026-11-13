package operation_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/operation"
)

func syncStep() operation.Step {
	return operation.Step{
		Kind: operation.StepSubtitleSync, App: "bazarr", Path: "/media/tv/Show/e6.en.srt", Name: "en",
		MediaID: 66, Category: "episode", Baseline: 0,
	}
}

func resyncSpec(steps ...operation.Step) operation.Spec {
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectIncident, ID: "inc-1"}, Kind: operation.KindResyncSubtitle,
		Title: "Show S01E06", Steps: steps,
	}
}

func resyncCreate(steps ...operation.Step) error {
	_, err := operation.Next(operation.Op{}, created(resyncSpec(steps...)))
	return err
}

func TestS1_ResyncShape(t *testing.T) {
	t.Parallel()
	good, movie := syncStep(), syncStep()
	movie.Category = "movie"
	movie.Baseline = 3
	for name, steps := range map[string][]operation.Step{
		"episode": {good},
		"movie":   {movie},
	} {
		if err := resyncCreate(steps...); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	noApp, noPath, noName, noID, badCat, noCat, negative := good, good, good, good, good, good, good
	noApp.App = "sonarr"
	noPath.Path = ""
	noName.Name = ""
	noID.MediaID = 0
	badCat.Category = "season"
	noCat.Category = ""
	negative.Baseline = -1
	for name, steps := range map[string][]operation.Step{
		"two steps":    {good, good},
		"other kind":   {{Kind: operation.StepBlocklist, App: "sonarr", HistoryID: 1}},
		"wrong app":    {noApp},
		"missing path": {noPath},
		"missing lang": {noName},
		"missing id":   {noID},
		"bad category": {badCat},
		"no category":  {noCat},
		"negative":     {negative},
	} {
		if err := resyncCreate(steps...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestS1_ResyncLines(t *testing.T) {
	t.Parallel()
	spec := resyncSpec(syncStep())
	cases := []struct {
		name string
		op   operation.Op
		line string
		next string
	}{
		{"pending", play(spec), "About to start step 1 of 1.", ""},
		{"done", play(spec, started(0), finished(0, operation.ResultDone)), "Resync sent to Bazarr.", ""},
		{"failed", play(spec, stopAt(0, operation.ResultFailed)...), "Stopped at step 1 of 1; nothing changed.", ""},
		{
			"unknown", play(spec, stopAt(0, operation.ResultUnknown)...),
			"The resync may have been sent to Bazarr.", "Resume: resync e6.en.srt (en) in Bazarr.",
		},
	}
	for _, c := range cases {
		if got := operation.Line(c.op); got != c.line {
			t.Errorf("%s: line %q, want %q", c.name, got, c.line)
		}
		if got := operation.NextLine(c.op); got != c.next {
			t.Errorf("%s: next %q, want %q", c.name, got, c.next)
		}
	}
}

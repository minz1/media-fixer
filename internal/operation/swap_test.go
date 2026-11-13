package operation_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/operation"
)

func importStep() operation.Step {
	return operation.Step{
		Kind: operation.StepImport, App: "sonarr", Path: "/data/downloads/sonarr/Show.S01E06/Show.S01E06.mkv",
		Size: 100, FileID: 9, MediaID: 5, EpisodeIDs: []int{66}, Season: 1, Episode: 6,
	}
}

func notifyStep() operation.Step {
	return operation.Step{
		Kind: operation.StepJellyfinNotify, App: "jellyfin", Path: "/media/tv/Show/e6.mkv", Name: "jf-1",
		Season: 1, Episode: 6,
	}
}

func swapSpec(steps ...operation.Step) operation.Spec {
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectIncident, ID: "inc-1"}, Kind: operation.KindSwapRelease,
		Title: "Show S01E06", Steps: steps,
	}
}

func swapCreate(steps ...operation.Step) error {
	_, err := operation.Next(operation.Op{}, created(swapSpec(steps...)))
	return err
}

func TestD1_SwapShape(t *testing.T) {
	t.Parallel()
	imp, note, block := importStep(), notifyStep(), blocklistStep(1)
	for name, steps := range map[string][]operation.Step{
		"with blocklist": {imp, note, block},
		"without":        {imp, note},
	} {
		if err := swapCreate(steps...); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	noFile, noEpisode, twoEpisodes, noSize, noName, noJF := imp, imp, imp, imp, note, note
	noFile.FileID = 0
	noEpisode.EpisodeIDs = nil
	twoEpisodes.EpisodeIDs = []int{66, 67}
	noSize.Size = 0
	noName.Name = ""
	noJF.App = "sonarr"
	for name, steps := range map[string][]operation.Step{
		"wrong order":   {note, imp, block},
		"blocklist 1st": {block, imp, note},
		"third kind":    {imp, note, block, searchStep()},
		"only import":   {imp},
		"missing file":  {noFile, note},
		"no episode":    {noEpisode, note},
		"two episodes":  {twoEpisodes, note},
		"no size":       {noSize, note},
		"no jf name":    {imp, noName},
		"notify app":    {imp, noJF},
	} {
		if err := swapCreate(steps...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestD1_SwapLines(t *testing.T) {
	t.Parallel()
	spec := swapSpec(importStep(), notifyStep(), blocklistStep(1))
	cases := []struct {
		name string
		op   operation.Op
		line string
		next string
	}{
		{
			"import failed", play(spec, stopAt(0, operation.ResultFailed)...),
			"Stopped at step 1 of 3; nothing changed.", "",
		},
		{
			"notify failed", play(spec, stopAt(1, operation.ResultFailed)...),
			"Imported; Jellyfin not told; grab not blocklisted.",
			"Resume: tell Jellyfin about the new file, mark grab dl1 failed.",
		},
		{
			"blocklist failed", play(spec, stopAt(2, operation.ResultFailed)...),
			"Imported; Jellyfin told; grab not blocklisted.", "Resume: mark grab dl1 failed.",
		},
		{
			"import unknown", play(spec, stopAt(0, operation.ResultUnknown)...),
			"The import may have landed; Jellyfin not told; grab not blocklisted.",
			"Resume: import Show.S01E06.mkv over S01E06, tell Jellyfin about the new file, mark grab dl1 failed.",
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

func TestD1_SwapFilelessLine(t *testing.T) {
	t.Parallel()
	spec := swapSpec(importStep(), notifyStep())
	evs := []operation.Event{
		started(0),
		{
			Kind: operation.StepFinished, At: at(2), Index: 0, Outcome: operation.ResultUnknown,
			Note: "fileless: no file",
		},
	}
	op := play(spec, evs...)
	want := "Sonarr ran the import but S01E06 has no file; search from Sonarr."
	if got := operation.Line(op); got != want {
		t.Errorf("line %q, want %q", got, want)
	}
}

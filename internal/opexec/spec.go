package opexec

import (
	"fmt"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
)

const appSonarr = "sonarr"

func ReplaceSpec(subject operation.Subject, plan *client.ReplacePlan, source []byte) (operation.Spec, error) {
	app := appSonarr
	if plan.MediaType == client.ReplaceMediaMovie {
		app = appRadarr
	}
	search, err := searchStep(app, plan)
	if err != nil {
		return operation.Spec{}, err
	}
	var steps []operation.Step
	if !plan.SkipBlocklist {
		for _, g := range plan.GrabsToBlocklist {
			steps = append(steps, operation.Step{
				Kind: operation.StepBlocklist, App: app, HistoryID: g.ID, DownloadID: g.DownloadID,
				GrabbedAt: g.Date, MediaID: plan.MediaID, Season: plan.SeasonNumber,
			})
		}
	}
	for _, f := range plan.Files {
		steps = append(steps, operation.Step{
			Kind: operation.StepDeleteFile, App: app, FileID: f.ID, Path: f.Path, Size: f.Size,
			MediaID: plan.MediaID, Season: plan.SeasonNumber,
		})
	}
	steps = append(steps, search)
	if len(steps) > operation.MaxSteps() {
		return operation.Spec{}, fmt.Errorf(
			"this plan has %d steps; one approval runs at most %d; narrow the scope", len(steps), operation.MaxSteps())
	}
	return operation.Spec{
		Subject: subject, Kind: operation.KindReplace, Title: plan.Title, Source: source, Steps: steps,
	}, nil
}

func searchStep(app string, plan *client.ReplacePlan) (operation.Step, error) {
	st := operation.Step{
		Kind: operation.StepSearch, App: app, MediaID: plan.MediaID, Season: plan.SeasonNumber,
	}
	switch {
	case plan.MediaType == client.ReplaceMediaMovie:
		st.Command = client.CommandMoviesSearch
	case plan.Scope == client.ReplaceScopeEpisode:
		st.Command, st.EpisodeIDs = client.CommandEpisodeSearch, []int{plan.EpisodeID}
	case plan.Scope == client.ReplaceScopeSeason:
		st.Command = client.CommandSeasonSearch
	case plan.Scope == client.ReplaceScopeSeries:
		st.Command = client.CommandSeriesSearch
	default:
		return st, fmt.Errorf("arr replace: cannot determine search command for media_type=%s scope=%s",
			plan.MediaType, plan.Scope)
	}
	return st, nil
}

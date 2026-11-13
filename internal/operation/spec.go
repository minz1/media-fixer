package operation

import (
	"errors"
	"fmt"
	"slices"
)

func validateSpec(sub Subject, kind Kind, steps []Step) error {
	if err := checkSubject(sub, kind); err != nil {
		return err
	}
	if len(steps) < 1 || len(steps) > MaxSteps() {
		return fmt.Errorf("O2: %d steps", len(steps))
	}
	return checkShape(kind, steps)
}

func checkShape(kind Kind, steps []Step) error {
	if err := checkOrder(kind, steps); err != nil {
		return err
	}
	for j, st := range steps {
		if err := checkFields(st); err != nil {
			return fmt.Errorf("O8: step %d: %w", j, err)
		}
	}
	return nil
}

func shapeOf(kind Kind) []StepKind {
	switch kind {
	case KindRegrab:
		return []StepKind{StepQueueRemove, StepSearch}
	case KindReplace:
	}
	return nil
}

const (
	rankBlocklist = iota
	rankDelete
	rankSearch
)

func replaceRank(k StepKind) int {
	switch k {
	case StepBlocklist:
		return rankBlocklist
	case StepDeleteFile:
		return rankDelete
	case StepSearch:
		return rankSearch
	case StepQueueRemove:
	}
	return -1
}

func checkOrder(kind Kind, steps []Step) error {
	if kind != KindReplace {
		got := make([]StepKind, len(steps))
		for i, st := range steps {
			got[i] = st.Kind
		}
		if !slices.Equal(got, shapeOf(kind)) {
			return fmt.Errorf("O8: %s steps are %v", kind, got)
		}
		return nil
	}
	prev := rankBlocklist
	for j, st := range steps {
		rank := replaceRank(st.Kind)
		if rank < prev || (rank == rankSearch) != (j == len(steps)-1) {
			return fmt.Errorf("O8: replace step %d is %s", j, st.Kind)
		}
		prev = rank
	}
	return nil
}

func checkFields(st Step) error {
	switch st.Kind {
	case StepBlocklist:
		return need(st.HistoryID > 0, "history id")
	case StepDeleteFile:
		return need(st.FileID > 0 && st.Path != "" && st.MediaID > 0, "file id, path and media id")
	case StepSearch:
		return checkSearch(st)
	case StepQueueRemove:
		return need(
			st.QueueID > 0 && st.DownloadID != "" && st.MediaID > 0 && (st.App == "sonarr" || st.App == "radarr"),
			"queue id, download id, media id and app",
		)
	}
	return fmt.Errorf("unknown step kind %q", st.Kind)
}

func checkSearch(st Step) error {
	switch st.Command {
	case "EpisodeSearch":
		if len(st.EpisodeIDs) == 0 {
			return errors.New("search needs episode ids")
		}
	case "SeasonSearch", "SeriesSearch", "MoviesSearch":
	default:
		return fmt.Errorf("unknown search command %q", st.Command)
	}
	return need(st.MediaID > 0, "media id")
}

func need(ok bool, what string) error {
	if !ok {
		return fmt.Errorf("missing %s", what)
	}
	return nil
}

package opexec

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
)

const (
	appJellyfin         = "jellyfin"
	defaultPollInterval = time.Second
	commandCompleted    = "completed"
	commandFailed       = "failed"
)

type Jellyfin interface {
	MediaUpdated(ctx context.Context, paths []string) error
}

var errStepRefused = errors.New("step refused")

type stepNoteError struct {
	state operation.StepState
	note  string
}

func (e *stepNoteError) Error() string { return e.note }

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errStepRefused, fmt.Sprintf(format, args...))
}

func unknownNote(format string, args ...any) error {
	return &stepNoteError{state: operation.ResultUnknown, note: fmt.Sprintf(format, args...)}
}

func (x *Executor) callJellyfin(ctx context.Context, st operation.Step) error {
	if x.Ports.Jellyfin == nil {
		return fmt.Errorf("%w: jellyfin", errNoPort)
	}
	return x.Ports.Jellyfin.MediaUpdated(ctx, []string{filepath.Dir(st.Path)})
}

func (x *Executor) pollInterval() time.Duration {
	if x.PollInterval > 0 {
		return x.PollInterval
	}
	return defaultPollInterval
}

func episodeOf(eps []client.Episode, id int) (client.Episode, bool) {
	for _, e := range eps {
		if e.ID == id {
			return e, true
		}
	}
	return client.Episode{}, false
}

func (x *Executor) importStep(ctx context.Context, a Arr, st operation.Step) error {
	episodeID := st.EpisodeIDs[0]
	label := fmt.Sprintf("S%02dE%02d", st.Season, st.Episode)
	eps, err := a.GetEpisodes(ctx, st.MediaID, st.Season)
	if err != nil {
		return refused("read %s: %v", label, err)
	}
	if ep, ok := episodeOf(eps, episodeID); !ok || ep.EpisodeFileID != st.FileID {
		return refused("%s has a different file since approval", label)
	}
	queue, err := a.QueueEntries(ctx)
	if err != nil {
		return refused("read the queue: %v", err)
	}
	for _, q := range queue {
		if q.EpisodeID == episodeID {
			return refused("another download is queued for %s", label)
		}
	}
	pending, err := a.ManualImportPending(ctx, []string{st.Path})
	if err != nil {
		return refused("read pending imports: %v", err)
	}
	if pending {
		return refused("%s is already being imported", filepath.Base(st.Path))
	}
	cands, err := a.ManualImportFolderCandidates(ctx, filepath.Dir(st.Path))
	if err != nil {
		return refused("preview the folder: %v", err)
	}
	cand, ok := freshCandidate(cands, st)
	if !ok {
		return refused("the candidate changed; preview again")
	}
	cand.DownloadID = ""
	cmdID, err := a.ManualImport(ctx, []client.ImportCandidate{cand})
	if err != nil {
		return err
	}
	if waitErr := x.awaitCommand(ctx, a, cmdID); waitErr != nil {
		return unknownNote("import command %d: %v", cmdID, waitErr)
	}
	return importVerdict(ctx, a, st, label)
}

func freshCandidate(cands []client.ImportCandidate, st operation.Step) (client.ImportCandidate, bool) {
	for _, c := range cands {
		if c.Path == st.Path && c.Size == st.Size && c.SeriesID == st.MediaID && c.SeasonNumber == st.Season &&
			len(c.Episodes) == 1 && c.Episodes[0].ID == st.EpisodeIDs[0] {
			return c, true
		}
	}
	return client.ImportCandidate{}, false
}

func (x *Executor) awaitCommand(ctx context.Context, a Arr, id int) error {
	tick := time.NewTicker(x.pollInterval())
	defer tick.Stop()
	for {
		status, err := a.Command(ctx, id)
		if err != nil {
			return err
		}
		switch status {
		case commandCompleted, commandFailed, "aborted", "cancelled", "orphaned":
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func importVerdict(ctx context.Context, a Arr, st operation.Step, label string) error {
	eps, err := a.GetEpisodes(ctx, st.MediaID, st.Season)
	if err != nil {
		return unknownNote("read %s after the import: %v", label, err)
	}
	ep, ok := episodeOf(eps, st.EpisodeIDs[0])
	if !ok {
		return unknownNote("%s is not listed after the import", label)
	}
	if ep.EpisodeFileID == 0 {
		return unknownNote("fileless: %s has no file after the import; the old file is gone", label)
	}
	if ep.EpisodeFileID == st.FileID {
		return fmt.Errorf("%w: the import did not replace the file of %s", errStepRefused, label)
	}
	files, err := a.GetEpisodeFiles(ctx, st.MediaID)
	if err != nil {
		return unknownNote("read the files of %s after the import: %v", label, err)
	}
	for _, f := range files {
		if f.ID != ep.EpisodeFileID {
			continue
		}
		if f.Size == st.Size {
			return &stepNoteError{state: operation.ResultDone, note: "imported as " + st.Path}
		}
		break
	}
	return unknownNote("%s has a new file that is not the candidate", label)
}

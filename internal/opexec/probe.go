package opexec

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
)

func (x *Executor) Probe(ctx context.Context, op operation.Op) ([]operation.Probe, error) {
	var out []operation.Probe
	for _, i := range operation.Remaining(op) {
		p, err := x.probeStep(ctx, op, i)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func (x *Executor) probeStep(ctx context.Context, op operation.Op, i int) (operation.Probe, error) {
	pctx, cancel := x.stepContext(ctx)
	defer cancel()
	st := op.Steps[i]
	var p operation.Probe
	var err error
	switch st.Kind {
	case operation.StepTorrentDelete, operation.StepTorrentAdd:
		p, err = x.probeTorrent(pctx, st)
	case operation.StepBlocklist, operation.StepDeleteFile, operation.StepSearch, operation.StepQueueRemove:
		p, err = x.probeArr(pctx, op, st)
	default:
		err = fmt.Errorf("unknown step kind %q", st.Kind)
	}
	p.Index = i
	return p, err
}

func (x *Executor) probeArr(ctx context.Context, op operation.Op, st operation.Step) (operation.Probe, error) {
	a, err := x.arrFor(st.App)
	if err != nil {
		return operation.Probe{}, err
	}
	switch st.Kind {
	case operation.StepBlocklist:
		return probeBlocklist(ctx, a, st)
	case operation.StepDeleteFile:
		return probeDelete(ctx, a, st)
	case operation.StepSearch:
		return probeSearch(ctx, a, st)
	case operation.StepQueueRemove:
		return probeQueue(ctx, a, op, st)
	case operation.StepTorrentDelete, operation.StepTorrentAdd:
	}
	return operation.Probe{}, fmt.Errorf("unknown step kind %q", st.Kind)
}

func (x *Executor) probeTorrent(ctx context.Context, st operation.Step) (operation.Probe, error) {
	if x.Ports.Decypharr == nil {
		return operation.Probe{}, fmt.Errorf("%w: decypharr", errNoPort)
	}
	present, err := x.Ports.Decypharr.TorrentByHash(ctx, st.Name, st.InfoHash)
	if err != nil {
		return operation.Probe{}, err
	}
	switch {
	case st.Kind == operation.StepTorrentAdd && present:
		return operation.Probe{Verdict: operation.VerdictDone, Note: "already re-added"}, nil
	case st.Kind == operation.StepTorrentDelete && !present:
		return operation.Probe{Verdict: operation.VerdictDone, Note: "the torrent is gone"}, nil
	}
	return operation.Probe{Verdict: operation.VerdictTodo}, nil
}

func mediaType(st operation.Step) string {
	if isMovie(st) {
		return client.ReplaceMediaMovie
	}
	return client.ReplaceMediaTV
}

func probeBlocklist(ctx context.Context, a Arr, st operation.Step) (operation.Probe, error) {
	if st.DownloadID == "" {
		note := fmt.Sprintf("grab %d has no download id to check; Rerun instead", st.HistoryID)
		return operation.Probe{Verdict: operation.VerdictConflict, Note: note}, nil
	}
	found, err := a.FailedForDownload(ctx, mediaType(st), st.MediaID, st.DownloadID, st.GrabbedAt)
	if err != nil {
		return operation.Probe{}, err
	}
	if found {
		return operation.Probe{Verdict: operation.VerdictDone, Note: "found in history"}, nil
	}
	return operation.Probe{Verdict: operation.VerdictTodo}, nil
}

func probeDelete(ctx context.Context, a Arr, st operation.Step) (operation.Probe, error) {
	f, present, err := lookupFile(ctx, a, st)
	switch {
	case errors.Is(err, client.ErrNotFound):
		return operation.Probe{Verdict: operation.VerdictConflict, Note: "the media is gone"}, nil
	case err != nil:
		return operation.Probe{}, err
	case !present:
		return operation.Probe{Verdict: operation.VerdictDone, Note: "file already deleted"}, nil
	case f.path != st.Path || (st.Size != 0 && f.size != st.Size):
		note := fmt.Sprintf("file %d changed since approval", st.FileID)
		return operation.Probe{Verdict: operation.VerdictConflict, Note: note}, nil
	}
	return operation.Probe{Verdict: operation.VerdictTodo}, nil
}

func probeSearch(ctx context.Context, a Arr, st operation.Step) (operation.Probe, error) {
	exists, err := mediaExists(ctx, a, st)
	if err != nil {
		return operation.Probe{}, err
	}
	if !exists {
		return operation.Probe{Verdict: operation.VerdictConflict, Note: "the media is gone"}, nil
	}
	return operation.Probe{Verdict: operation.VerdictTodo}, nil
}

func mediaExists(ctx context.Context, a Arr, st operation.Step) (bool, error) {
	if !isMovie(st) {
		_, err := a.GetSeries(ctx, st.MediaID)
		if errors.Is(err, client.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	movies, err := a.ListMovies(ctx)
	if err != nil {
		return false, err
	}
	for _, m := range movies {
		if m.ID == st.MediaID {
			return true, nil
		}
	}
	return false, nil
}

func probeQueue(ctx context.Context, a Arr, op operation.Op, st operation.Step) (operation.Probe, error) {
	entries, err := a.QueueEntries(ctx)
	if err != nil {
		return operation.Probe{}, err
	}
	for _, e := range entries {
		if strings.EqualFold(e.DownloadID, st.DownloadID) {
			return operation.Probe{Verdict: operation.VerdictTodo, QueueID: e.ID}, nil
		}
	}
	found, err := a.FailedForDownload(ctx, mediaType(st), st.MediaID, st.DownloadID, op.CreatedAt)
	if err != nil {
		return operation.Probe{}, err
	}
	if found {
		return operation.Probe{Verdict: operation.VerdictDone, Note: "removed and blocklisted"}, nil
	}
	return operation.Probe{
		Verdict: operation.VerdictConflict, Note: "the download left the queue without a blocklist record",
	}, nil
}

type fileInfo struct {
	id   int
	path string
	size int64
}

func lookupFile(ctx context.Context, a Arr, st operation.Step) (fileInfo, bool, error) {
	var all []fileInfo
	if isMovie(st) {
		files, err := a.GetMovieFiles(ctx, st.MediaID)
		if err != nil {
			return fileInfo{}, false, err
		}
		for _, f := range files {
			all = append(all, fileInfo{id: f.ID, path: f.Path, size: f.Size})
		}
	} else {
		files, err := a.GetEpisodeFiles(ctx, st.MediaID)
		if err != nil {
			return fileInfo{}, false, err
		}
		for _, f := range files {
			all = append(all, fileInfo{id: f.ID, path: f.Path, size: f.Size})
		}
	}
	for _, f := range all {
		if f.id == st.FileID {
			return f, true, nil
		}
	}
	return fileInfo{}, false, nil
}

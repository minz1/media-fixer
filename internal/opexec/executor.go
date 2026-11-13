package opexec

import (
	"context"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
)

const (
	appRadarr = "radarr"

	writeTimeout       = 5 * time.Second
	defaultStepTimeout = 2 * time.Minute
)

type Store interface {
	ApplyOperation(
		ctx context.Context,
		id int64,
		decide func(operation.Op) (operation.Op, error),
	) (operation.Op, error)
}

type Arr interface {
	MarkHistoryFailed(ctx context.Context, historyID int) error
	DeleteEpisodeFile(ctx context.Context, fileID int) error
	DeleteMovieFile(ctx context.Context, fileID int) error
	Search(ctx context.Context, command string, mediaID, season int, episodeIDs []int) error
	RemoveQueueItem(ctx context.Context, id int, removeFromClient, blocklist, skipRedownload bool) error
	GetEpisodeFiles(ctx context.Context, seriesID int) ([]client.EpisodeFile, error)
	GetMovieFiles(ctx context.Context, movieID int) ([]client.MovieFile, error)
	GetSeries(ctx context.Context, id int) (client.Series, error)
	ListMovies(ctx context.Context) ([]client.Movie, error)
	QueueEntries(ctx context.Context) ([]client.QueueEntry, error)
	FailedForDownload(
		ctx context.Context,
		mediaType string,
		mediaID int,
		downloadID string,
		since time.Time,
	) (bool, error)
}

type Decypharr interface {
	DeleteTorrent(ctx context.Context, category, hash string, removeFromDebrid bool) error
	AddTorrent(ctx context.Context, magnet, category string) error
	TorrentByHash(ctx context.Context, hash string) (bool, error)
}

type Ports struct {
	Arr       func(app string) Arr
	Decypharr Decypharr
}

type Executor struct {
	Store       Store
	Ports       Ports
	Now         func() time.Time
	StepTimeout time.Duration
}

func (x *Executor) Run(ctx context.Context, op operation.Op) operation.Op {
	for op.Status == operation.Running {
		next, ok := x.runStep(ctx, op)
		op = next
		if !ok {
			break
		}
	}
	return op
}

func (x *Executor) runStep(ctx context.Context, op operation.Op) (operation.Op, bool) {
	i := op.Index
	begun, err := x.write(ctx, op, operation.Event{Kind: operation.StepStarted, Index: i})
	if err != nil {
		return op, false
	}
	out := x.classify(ctx, begun, i, x.call(ctx, begun, i))
	finished, err := x.write(ctx, begun, operation.Event{
		Kind: operation.StepFinished, Index: i, Outcome: out.state, Note: out.note, Err: out.errText,
	})
	if err != nil {
		return begun, false
	}
	return finished, true
}

func (x *Executor) write(ctx context.Context, op operation.Op, ev operation.Event) (operation.Op, error) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	ev.At = x.now()
	return x.Store.ApplyOperation(wctx, op.ID, func(cur operation.Op) (operation.Op, error) {
		return operation.Next(cur, ev)
	})
}

func (x *Executor) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

func (x *Executor) stepContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := x.StepTimeout
	if timeout <= 0 {
		timeout = defaultStepTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (x *Executor) arrFor(app string) (Arr, error) {
	if x.Ports.Arr != nil {
		if a := x.Ports.Arr(app); a != nil {
			return a, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", errNoPort, app)
}

func (x *Executor) call(ctx context.Context, op operation.Op, i int) error {
	cctx, cancel := x.stepContext(ctx)
	defer cancel()
	st := op.Steps[i]
	if st.Kind == operation.StepTorrentDelete || st.Kind == operation.StepTorrentAdd {
		return x.callDecypharr(cctx, st)
	}
	if st.Kind == operation.StepSubtitleSync {
		return fmt.Errorf("step kind %s not supported yet", st.Kind)
	}
	a, err := x.arrFor(st.App)
	if err != nil {
		return err
	}
	return callArr(cctx, a, st, op.Results[i])
}

func callArr(ctx context.Context, a Arr, st operation.Step, res operation.Result) error {
	switch st.Kind {
	case operation.StepBlocklist:
		return a.MarkHistoryFailed(ctx, st.HistoryID)
	case operation.StepDeleteFile:
		if isMovie(st) {
			return a.DeleteMovieFile(ctx, st.FileID)
		}
		return a.DeleteEpisodeFile(ctx, st.FileID)
	case operation.StepSearch:
		return a.Search(ctx, st.Command, st.MediaID, st.Season, st.EpisodeIDs)
	case operation.StepQueueRemove:
		id := st.QueueID
		if res.QueueID > 0 {
			id = res.QueueID
		}
		return a.RemoveQueueItem(ctx, id, false, true, true)
	case operation.StepTorrentDelete, operation.StepTorrentAdd, operation.StepSubtitleSync:
	}
	return fmt.Errorf("unknown step kind %q", st.Kind)
}

func (x *Executor) callDecypharr(ctx context.Context, st operation.Step) error {
	d := x.Ports.Decypharr
	if d == nil {
		return fmt.Errorf("%w: decypharr", errNoPort)
	}
	if st.Kind == operation.StepTorrentDelete {
		return d.DeleteTorrent(ctx, st.Category, st.InfoHash, true)
	}
	return d.AddTorrent(ctx, st.Magnet, st.Category)
}

func isMovie(st operation.Step) bool { return st.App == appRadarr }

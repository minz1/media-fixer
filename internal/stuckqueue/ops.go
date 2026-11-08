package stuckqueue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

const (
	opSettleBound  = 5 * time.Second
	changedRefusal = "this changed, look again"
	noSeason       = -1
)

var (
	errOpUnsettled = errors.New("the action could not be recorded: database error")
	errOpStopped   = errors.New("the action stopped")
)

type RegrabResult struct {
	Op        operation.Op
	Resumable bool
}

func OpSubject(app App, downloadID string) operation.Subject {
	return operation.Subject{Kind: operation.SubjectDownload, ID: appCode(app) + ":" + strings.ToLower(downloadID)}
}

func (s *Scheduler) executor() *opexec.Executor {
	return &opexec.Executor{
		Store: s.store,
		Ports: opexec.Ports{Arr: func(app string) opexec.Arr {
			if c := s.r.client(App(app)); c != nil {
				return c
			}
			return nil
		}},
		Now: s.r.Now,
	}
}

func regrabSpec(d Download) operation.Spec {
	mediaID, search := d.Records[0].SeriesID, operation.Step{
		Kind: operation.StepSearch, App: string(d.App), Command: client.CommandEpisodeSearch, Season: noSeason,
	}
	if d.App == AppRadarr {
		mediaID, search.Command = d.Records[0].MovieID, client.CommandMoviesSearch
	} else {
		for _, r := range d.Records {
			search.EpisodeIDs = append(search.EpisodeIDs, r.EpisodeID)
		}
	}
	search.MediaID = mediaID
	return operation.Spec{
		Subject: OpSubject(d.App, d.DownloadID), Kind: operation.KindRegrab, Title: d.Title, Source: []byte("{}"),
		Steps: []operation.Step{{
			Kind: operation.StepQueueRemove, App: string(d.App), QueueID: d.Records[0].ID,
			DownloadID: d.DownloadID, MediaID: mediaID, Season: noSeason,
		}, search},
	}
}

func (s *Scheduler) createOp(ctx context.Context, spec operation.Spec) (operation.Op, error) {
	op, err := s.store.ApplyOperation(ctx, 0, func(operation.Op) (operation.Op, error) {
		return operation.Next(operation.Op{}, operation.Event{Kind: operation.Created, At: s.r.Now(), Spec: &spec})
	})
	switch {
	case err == nil:
		return op, nil
	case strings.Contains(err.Error(), "UNIQUE constraint failed"):
		return operation.Op{}, refuse("an action for this download is running")
	}
	return operation.Op{}, err
}

func (s *Scheduler) runRegrab(ctx context.Context, _ *client.ArrClient, d Download) (RegrabResult, error) {
	op, err := s.createOp(ctx, regrabSpec(d))
	if err != nil {
		return RegrabResult{}, err
	}
	op = s.runOp(ctx, op)
	return RegrabResult{Op: op, Resumable: operation.Resumable(op)}, opError(op)
}

func (s *Scheduler) runOp(ctx context.Context, op operation.Op) operation.Op {
	op = s.executor().Run(ctx, op)
	if op.Status == operation.Running {
		return s.interruptOp(ctx, op)
	}
	return op
}

func (s *Scheduler) interruptOp(ctx context.Context, op operation.Op) operation.Op {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opSettleBound)
	defer cancel()
	stopped, err := s.store.ApplyOperation(wctx, op.ID, func(cur operation.Op) (operation.Op, error) {
		return operation.Next(cur, operation.Event{
			Kind: operation.Interrupted, At: s.r.Now(), Cause: operation.CauseStore,
		})
	})
	if err != nil {
		s.r.logger().ErrorContext(ctx, "stuck-queue: interrupt operation after a store failure",
			"op", op.ID, "error", err)
		return op
	}
	return stopped
}

func opError(op operation.Op) error {
	switch op.Status {
	case operation.Done:
		return nil
	case operation.Running:
		return errOpUnsettled
	case operation.None, operation.FailedAtStep:
	}
	if op.Index < 0 || op.Index >= len(op.Results) {
		return errOpStopped
	}
	r := op.Results[op.Index]
	text := cmp.Or(r.Err, r.Note)
	switch {
	case text == "":
		return errOpStopped
	case op.Kind == operation.KindRegrab && op.Index == 1:
		return fmt.Errorf("%w: %s", errRegrabSearch, text)
	}
	return errors.New(text)
}

func (s *Scheduler) resumeOp(
	ctx context.Context, app App, downloadID string, opID, opVersion int64,
) (operation.Op, error) {
	op, err := s.d.Operation(ctx, opID)
	if errors.Is(err, db.ErrNotFound) {
		return operation.Op{}, refuse(changedRefusal)
	}
	if err != nil {
		return operation.Op{}, err
	}
	if op.Subject != OpSubject(app, downloadID) || op.Version != opVersion || !operation.Resumable(op) {
		return operation.Op{}, refuse(changedRefusal)
	}
	x := s.executor()
	probes, err := x.Probe(ctx, op)
	if err != nil {
		return operation.Op{}, fmt.Errorf("couldn't check the live state: %w; try again", err)
	}
	op, err = s.store.ApplyOperation(ctx, op.ID, func(cur operation.Op) (operation.Op, error) {
		return operation.Next(cur, operation.Event{
			Kind: operation.OwnerContinued, At: s.r.Now(), Version: opVersion, Probes: probes,
		})
	})
	if err != nil {
		return operation.Op{}, refuseOp(err)
	}
	return s.runOp(ctx, op), nil
}

func opSettled(op operation.Op) bool {
	return op.Status != operation.Running && !operation.Resumable(op)
}

func refuseOp(err error) error {
	rej, ok := errors.AsType[*operation.RejectError](err)
	switch {
	case !ok:
		return err
	case rej.Code == operation.CodeConflict && rej.Note != "":
		return refuse("%s", rej.Note)
	case rej.Code == operation.CodeChanged, rej.Code == operation.CodeProbesStale,
		rej.Code == operation.CodeNotResumable:
		return refuse(changedRefusal)
	}
	return err
}

func (s *Scheduler) Resume(
	ctx context.Context, app App, downloadID string, opID, opVersion int64,
) (string, error) {
	release, err := s.claim(holderOwner, s.r.Now())
	if err != nil {
		return "", err
	}
	var u update
	var subject string
	defer func() { s.refreshSubject(subject) }()
	defer func() { release(u) }()
	key := Key{App: app, DownloadID: downloadID}
	snap, minAge, err := s.guard(ctx, s.r.client(app), key, OwnerResume)
	if err != nil {
		return "", err
	}
	if snap.Acted == nil || snap.Acted.OpID != opID {
		return "", refuse(changedRefusal)
	}
	op, err := s.resumeOp(ctx, app, downloadID, opID, opVersion)
	if err != nil {
		return "", err
	}
	u = s.record(ctx, key, Event{Kind: EvResumed, OpID: op.ID, Done: opSettled(op)}, minAge)
	subject = card.StuckSubject(appCode(app), op.Steps[0].DownloadID)
	if err = opError(op); err != nil {
		return "", err
	}
	s.r.logger().InfoContext(ctx, "stuck-queue: resumed re-grab", "app", app, "download", downloadID, "op", op.ID)
	return "Search sent for " + op.Title + ".", nil
}

package stuckqueue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
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
		return fmt.Errorf("%w: %s", errSearchFailed, text)
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
	var subject string
	defer func() { s.refreshSubject(subject) }()
	defer func() { release(nil) }()
	op, err := s.resumeOp(ctx, app, downloadID, opID, opVersion)
	if err != nil {
		return "", err
	}
	subject = card.StuckSubject(appCode(app), op.Steps[0].DownloadID)
	s.trackOp(op)
	if err = opError(op); err != nil {
		return "", err
	}
	s.recordOutcome(subject, ActionRegrab)
	s.r.logger().InfoContext(ctx, "stuck-queue: resumed re-grab", "app", app, "download", downloadID, "op", op.ID)
	return "Search sent for " + op.Title + ".", nil
}

func (s *Scheduler) trackOp(op operation.Op) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if operation.Resumable(op) {
		if s.slot.failedOps == nil {
			s.slot.failedOps = map[string]operation.Op{}
		}
		s.slot.failedOps[op.Subject.ID] = op
		return
	}
	delete(s.slot.failedOps, op.Subject.ID)
}

func (s *Scheduler) LoadOperations(ctx context.Context) error {
	ops, err := s.d.ResumableDownloadOperations(ctx)
	if err != nil {
		return err
	}
	loaded := make(map[string]operation.Op, len(ops))
	for _, op := range ops {
		loaded[op.Subject.ID] = op
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slot.failedOps = loaded
	return nil
}

func (s *Scheduler) FailedOperations() []operation.Op {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]operation.Op, 0, len(s.slot.failedOps))
	for _, op := range s.slot.failedOps {
		out = append(out, op)
	}
	slices.SortFunc(out, func(a, b operation.Op) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

package opexec

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
)

var errNoPort = errors.New("no client configured")

type outcome struct {
	state   operation.StepState
	note    string
	errText string
}

func (x *Executor) classify(ctx context.Context, op operation.Op, i int, err error) outcome {
	if err == nil {
		return outcome{state: operation.ResultDone}
	}
	st := op.Steps[i]
	switch st.Kind {
	case operation.StepQueueRemove:
		if isNotFound(err) {
			return unknownOutcome(err)
		}
	case operation.StepDeleteFile:
		if errors.Is(err, client.ErrNotFound) {
			return x.confirmGone(ctx, st, err)
		}
	case operation.StepBlocklist, operation.StepSearch:
	}
	return byError(err)
}

func (x *Executor) confirmGone(ctx context.Context, st operation.Step, err error) outcome {
	cctx, cancel := x.stepContext(ctx)
	defer cancel()
	a, aerr := x.arrFor(st.App)
	if aerr != nil {
		return unknownOutcome(err)
	}
	_, present, lerr := lookupFile(cctx, a, st)
	switch {
	case lerr != nil:
		return outcome{
			state: operation.ResultUnknown, errText: err.Error(),
			note: fmt.Sprintf("could not confirm the file is gone: %v", lerr),
		}
	case present:
		return outcome{state: operation.ResultFailed, errText: err.Error()}
	}
	return outcome{state: operation.ResultDone, note: "already gone"}
}

func unknownOutcome(err error) outcome {
	return outcome{state: operation.ResultUnknown, errText: err.Error()}
}

func isNotFound(err error) bool {
	if errors.Is(err, client.ErrNotFound) {
		return true
	}
	se, ok := errors.AsType[*client.HTTPStatusError](err)
	return ok && se.Status == http.StatusNotFound
}

func byError(err error) outcome {
	if errors.Is(err, errNoPort) {
		return outcome{state: operation.ResultFailed, errText: err.Error()}
	}
	se, ok := errors.AsType[*client.HTTPStatusError](err)
	if !ok || retryable(se.Status) || se.Status < http.StatusBadRequest {
		return unknownOutcome(err)
	}
	return outcome{state: operation.ResultFailed, errText: err.Error()}
}

func retryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return status >= http.StatusInternalServerError
}

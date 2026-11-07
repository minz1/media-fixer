package operation

import (
	"errors"
	"fmt"
)

type Code string

const (
	CodeBadSpec      Code = "bad_spec"
	CodeNoOp         Code = "no_op"
	CodeExists       Code = "exists"
	CodeOutOfOrder   Code = "out_of_order"
	CodeBadCause     Code = "bad_cause"
	CodeRunning      Code = "running"
	CodeFinished     Code = "finished"
	CodeNotRunning   Code = "not_running"
	CodeChanged      Code = "changed"
	CodeNotResumable Code = "not_resumable"
	CodeProbesStale  Code = "probes_stale"
	CodeConflict     Code = "conflict"
)

var ErrIllegal = errors.New("operation: illegal op")

type RejectError struct {
	Code Code
	Note string
}

func (r *RejectError) Error() string {
	if r.Note == "" {
		return fmt.Sprintf("operation: rejected: %s", r.Code)
	}
	return fmt.Sprintf("operation: rejected: %s: %s", r.Code, r.Note)
}

func CodeOf(err error) Code {
	if r, ok := errors.AsType[*RejectError](err); ok {
		return r.Code
	}
	return ""
}

func reject(c Code, note string) error { return &RejectError{Code: c, Note: note} }

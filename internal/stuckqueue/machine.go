package stuckqueue

import (
	"fmt"
	"strings"
	"time"
)

const (
	Grace = 10 * time.Minute
	Keep  = 16 * 24 * time.Hour
)

type State string

const (
	StateUntracked      State = "untracked"
	StateWaiting        State = "waiting"
	StateDue            State = "due"
	StateHeld           State = "held"
	StateNotified       State = "notified"
	StateImportInflight State = "import_inflight"
	StateActed          State = "acted"
	StateCleared        State = "cleared"
)

func States() []State {
	return []State{
		StateUntracked, StateWaiting, StateDue, StateHeld, StateNotified,
		StateImportInflight, StateActed, StateCleared,
	}
}

type Key struct {
	App        App
	DownloadID string
}

type Offer string

const (
	OfferNone         Offer = ""
	OfferRemove       Offer = "remove"
	OfferRegrabRemove Offer = "regrab remove"
	OfferAll          Offer = "import regrab remove"
)

func Offers() []Offer { return []Offer{OfferNone, OfferRemove, OfferRegrabRemove, OfferAll} }

func (o Offer) Actions() []Action {
	fields := strings.Fields(string(o))
	out := make([]Action, 0, len(fields))
	for _, f := range fields {
		out = append(out, Action(f))
	}
	return out
}

type Files struct{ N, M int }

type ActResult string

const (
	ResultOK                ActResult = "ok"
	ResultUnfinished        ActResult = "unfinished"
	ResultImportFailed      ActResult = "import_failed"
	ResultImportTimeout     ActResult = "import_timeout"
	ResultImportUnconfirmed ActResult = "import_unconfirmed"
	ResultPartial           ActResult = "partial"
)

func ActResults() []ActResult {
	return []ActResult{
		ResultOK, ResultUnfinished, ResultImportFailed, ResultImportTimeout, ResultImportUnconfirmed, ResultPartial,
	}
}

type Acted struct {
	Action Action
	At     time.Time
	Result ActResult
	Files  Files
	OpID   int64
}

type ImportCmd struct {
	CommandID int
	StartedAt time.Time
	Files     Files
}

type Snapshot struct {
	Key        Key
	State      State
	FirstSeen  time.Time
	NotifiedAt time.Time
	Misses     int
	TargetsFP  string
	Offered    Offer
	Acted      *Acted
	Import     *ImportCmd
}

type CmdStatus string

const (
	CmdRunning    CmdStatus = "running"
	CmdCompleted  CmdStatus = "completed"
	CmdFailed     CmdStatus = "failed"
	CmdGone       CmdStatus = "gone"
	CmdUnreadable CmdStatus = "unreadable"
)

func CmdStatuses() []CmdStatus {
	return []CmdStatus{CmdRunning, CmdCompleted, CmdFailed, CmdGone, CmdUnreadable}
}

type EventKind string

const (
	EvSeen            EventKind = "seen"
	EvMissed          EventKind = "missed"
	EvClaimed         EventKind = "claimed"
	EvNotifyFailed    EventKind = "notify_failed"
	EvClearDone       EventKind = "clear_done"
	EvClearFailed     EventKind = "clear_failed"
	EvActed           EventKind = "acted"
	EvImportPosted    EventKind = "import_posted"
	EvCommandObserved EventKind = "command_observed"
	EvExpire          EventKind = "expire"
	EvAdopted         EventKind = "adopted"
	EvResumed         EventKind = "resumed"
)

func EventKinds() []EventKind {
	return []EventKind{
		EvSeen, EvMissed, EvClaimed, EvNotifyFailed, EvClearDone, EvClearFailed,
		EvActed, EvImportPosted, EvCommandObserved, EvExpire, EvAdopted, EvResumed,
	}
}

type Observation struct {
	FP      string
	Verdict Verdict
	Offered Offer
}

type Event struct {
	Kind      EventKind
	At        time.Time
	Obs       Observation
	MinAge    time.Duration
	Hold      bool
	Action    Action
	Resumable bool
	OpID      int64
	Done      bool
	CommandID int
	Mapped    bool
	Files     Files
	Cmd       CmdStatus
	Timeout   time.Duration
	ClaimAt   time.Time
}

type Effect interface{ isEffect() }

type EffClearDownload struct{ Key Key }

type EffRefreshCard struct{ Key Key }

func (EffClearDownload) isEffect() {}
func (EffRefreshCard) isEffect()   {}

type Code string

const (
	CodeUntracked     Code = "untracked"
	CodeNotDue        Code = "not_due"
	CodeStaleClaim    Code = "stale_claim"
	CodeNotNotified   Code = "not_notified"
	CodeNotClearing   Code = "not_clearing"
	CodeNoOp          Code = "no_op"
	CodeInsideWait    Code = "inside_wait"
	CodeNoCommand     Code = "no_command"
	CodeNoImport      Code = "no_import"
	CodeNotActed      Code = "not_acted"
	CodeImportRunning Code = "import_running"
	CodeNotExpired    Code = "not_expired"
	CodeAlreadyDone   Code = "already_done"
	CodeAdopted       Code = "adopted"
	CodeOpChanged     Code = "op_changed"
	CodeNotResumable  Code = "not_resumable"
)

func Codes() []Code {
	return []Code{
		CodeUntracked, CodeNotDue, CodeStaleClaim, CodeNotNotified, CodeNotClearing, CodeNoOp, CodeInsideWait,
		CodeNoCommand, CodeNoImport, CodeNotActed, CodeImportRunning, CodeNotExpired, CodeAlreadyDone,
		CodeAdopted, CodeOpChanged, CodeNotResumable,
	}
}

type RejectError struct {
	Code Code
	Note string
}

func (r *RejectError) Error() string {
	if r.Note == "" {
		return fmt.Sprintf("stuckqueue: rejected: %s", r.Code)
	}
	return fmt.Sprintf("stuckqueue: rejected: %s: %s", r.Code, r.Note)
}

func reject(c Code, note string) error { return &RejectError{Code: c, Note: note} }

type OwnerAction string

const (
	OwnerRemove       OwnerAction = "remove"
	OwnerRegrab       OwnerAction = "regrab"
	OwnerImport       OwnerAction = "import"
	OwnerMappedImport OwnerAction = "mapped_import"
	OwnerResume       OwnerAction = "resume"
)

func OwnerActions() []OwnerAction {
	return []OwnerAction{OwnerRemove, OwnerRegrab, OwnerImport, OwnerMappedImport, OwnerResume}
}

type Tone string

const (
	ToneNeutral Tone = "neutral"
	ToneOK      Tone = "ok"
	ToneWarn    Tone = "warn"
)

type Driver string

const (
	DriverRun          Driver = "run"
	DriverHousekeeping Driver = "housekeeping"
	DriverOwner        Driver = "owner"
)

type StateMeta struct {
	Stored   bool
	Terminal bool
	Label    string
	Tone     Tone
	Drivers  []Driver
}

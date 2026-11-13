package operation

import (
	"encoding/json"
	"time"
)

type Status string

const (
	None         Status = ""
	Running      Status = "running"
	Done         Status = "done"
	FailedAtStep Status = "failed_at_step"
)

func Statuses() []Status { return []Status{Running, Done, FailedAtStep} }

type Kind string

const (
	KindReplace      Kind = "replace"
	KindTorrentReadd Kind = "torrent_readd"
	KindRegrab       Kind = "regrab"
	KindSwapRelease  Kind = "swap_release"

	KindResyncSubtitle Kind = "resync_subtitle"
)

func Kinds() []Kind {
	return []Kind{KindReplace, KindTorrentReadd, KindRegrab, KindSwapRelease, KindResyncSubtitle}
}

type StepKind string

const (
	StepBlocklist      StepKind = "blocklist"
	StepDeleteFile     StepKind = "delete_file"
	StepSearch         StepKind = "search"
	StepTorrentDelete  StepKind = "torrent_delete"
	StepTorrentAdd     StepKind = "torrent_add"
	StepQueueRemove    StepKind = "queue_remove"
	StepImport         StepKind = "import"
	StepJellyfinNotify StepKind = "jellyfin_notify"
	StepSubtitleSync   StepKind = "subtitle_sync"
)

func StepKinds() []StepKind {
	return []StepKind{
		StepBlocklist, StepDeleteFile, StepSearch, StepTorrentDelete, StepTorrentAdd, StepQueueRemove, StepImport,
		StepJellyfinNotify, StepSubtitleSync,
	}
}

type StepState string

const (
	ResultPending StepState = "pending"
	ResultStarted StepState = "started"
	ResultDone    StepState = "done"
	ResultFailed  StepState = "failed"
	ResultUnknown StepState = "unknown"
)

func StepStates() []StepState {
	return []StepState{ResultPending, ResultStarted, ResultDone, ResultFailed, ResultUnknown}
}

type Cause string

const (
	CauseNone    Cause = ""
	CauseError   Cause = "error"
	CauseRestart Cause = "restart"
	CauseStore   Cause = "store"
)

func Causes() []Cause { return []Cause{CauseError, CauseRestart, CauseStore} }

type SubjectKind string

const (
	SubjectIncident SubjectKind = "incident"
	SubjectDownload SubjectKind = "download"
)

func SubjectKinds() []SubjectKind { return []SubjectKind{SubjectIncident, SubjectDownload} }

type Subject struct {
	Kind SubjectKind
	ID   string
}

type Step struct {
	Kind       StepKind  `json:"kind"`
	App        string    `json:"app"`
	HistoryID  int       `json:"history_id,omitempty"`
	DownloadID string    `json:"download_id,omitempty"`
	GrabbedAt  time.Time `json:"grabbed_at,omitzero"`
	FileID     int       `json:"file_id,omitempty"`
	Path       string    `json:"path,omitempty"`
	Size       int64     `json:"size,omitempty"`
	MediaID    int       `json:"media_id,omitempty"`
	Command    string    `json:"command,omitempty"`
	Season     int       `json:"season"`
	EpisodeIDs []int     `json:"episode_ids,omitempty"`
	QueueID    int       `json:"queue_id,omitempty"`
	InfoHash   string    `json:"info_hash,omitempty"`
	Category   string    `json:"category,omitempty"`
	Name       string    `json:"name,omitempty"`
	Magnet     string    `json:"magnet,omitempty"`
	Episode    int       `json:"episode,omitempty"`
	Baseline   int       `json:"baseline,omitempty"`
	HI         bool      `json:"hi,omitempty"`
}

type Result struct {
	State      StepState `json:"state"`
	Note       string    `json:"note,omitempty"`
	Err        string    `json:"error,omitempty"`
	QueueID    int       `json:"queue_id,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

type Op struct {
	ID        int64
	Subject   Subject
	Kind      Kind
	Title     string
	Source    json.RawMessage
	Steps     []Step
	Results   []Result
	Index     int
	Status    Status
	Cause     Cause
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Spec struct {
	Subject Subject
	Kind    Kind
	Title   string
	Source  json.RawMessage
	Steps   []Step
}

type EventKind string

const (
	Created        EventKind = "created"
	StepStarted    EventKind = "step_started"
	StepFinished   EventKind = "step_finished"
	Interrupted    EventKind = "interrupted"
	OwnerContinued EventKind = "owner_continued"
)

func EventKinds() []EventKind {
	return []EventKind{Created, StepStarted, StepFinished, Interrupted, OwnerContinued}
}

type Verdict string

const (
	VerdictTodo     Verdict = "todo"
	VerdictDone     Verdict = "done"
	VerdictConflict Verdict = "conflict"
)

func Verdicts() []Verdict { return []Verdict{VerdictTodo, VerdictDone, VerdictConflict} }

type Probe struct {
	Index   int
	Verdict Verdict
	Note    string
	QueueID int
}

type Event struct {
	Kind    EventKind
	At      time.Time
	Spec    *Spec
	Index   int
	Outcome StepState
	Note    string
	Err     string
	Cause   Cause
	Version int64
	Probes  []Probe
}

const maxSteps = 2000

func MaxSteps() int { return maxSteps }

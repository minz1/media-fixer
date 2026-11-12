package lifecycle

import "time"

type EffectKind string

const (
	LaunchRun       EffectKind = "launch_run"
	CancelRun       EffectKind = "cancel_run"
	ScheduleCheck   EffectKind = "schedule_check"
	ExecutePlan     EffectKind = "execute_plan"
	NotifyReporters EffectKind = "notify_reporters"
	NotifyOwner     EffectKind = "notify_owner"
	SeerrResolve    EffectKind = "seerr_resolve"
	SeerrComment    EffectKind = "seerr_comment"
)

type Seed string

const (
	SeedFresh   Seed = "fresh"
	SeedSummary Seed = "summary"
)

type CheckKind string

const (
	CheckVerify   CheckKind = "verify"
	CheckDownload CheckKind = "download"
)

type Note string

const (
	NoteFixed       Note = "fixed"
	NoteSearching   Note = "searching"
	NoteFound       Note = "found"
	NoteETA         Note = "eta"
	NoteHanded      Note = "handed"
	NoteAnotherLook Note = "another_look"
	NoteResolved    Note = "resolved"
)

type NoticeKind string

const (
	NoticeNone            NoticeKind = ""
	NoticeCard            NoticeKind = "card"
	NoticeSystemic        NoticeKind = "systemic"
	NoticeStale           NoticeKind = "stale"
	NoticePlanInterrupted NoticeKind = "plan_interrupted"
	NoticeDownload        NoticeKind = "download"
)

type DownloadProblem string

const (
	ProblemNone          DownloadProblem = ""
	ProblemLostTrack     DownloadProblem = "lost_track"
	ProblemStalled       DownloadProblem = "stalled"
	ProblemOverlong      DownloadProblem = "overlong"
	ProblemNeverImported DownloadProblem = "never_imported"
	ProblemNoRelease     DownloadProblem = "no_release"
	ProblemSearchEnded   DownloadProblem = "search_ended"
)

type Field struct {
	Name  string
	Value string
}

type Notice struct {
	Kind    NoticeKind
	Tone    string
	Title   string
	Body    string
	Fields  []Field
	Minutes int
	Problem DownloadProblem
	Target  string
	Err     string
}

type Effect struct {
	Kind       EffectKind
	Gen        int64
	Seq        int
	Seed       Seed
	Origin     Origin
	At         time.Time
	Check      CheckKind
	OpID       int64
	Note       Note
	ETAMinutes int
	Notice     Notice
}

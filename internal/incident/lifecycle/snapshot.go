package lifecycle

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/minz1/mediafixer/internal/operation"
)

const (
	EscalateRemoveAndSearch    = "remove_and_search"
	EscalateDeleteTorrentReadd = "delete_torrent_readd"
	EscalateSwapRelease        = "swap_release"
	EscalateResyncSubtitle     = "resync_subtitle"
	WhatStuckRead              = "stuck_read"
)

type Snapshot struct {
	ID             string
	Status         State
	Version        int64
	Generation     int64
	Locked         bool
	ActionCount    int
	Parked         bool
	StuckReadReran bool
	Reason         Reason
	What           string
	ItemID         string
	Finding        Finding
	NextCheck      time.Time
	Verify         *Verify
	Download       *Download
	Plan           *Plan
	Op             *operation.Op
	In             Inputs
}

type Finding struct {
	EscalateAction string
	JSON           json.RawMessage
}

func (f Finding) Escalatable() bool {
	return slices.Contains(
		[]string{EscalateRemoveAndSearch, EscalateDeleteTorrentReadd, EscalateSwapRelease, EscalateResyncSubtitle},
		f.EscalateAction,
	)
}

type VerifyKind string

const (
	VerifyGeneric   VerifyKind = "generic"
	VerifyStuckRead VerifyKind = "stuck_read"
	VerifySubtitles VerifyKind = "subtitles"
	VerifyResync    VerifyKind = "resync"
)

type Verify struct {
	ChecksLeft int
	Spec       VerifySpec
}

type VerifySpec struct {
	Kind           VerifyKind      `json:"kind"`
	ItemID         string          `json:"item_id"`
	PreFix         json.RawMessage `json:"pre,omitempty"`
	Language       string          `json:"language,omitempty"`
	SubtitleItemID string          `json:"subtitle_item_id,omitempty"`
	Swap           *SwapTarget     `json:"swap,omitempty"`
	Resync         *ResyncTarget   `json:"resync,omitempty"`
	Action         string          `json:"action,omitempty"`
	ETAMinutes     int             `json:"eta_minutes,omitempty"`
	Extensions     int             `json:"extensions"`
	CheckSeq       int             `json:"check_seq,omitempty"`
	ETANoted       bool            `json:"eta_noted"`

	VerifyEnteredAt time.Time `json:"verify_entered_at,omitzero"`
}

type SwapTarget struct {
	SeriesItemID   string `json:"series_item_id"`
	Season         int    `json:"season"`
	Episode        int    `json:"episode"`
	SonarrSeriesID int    `json:"sonarr_series_id"`
	EpisodeID      int    `json:"episode_id"`
	OldFileID      int    `json:"old_file_id"`
	CandidateSize  int64  `json:"candidate_size"`
}

type ResyncTarget struct {
	Movie    bool   `json:"movie,omitempty"`
	ID       int    `json:"id,omitempty"`
	Path     string `json:"path,omitempty"`
	Language string `json:"language,omitempty"`
	Baseline int    `json:"baseline,omitempty"`
}

type Stage string

const (
	StageSearching   Stage = "searching"
	StageDownloading Stage = "downloading"
	StageImporting   Stage = "importing"
)

func Stages() []Stage { return []Stage{StageSearching, StageDownloading, StageImporting} }

type Download struct {
	MediaType          string    `json:"media_type"`
	Title              string    `json:"title"`
	Season             int       `json:"season"`
	Episode            int       `json:"episode"`
	StartedAt          time.Time `json:"started_at"`
	Stage              Stage     `json:"last_stage,omitempty"`
	QueueStatus        string    `json:"queue_status,omitempty"`
	LastProgressAt     time.Time `json:"last_progress_at"`
	LastProgressPct    float64   `json:"last_progress_pct"`
	GrabNotified       bool      `json:"grab_notified"`
	KeepSearching      bool      `json:"keep_searching"`
	KeepSearchingUntil time.Time `json:"keep_searching_until"`
	CheckSeq           int       `json:"check_seq,omitempty"`
}

type Plan struct {
	JSON []byte
	At   time.Time
	FP   string
}

type Inputs struct {
	Paused      bool
	OpRunning   bool
	ActiveCount int
	Limits      Limits
}

type Limits struct {
	MaxActions         int
	SystemicThreshold  int
	VerifyChecks       int
	ExtensionsMax      int
	Minute             time.Duration
	FirstVerifyCap     time.Duration
	VerifyGap          time.Duration
	StuckReadFloor     time.Duration
	ApprovedVerify     time.Duration
	CheckEvery         time.Duration
	NoReleaseGrace     time.Duration
	Stall              time.Duration
	TrackCap           time.Duration
	KeepSearchingCheck time.Duration
	KeepSearchingSpan  time.Duration
	PlanTTL            time.Duration
}

type Decision struct {
	Next        Snapshot
	Effects     []Effect
	JournalOnly bool
	OpWrite     *operation.Op
	OpNew       bool
}

func (s Snapshot) PlanFresh(now time.Time) bool {
	return s.Plan != nil && !now.After(s.Plan.At.Add(s.In.Limits.PlanTTL))
}

func (s Snapshot) Clone() Snapshot {
	n := s
	if s.Verify != nil {
		v := *s.Verify
		n.Verify = &v
	}
	if s.Download != nil {
		d := *s.Download
		n.Download = &d
	}
	if s.Plan != nil {
		p := *s.Plan
		n.Plan = &p
	}
	if s.Op != nil {
		o := *s.Op
		o.Results = slices.Clone(s.Op.Results)
		n.Op = &o
	}
	return n
}

func (s Snapshot) CheckSeq() int {
	switch {
	case s.Verify != nil:
		return s.Verify.Spec.CheckSeq
	case s.Download != nil:
		return s.Download.CheckSeq
	}
	return 0
}

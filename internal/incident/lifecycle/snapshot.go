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
	return f.EscalateAction == EscalateRemoveAndSearch || f.EscalateAction == EscalateDeleteTorrentReadd
}

type VerifyKind string

const (
	VerifyGeneric   VerifyKind = "generic"
	VerifyStuckRead VerifyKind = "stuck_read"
)

type Verify struct {
	ChecksLeft int
	Spec       VerifySpec
}

type VerifySpec struct {
	Kind       VerifyKind      `json:"kind"`
	ItemID     string          `json:"item_id"`
	PreFix     json.RawMessage `json:"pre,omitempty"`
	Action     string          `json:"action,omitempty"`
	ETAMinutes int             `json:"eta_minutes,omitempty"`
	Extensions int             `json:"extensions"`
	ETANoted   bool            `json:"eta_noted"`
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

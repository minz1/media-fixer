package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
)

type View struct {
	Status        State
	Reason        Reason
	Locked        bool
	ActionCount   int
	Parked        bool
	Finding       string
	HasPlan       bool
	PlanFP        string
	PlanAt        int64
	Stage         Stage
	QueueStatus   string
	KeepSearching bool
	Target        string
}

func OwnerView(s Snapshot) View {
	v := View{
		Status: s.Status, Reason: s.Reason, Locked: s.Locked, ActionCount: s.ActionCount, Parked: s.Parked,
	}
	if len(s.Finding.JSON) > 0 {
		sum := sha256.Sum256(s.Finding.JSON)
		v.Finding = s.Finding.EscalateAction + ":" + hex.EncodeToString(sum[:])
	}
	if s.Plan != nil {
		v.HasPlan, v.PlanFP, v.PlanAt = true, s.Plan.FP, s.Plan.At.UnixNano()
	}
	if d := s.Download; d != nil {
		v.Stage, v.QueueStatus, v.KeepSearching = d.Stage, d.QueueStatus, d.KeepSearching
		v.Target = d.MediaType + "|" + d.Label()
	}
	return v
}

func Versioned(cur, next Snapshot) Snapshot {
	if OwnerView(cur) != OwnerView(next) {
		next.Version = cur.Version + 1
	} else {
		next.Version = cur.Version
	}
	return next
}

func VersionChecked(k EventKind) bool { return versionChecked(k) }

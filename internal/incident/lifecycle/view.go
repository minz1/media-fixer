package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/minz1/mediafixer/internal/operation"
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
	OpID          int64
	OpVersion     int64
	OpStatus      operation.Status
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
	if o := s.Op; o != nil {
		v.OpID, v.OpVersion, v.OpStatus = o.ID, o.Version, o.Status
	}
	if d := s.Download; d != nil {
		v.Stage, v.QueueStatus, v.KeepSearching = d.Stage, d.QueueStatus, d.KeepSearching
		v.Target = d.MediaType + "|" + d.Label()
	}
	return v
}

func Versioned(cur, next Snapshot) Snapshot {
	if OwnerView(cur) != OwnerView(next) || cur.Generation != next.Generation {
		next.Version = cur.Version + 1
	} else {
		next.Version = cur.Version
	}
	return next
}

func VersionChecked(k EventKind) bool { return versionChecked(k) }

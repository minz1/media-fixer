package server

import (
	"context"

	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
)

type actionView struct {
	Label string
	Path  string
	Op    int64
	OpV   int64
	Next  string
}

type ownerView struct {
	Buttons     []actionView
	Keep        bool
	Preview     bool
	Version     int64
	Meta        lifecycle.StateMeta
	Parked      bool
	BudgetSpent bool
	Partial     string
}

func actionViewOf(snap lifecycle.Snapshot, k lifecycle.ActionKind) (actionView, bool) {
	switch k {
	case lifecycle.ActRerun:
		return actionView{Label: "Re-run diagnosis", Path: "rerun"}, true
	case lifecycle.ActResolve:
		return actionView{Label: "Mark resolved", Path: "resolve"}, true
	case lifecycle.ActUnlock:
		return actionView{Label: "🔓 Unlock", Path: "unlock"}, true
	case lifecycle.ActResume:
		if snap.Op != nil {
			return actionView{
				Label: "Resume",
				Path:  "resume",
				Op:    snap.Op.ID,
				OpV:   snap.Op.Version,
				Next:  operation.NextLine(*snap.Op),
			}, true
		}
	case lifecycle.ActKeepSearching, lifecycle.ActPreview, lifecycle.ActApprove, lifecycle.ActRefresh:
	}
	return actionView{}, false
}

func (s *Server) ownerView(ctx context.Context, snap lifecycle.Snapshot, actions []lifecycle.Action) ownerView {
	v := ownerView{
		Version: snap.Version, Meta: lifecycle.Meta(snap.Status), Parked: snap.Parked,
		BudgetSpent: incident.BudgetSpent(snap), Partial: s.svc.PartialLine(ctx, snap),
	}
	for _, a := range actions {
		switch a.Kind {
		case lifecycle.ActKeepSearching:
			v.Keep = true
		case lifecycle.ActPreview, lifecycle.ActApprove, lifecycle.ActRefresh:
			v.Preview = true
		case lifecycle.ActRerun, lifecycle.ActResolve, lifecycle.ActUnlock, lifecycle.ActResume:
		}
		if av, ok := actionViewOf(snap, a.Kind); ok {
			v.Buttons = append(v.Buttons, av)
		}
	}
	return v
}

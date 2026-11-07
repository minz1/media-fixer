package server

import (
	"context"
	"time"

	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

type actionView struct {
	Label string
	Path  string
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

func actionViewOf(k lifecycle.ActionKind) (actionView, bool) {
	switch k {
	case lifecycle.ActRerun:
		return actionView{Label: "Re-run diagnosis", Path: "rerun"}, true
	case lifecycle.ActResolve:
		return actionView{Label: "Mark resolved", Path: "resolve"}, true
	case lifecycle.ActUnlock:
		return actionView{Label: "🔓 Unlock", Path: "unlock"}, true
	case lifecycle.ActKeepSearching, lifecycle.ActPreview, lifecycle.ActApprove, lifecycle.ActRefresh:
	}
	return actionView{}, false
}

func (s *Server) ownerView(ctx context.Context, snap lifecycle.Snapshot) ownerView {
	v := ownerView{
		Version: snap.Version, Meta: lifecycle.Meta(snap.Status), Parked: snap.Parked,
		BudgetSpent: incident.BudgetSpent(snap), Partial: s.svc.PartialLine(ctx, snap),
	}
	for _, a := range lifecycle.Actions(snap, time.Now()) {
		switch a.Kind {
		case lifecycle.ActKeepSearching:
			v.Keep = true
		case lifecycle.ActPreview, lifecycle.ActApprove, lifecycle.ActRefresh:
			v.Preview = true
		case lifecycle.ActRerun, lifecycle.ActResolve, lifecycle.ActUnlock:
		}
		if av, ok := actionViewOf(a.Kind); ok {
			v.Buttons = append(v.Buttons, av)
		}
	}
	return v
}

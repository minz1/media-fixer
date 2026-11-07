package journal

import (
	"context"
	"encoding/json"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

type actionAttemptedPayload struct {
	Generation int64          `json:"generation"`
	Tool       string         `json:"tool"`
	Params     map[string]any `json:"params,omitempty"`
}

type ownerRerunPayload struct {
	Generation int64 `json:"generation"`
}

type Attempt struct {
	Generation int64
	Tool       string
	Params     map[string]any
	At         time.Time
}

type AppliedAction struct {
	Generation int64
	Action     string
	Params     map[string]any
	Status     string
}

type AttemptLog struct {
	Attempts   []Attempt
	Applied    []AppliedAction
	OwnerRerun int64
}

func ActionAttemptedEvent(incidentID string, generation int64, tool string, params map[string]any) (*db.Event, error) {
	payload, err := json.Marshal(actionAttemptedPayload{Generation: generation, Tool: tool, Params: params})
	if err != nil {
		return nil, err
	}
	return &db.Event{IncidentID: incidentID, At: time.Now(), Kind: string(KindActionAttempted), Payload: payload}, nil
}

func OwnerRerunEvent(incidentID string, generation int64) (*db.Event, error) {
	payload, err := json.Marshal(ownerRerunPayload{Generation: generation})
	if err != nil {
		return nil, err
	}
	return &db.Event{IncidentID: incidentID, At: time.Now(), Kind: string(KindOwnerRerun), Payload: payload}, nil
}

func (j *Journal) ActionAttempts(ctx context.Context, incidentID string) (AttemptLog, error) {
	events, err := j.db.EventsSince(ctx, incidentID, 0)
	if err != nil {
		return AttemptLog{}, err
	}
	var out AttemptLog
	for _, e := range events {
		switch e.Kind {
		case string(KindActionAttempted):
			var p actionAttemptedPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				out.Attempts = append(out.Attempts, Attempt{
					Generation: p.Generation, Tool: p.Tool, Params: p.Params, At: e.At,
				})
			}
		case string(KindActionApplied):
			var p struct {
				Action     string         `json:"action"`
				Params     map[string]any `json:"params"`
				Status     string         `json:"status"`
				Generation int64          `json:"generation"`
			}
			if json.Unmarshal(e.Payload, &p) == nil {
				out.Applied = append(out.Applied, AppliedAction{
					Generation: p.Generation, Action: p.Action, Params: p.Params, Status: p.Status,
				})
			}
		case string(KindOwnerRerun):
			var p ownerRerunPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				out.OwnerRerun = max(out.OwnerRerun, p.Generation)
			}
		}
	}
	return out, nil
}

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const snapshotColumns = `id, status, version, generation, autonomous_locked, action_count, parked,
	COALESCE(reason, ''), what, COALESCE(jellyfin_item_id, ''), COALESCE(finding, ''), pending_outcome,
	escalation_plan, escalation_plan_at, verify_next_check, verify_checks_left, verify_spec`

const snapshotQuery = `SELECT ` + snapshotColumns + ` FROM incidents WHERE id = ?`

func (d *DB) GetIncidentSnapshot(ctx context.Context, id string) (lifecycle.Snapshot, error) {
	return loadSnapshot(ctx, d.read, id)
}

func loadSnapshot(ctx context.Context, q querier, id string) (lifecycle.Snapshot, error) {
	s, err := scanSnapshot(q.QueryRowContext(ctx, snapshotQuery, id))
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycle.Snapshot{}, ErrNotFound
	}
	return s, err
}

type snapshotRow struct {
	locked, parked int
	finding        string
	po, plan, spec sql.NullString
	planAt, next   sql.NullTime
	checksLeft     sql.NullInt64
}

func scanSnapshot(sc scanner) (lifecycle.Snapshot, error) {
	var (
		s lifecycle.Snapshot
		r snapshotRow
	)
	err := sc.Scan(&s.ID, &s.Status, &s.Version, &s.Generation, &r.locked, &s.ActionCount, &r.parked, &s.Reason,
		&s.What, &s.ItemID, &r.finding, &r.po, &r.plan, &r.planAt, &r.next, &r.checksLeft, &r.spec)
	if err != nil {
		return s, err
	}
	s.Locked, s.Parked = r.locked == 1, r.parked == 1
	if err = fillSnapshot(&s, r); err != nil {
		return s, fmt.Errorf("incident %s: %w", s.ID, err)
	}
	return s, nil
}

func fillSnapshot(s *lifecycle.Snapshot, r snapshotRow) error {
	if r.finding != "" && r.finding != nullJSON {
		var f struct {
			EscalateAction string `json:"escalate_action"`
		}
		if err := json.Unmarshal([]byte(r.finding), &f); err != nil {
			return fmt.Errorf("decode finding: %w", err)
		}
		s.Finding = lifecycle.Finding{EscalateAction: f.EscalateAction, JSON: json.RawMessage(r.finding)}
	}
	if r.po.Valid {
		var dl lifecycle.Download
		if err := json.Unmarshal([]byte(r.po.String), &dl); err != nil {
			return fmt.Errorf("decode pending outcome: %w", err)
		}
		s.Download = &dl
	}
	if r.plan.Valid && r.plan.String != "" {
		s.Plan = &lifecycle.Plan{
			JSON: []byte(r.plan.String),
			At:   r.planAt.Time,
			FP:   card.Fingerprint([]byte(r.plan.String)),
		}
	}
	if r.next.Valid {
		s.NextCheck = r.next.Time
	}
	if r.checksLeft.Valid || r.spec.Valid {
		v := &lifecycle.Verify{ChecksLeft: int(r.checksLeft.Int64)}
		if r.spec.Valid {
			if err := json.Unmarshal([]byte(r.spec.String), &v.Spec); err != nil {
				return fmt.Errorf("decode verify spec: %w", err)
			}
		}
		s.Verify = v
	}
	return nil
}

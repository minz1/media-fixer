package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
)

func (d *DB) loadIncidentOp(ctx context.Context, tx querier, cur *lifecycle.Snapshot) error {
	subject := operation.Subject{Kind: operation.SubjectIncident, ID: cur.ID}
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM operations
		WHERE subject_kind = ? AND subject_id = ? AND status = 'running')`,
		string(subject.Kind), subject.ID).Scan(&cur.In.OpRunning)
	if err != nil {
		return err
	}
	id, err := latestOpID(ctx, tx, cur.ID)
	if err != nil || id == 0 {
		return err
	}
	op, err := loadOperation(ctx, tx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case op.Subject != subject:
		d.log.WarnContext(ctx, "status_changed op belongs to another subject", "incident", cur.ID, "op_id", id,
			"subject", string(op.Subject.Kind)+":"+op.Subject.ID)
		return nil
	case cur.Status == lifecycle.ExecutingPlan && (cur.Plan == nil || !bytes.Equal(op.Source, cur.Plan.JSON)):
		d.log.WarnContext(ctx, "status_changed op source differs from the plan", "incident", cur.ID, "op_id", id)
		return nil
	}
	cur.Op = &op
	return nil
}

func latestOpID(ctx context.Context, tx querier, incidentID string) (int64, error) {
	var payload string
	err := tx.QueryRowContext(ctx, `SELECT payload FROM incident_events
		WHERE incident_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`, incidentID, kindStatusChanged).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var p struct {
		OpID int64 `json:"op_id"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	return p.OpID, nil
}

func writeIncidentOp(
	ctx context.Context,
	tx *sql.Tx,
	dec Decision,
	next lifecycle.Snapshot,
) (lifecycle.Snapshot, []lifecycle.Effect, error) {
	effects := slices.Clone(dec.Effects)
	if dec.OpWrite == nil {
		return next, effects, nil
	}
	if err := lifecycle.CheckDecision(dec.Decision); err != nil {
		return next, effects, err
	}
	written, err := storeIncidentOp(ctx, tx, dec)
	if err != nil {
		return next, effects, err
	}
	if !dec.OpNew {
		return next, effects, nil
	}
	if next.Op != nil && next.Op.ID == 0 {
		o := *next.Op
		o.ID = written.ID
		next.Op = &o
	}
	for i := range effects {
		if effects[i].Kind == lifecycle.ExecutePlan && effects[i].OpID == 0 {
			effects[i].OpID = written.ID
		}
	}
	return next, effects, nil
}

func storeIncidentOp(ctx context.Context, tx *sql.Tx, dec Decision) (operation.Op, error) {
	if dec.OpNew {
		return createOperation(ctx, tx, *dec.OpWrite)
	}
	stored, err := loadOperation(ctx, tx, dec.OpWrite.ID)
	if err != nil {
		return operation.Op{}, fmt.Errorf("incident %s: op %d: %w", dec.Next.ID, dec.OpWrite.ID, err)
	}
	if stored.Subject != dec.OpWrite.Subject {
		return operation.Op{}, fmt.Errorf("%w: %s: I11: op %d belongs to %v", lifecycle.ErrIllegal, dec.Next.ID,
			stored.ID, stored.Subject)
	}
	return writeOperation(ctx, tx, stored, *dec.OpWrite)
}

func (d *DB) IncidentCardView(ctx context.Context, id string) (*Incident, lifecycle.Snapshot, error) {
	tx, err := d.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, lifecycle.Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	inc, err := scanIncident(tx.QueryRowContext(ctx, incidentQuery, id))
	if err != nil {
		return nil, lifecycle.Snapshot{}, err
	}
	s, err := loadSnapshot(ctx, tx, id)
	if err != nil {
		return nil, s, err
	}
	return inc, s, d.loadIncidentOp(ctx, tx, &s)
}

func (d *DB) GetIncidentSnapshotWithOp(ctx context.Context, id string) (lifecycle.Snapshot, error) {
	s, err := loadSnapshot(ctx, d.read, id)
	if err != nil {
		return s, err
	}
	return s, d.loadIncidentOp(ctx, d.read, &s)
}

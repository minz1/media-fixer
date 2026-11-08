package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const snapshotColumns = `id, status, version, generation, autonomous_locked, action_count, parked,
	COALESCE(reason, ''), what, COALESCE(jellyfin_item_id, ''), COALESCE(finding, ''), pending_outcome,
	escalation_plan, escalation_plan_at, verify_next_check, verify_checks_left, verify_spec, stuck_read_reran`

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
	locked, parked, reran int
	finding               string
	po, plan, spec        sql.NullString
	planAt, next          sql.NullTime
	checksLeft            sql.NullInt64
}

func scanSnapshot(sc scanner) (lifecycle.Snapshot, error) { return scanSnapshotWith(sc) }

func scanSnapshotWith(sc scanner, extra ...any) (lifecycle.Snapshot, error) {
	var (
		s lifecycle.Snapshot
		r snapshotRow
	)
	dest := []any{&s.ID, &s.Status, &s.Version, &s.Generation, &r.locked, &s.ActionCount, &r.parked, &s.Reason,
		&s.What, &s.ItemID, &r.finding, &r.po, &r.plan, &r.planAt, &r.next, &r.checksLeft, &r.spec, &r.reran}
	err := sc.Scan(append(dest, extra...)...)
	if err != nil {
		return s, err
	}
	s.Locked, s.Parked, s.StuckReadReran = r.locked == 1, r.parked == 1, r.reran == 1
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

type Decision struct {
	lifecycle.Decision

	Events []*Event
}

type Decide func(cur lifecycle.Snapshot) (Decision, error)

type Applied struct {
	Snap       lifecycle.Snapshot
	PrevStatus lifecycle.State
	Effects    []lifecycle.Effect
	Events     []*Event
	Merged     bool
	OpID       int64
	seqs       []int64
}

type NewIncident struct {
	ID                string
	Source            string
	ReportedBy        string
	ReporterDiscordID string
	SeerrIssueID      string
	What              string
	Title             string
	JellyfinItemID    string
	Details           string
}

const kindStatusChanged = "status_changed"

func (d *DB) ApplyIncident(ctx context.Context, id string, decide Decide) (Applied, error) {
	var out Applied
	err := runTx(ctx, d.write, func(ctx context.Context, tx *sql.Tx) error {
		cur, err := loadSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.In, err = readInputs(ctx, tx); err != nil {
			return err
		}
		if err = d.loadIncidentOp(ctx, tx, &cur); err != nil {
			return err
		}
		dec, err := decide(cur)
		if err != nil {
			return err
		}
		out, err = commitDecision(ctx, tx, cur, dec)
		return err
	})
	if err != nil {
		return Applied{}, err
	}
	return out.withSeqs(), nil
}

func commitDecision(ctx context.Context, tx *sql.Tx, cur lifecycle.Snapshot, dec Decision) (Applied, error) {
	events := dec.Events
	next := cur
	effects := dec.Effects
	if !dec.JournalOnly {
		var err error
		next = lifecycle.Versioned(cur, dec.Next)
		if err = lifecycle.Check(next); err != nil {
			return Applied{}, err
		}
		if next, effects, err = writeIncidentOp(ctx, tx, dec, next); err != nil {
			return Applied{}, err
		}
		if err = writeSnapshot(ctx, tx, cur, next); err != nil {
			return Applied{}, err
		}
		if next.Version != cur.Version {
			events = append(events, statusChangedEvent(next))
		}
	}
	seqs, err := appendAll(ctx, tx, events)
	if err != nil {
		return Applied{}, err
	}
	out := Applied{Snap: next, PrevStatus: cur.Status, Effects: effects, Events: events, seqs: seqs}
	if next.Op != nil {
		out.OpID = next.Op.ID
	}
	return out, nil
}

func appendAll(ctx context.Context, tx *sql.Tx, events []*Event) ([]int64, error) {
	seqs := make([]int64, len(events))
	for i, e := range events {
		seq, err := appendEvent(ctx, tx, e)
		if err != nil {
			return nil, err
		}
		seqs[i] = seq
	}
	return seqs, nil
}

func (a Applied) withSeqs() Applied {
	for i, e := range a.Events {
		e.Seq = a.seqs[i]
	}
	a.seqs = nil
	return a
}

func statusChangedEvent(s lifecycle.Snapshot) *Event {
	var opID int64
	if s.Op != nil {
		opID = s.Op.ID
	}
	payload, _ := json.Marshal(struct {
		Status  string `json:"status"`
		Version int64  `json:"version"`
		OpID    int64  `json:"op_id,omitempty"`
	}{string(s.Status), s.Version, opID})
	return &Event{IncidentID: s.ID, Kind: kindStatusChanged, Payload: payload}
}

func readInputs(ctx context.Context, tx *sql.Tx) (lifecycle.Inputs, error) {
	var in lifecycle.Inputs
	var paused sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'autonomous_paused'`).Scan(&paused)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return in, err
	}
	in.Paused = paused.String == "true"
	active, err := json.Marshal(lifecycle.StatesWhere(func(m lifecycle.StateMeta) bool { return m.Active }))
	if err != nil {
		return in, err
	}
	err = tx.QueryRowContext(ctx, countOpenIncidentsQuery, string(active)).Scan(&in.ActiveCount)
	return in, err
}

type snapshotCols struct {
	locked, parked, reran int
	reason, po, plan      sql.NullString
	planAt, next          sql.NullTime
	checksLeft            sql.NullInt64
	spec                  sql.NullString
}

func encodeSnapshot(s lifecycle.Snapshot) (snapshotCols, error) {
	var c snapshotCols
	if s.Locked {
		c.locked = 1
	}
	if s.Parked {
		c.parked = 1
	}
	if s.StuckReadReran {
		c.reran = 1
	}
	if s.Reason != lifecycle.ReasonNone {
		c.reason = sql.NullString{String: string(s.Reason), Valid: true}
	}
	if s.Download != nil {
		b, err := json.Marshal(s.Download)
		if err != nil {
			return c, err
		}
		c.po = sql.NullString{String: string(b), Valid: true}
	}
	if s.Plan != nil {
		c.plan = sql.NullString{String: string(s.Plan.JSON), Valid: true}
		c.planAt = sql.NullTime{Time: s.Plan.At, Valid: true}
	}
	if !s.NextCheck.IsZero() {
		c.next = sql.NullTime{Time: s.NextCheck, Valid: true}
	}
	if s.Verify != nil {
		b, err := json.Marshal(s.Verify.Spec)
		if err != nil {
			return c, err
		}
		c.spec = sql.NullString{String: string(b), Valid: true}
		c.checksLeft = sql.NullInt64{Int64: int64(s.Verify.ChecksLeft), Valid: true}
	}
	return c, nil
}

func writeSnapshot(ctx context.Context, tx *sql.Tx, cur, next lifecycle.Snapshot) error {
	c, err := encodeSnapshot(next)
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = tx.ExecContext(ctx, `UPDATE incidents SET status = ?, version = ?, generation = ?,
		autonomous_locked = ?, action_count = ?, parked = ?, reason = ?, pending_outcome = ?,
		pending_outcome_next_check = NULL, escalation_plan = ?, escalation_plan_at = ?, verify_next_check = ?,
		verify_checks_left = ?, verify_spec = ?, stuck_read_reran = ?, updated_at = ? WHERE id = ?`,
		next.Status, next.Version, next.Generation, c.locked, next.ActionCount, c.parked, c.reason, c.po,
		c.plan, c.planAt, c.next, c.checksLeft, c.spec, c.reran, now, next.ID)
	if err != nil {
		return err
	}
	if !bytes.Equal(cur.Finding.JSON, next.Finding.JSON) {
		return writeFinding(ctx, tx, next.ID, next.Finding.JSON, next.Finding.JSON)
	}
	return nil
}

func (d *DB) CreateFromReport(ctx context.Context, r *NewIncident, decide Decide) (Applied, error) {
	var out Applied
	err := runTx(ctx, d.write, func(ctx context.Context, tx *sql.Tx) error {
		existing, err := d.activeByTitle(ctx, tx, r.Title)
		switch {
		case err == nil:
			out, err = mergeReport(ctx, tx, existing, r)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		out, err = insertReport(ctx, tx, r, decide)
		return err
	})
	if err != nil {
		return Applied{}, err
	}
	return out.withSeqs(), nil
}

func (d *DB) activeByTitle(ctx context.Context, tx *sql.Tx, title string) (string, error) {
	norm := d.normalizeTitle(title)
	active, err := json.Marshal(lifecycle.StatesWhere(func(m lifecycle.StateMeta) bool { return m.Active }))
	if err != nil {
		return "", err
	}
	inc, err := scanIncident(tx.QueryRowContext(ctx, findOpenByTitleQuery, norm, likeEscape(norm), string(active)))
	if err != nil {
		return "", err
	}
	return inc.ID, nil
}

func mergeReport(ctx context.Context, tx *sql.Tx, id string, r *NewIncident) (Applied, error) {
	events, effects, err := addReport(ctx, tx, id, r)
	if err != nil {
		return Applied{}, err
	}
	s, err := loadSnapshot(ctx, tx, id)
	if err != nil {
		return Applied{}, err
	}
	seqs, err := appendAll(ctx, tx, events)
	if err != nil {
		return Applied{}, err
	}
	return Applied{Snap: s, Effects: effects, Events: events, Merged: true, seqs: seqs}, nil
}

func addReport(ctx context.Context, tx *sql.Tx, id string, r *NewIncident) ([]*Event, []lifecycle.Effect, error) {
	now := time.Now()
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO incident_reporters (incident_id, reporter, source, discord_user_id, reported_at)
		 VALUES (?, ?, ?, ?, ?)`, id, r.ReportedBy, r.Source, nullStr(r.ReporterDiscordID), now)
	if err != nil {
		return nil, nil, err
	}
	payload, err := json.Marshal(struct {
		Reporter      string `json:"reporter"`
		Source        string `json:"source"`
		DiscordUserID string `json:"discord_user_id,omitempty"`
	}{r.ReportedBy, r.Source, r.ReporterDiscordID})
	if err != nil {
		return nil, nil, err
	}
	events := []*Event{{IncidentID: id, Kind: "reporter_added", Payload: payload}}
	if r.SeerrIssueID == "" {
		return events, nil, nil
	}
	inserted, err := rowChanged(tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO incident_seerr_issues (incident_id, issue_id, added_at) VALUES (?, ?, ?)`,
		id, r.SeerrIssueID, now))
	if err != nil || !inserted {
		return events, nil, err
	}
	return events, []lifecycle.Effect{{Kind: lifecycle.SeerrComment}}, nil
}

func insertReport(ctx context.Context, tx *sql.Tx, r *NewIncident, decide Decide) (Applied, error) {
	in, err := readInputs(ctx, tx)
	if err != nil {
		return Applied{}, err
	}
	id := r.ID
	if id == "" {
		id = uuid.New().String()
	}
	cur := lifecycle.Snapshot{ID: id, What: r.What, ItemID: r.JellyfinItemID, In: in}
	dec, err := decide(cur)
	if err != nil {
		return Applied{}, err
	}
	next := dec.Next
	next.ID, next.Version = cur.ID, 0
	if err = lifecycle.Check(next); err != nil {
		return Applied{}, err
	}
	if err = insertIncident(ctx, tx, next, r); err != nil {
		return Applied{}, err
	}
	created, err := json.Marshal(struct {
		Source     string `json:"source"`
		ReportedBy string `json:"reported_by"`
		What       string `json:"what"`
		Title      string `json:"title"`
	}{r.Source, r.ReportedBy, r.What, r.Title})
	if err != nil {
		return Applied{}, err
	}
	events := []*Event{{IncidentID: next.ID, Kind: "incident_created", Payload: created}}
	more, effects, err := addReport(ctx, tx, next.ID, r)
	if err != nil {
		return Applied{}, err
	}
	events = append(append(events, more...), dec.Events...)
	seqs, err := appendAll(ctx, tx, events)
	if err != nil {
		return Applied{}, err
	}
	return Applied{Snap: next, Effects: append(dec.Effects, effects...), Events: events, seqs: seqs}, nil
}

func insertIncident(ctx context.Context, tx *sql.Tx, s lifecycle.Snapshot, r *NewIncident) error {
	c, err := encodeSnapshot(s)
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = tx.ExecContext(ctx, `INSERT INTO incidents (id, created_at, updated_at, status, source, reported_by,
		what, title, jellyfin_item_id, details, action_count, autonomous_locked, version, generation, parked, reason,
		pending_outcome, escalation_plan, escalation_plan_at, verify_next_check, verify_checks_left, verify_spec)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, now, now, s.Status, r.Source, r.ReportedBy, r.What, r.Title, nullStr(r.JellyfinItemID),
		nullStr(r.Details), s.ActionCount, c.locked, s.Version, s.Generation, c.parked, c.reason,
		c.po, c.plan, c.planAt, c.next, c.checksLeft, c.spec)
	return err
}

const drivenQuery = `SELECT ` + snapshotColumns + `,
	(SELECT MAX(e.at) FROM incident_events e WHERE e.incident_id = incidents.id) AS last_activity
	FROM incidents
	WHERE status IN ('open', 'reopened', 'investigating', 'verifying', 'awaiting_download', 'executing_plan')
	   OR (status = 'manual_test_needed' AND escalation_plan IS NOT NULL)
	ORDER BY created_at`

type SkippedRow struct {
	ID  string
	Err error
}

func (d *DB) ListDriven(ctx context.Context) ([]lifecycle.Observed, []SkippedRow, error) {
	rows, err := d.read.QueryContext(ctx, drivenQuery)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var (
		out     []lifecycle.Observed
		skipped []SkippedRow
	)
	for rows.Next() {
		var last sql.NullString
		o, serr := scanSnapshotWith(rows, &last)
		if serr != nil {
			skipped = append(skipped, SkippedRow{ID: o.ID, Err: serr})
			continue
		}
		out = append(out, lifecycle.Observed{Snap: o, LastActivity: parseActivity(last.String)})
	}
	return out, skipped, rows.Err()
}

func (d *DB) ClaimReporterNote(ctx context.Context, id string, now time.Time, minGap time.Duration) (bool, error) {
	res, err := d.write.ExecContext(ctx,
		`UPDATE incidents SET last_reporter_note_at = ?
		 WHERE id = ? AND (last_reporter_note_at IS NULL OR last_reporter_note_at <= ?)`,
		now, id, now.Add(-minGap))
	return rowChanged(res, err)
}

func parseActivity(s string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05.999999999-07:00", time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
func writeFinding(ctx context.Context, tx *sql.Tx, id string, finding, actions []byte) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE incidents SET finding = ?, recommended_actions = ?, updated_at = ? WHERE id = ?`,
		string(finding), string(actions), time.Now(), id)
	return err
}

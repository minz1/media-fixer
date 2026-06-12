package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS incidents (
	id                  TEXT PRIMARY KEY,
	created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	status              TEXT NOT NULL DEFAULT 'open',
	source              TEXT NOT NULL,
	reported_by         TEXT NOT NULL,
	what                TEXT NOT NULL,
	title               TEXT NOT NULL,
	jellyfin_item_id    TEXT,
	details             TEXT,
	finding             TEXT,
	recommended_actions TEXT,
	action_count        INTEGER NOT NULL DEFAULT 0,
	autonomous_locked   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS incident_reporters (
	incident_id TEXT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
	reporter    TEXT NOT NULL,
	source      TEXT NOT NULL,
	reported_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (incident_id, reporter)
);

CREATE TABLE IF NOT EXISTS actions_log (
	id           TEXT PRIMARY KEY,
	incident_id  TEXT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
	action       TEXT NOT NULL,
	params       TEXT,
	triggered_by TEXT NOT NULL,
	status       TEXT NOT NULL DEFAULT 'pending',
	applied_at   DATETIME,
	result       TEXT,
	error        TEXT
);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

INSERT OR IGNORE INTO settings (key, value) VALUES ('autonomous_paused', 'false');

CREATE INDEX IF NOT EXISTS idx_incidents_status ON incidents(status);
CREATE INDEX IF NOT EXISTS idx_incidents_title  ON incidents(title);
CREATE INDEX IF NOT EXISTS idx_actions_incident ON actions_log(incident_id);
`

type DB struct {
	sql *sql.DB
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	conn.SetMaxOpenConns(1) // SQLite WAL handles readers, but serialise writes
	if _, err := conn.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &DB{sql: conn}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// --- Incidents ---

type IncidentStatus string

const (
	StatusOpen             IncidentStatus = "open"
	StatusInvestigating    IncidentStatus = "investigating"
	StatusAgentFixed       IncidentStatus = "agent_fixed"
	StatusManualTestNeeded IncidentStatus = "manual_test_needed"
	StatusResolved         IncidentStatus = "resolved"
	StatusReopened         IncidentStatus = "reopened"
)

type Incident struct {
	ID                 string         `json:"id"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	Status             IncidentStatus `json:"status"`
	Source             string         `json:"source"`
	ReportedBy         string         `json:"reported_by"`
	What               string         `json:"what"`
	Title              string         `json:"title"`
	JellyfinItemID     string         `json:"jellyfin_item_id,omitempty"`
	Details            string         `json:"details,omitempty"`
	Finding            any            `json:"finding,omitempty"`
	RecommendedActions any            `json:"recommended_actions,omitempty"`
	ActionCount        int            `json:"action_count"`
	AutonomousLocked   bool           `json:"autonomous_locked"`
}

func (d *DB) CreateIncident(ctx context.Context, inc *Incident) error {
	if inc.ID == "" {
		inc.ID = uuid.New().String()
	}
	inc.CreatedAt = time.Now()
	inc.UpdatedAt = inc.CreatedAt

	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO incidents (id, created_at, updated_at, status, source, reported_by, what, title, jellyfin_item_id, details)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inc.ID, inc.CreatedAt, inc.UpdatedAt, inc.Status,
		inc.Source, inc.ReportedBy, inc.What, inc.Title,
		nullStr(inc.JellyfinItemID), nullStr(inc.Details),
	)
	return err
}

func (d *DB) GetIncident(ctx context.Context, id string) (*Incident, error) {
	row := d.sql.QueryRowContext(ctx, `
		SELECT id, created_at, updated_at, status, source, reported_by, what, title,
		       COALESCE(jellyfin_item_id,''), COALESCE(details,''),
		       COALESCE(finding,''), COALESCE(recommended_actions,''),
		       action_count, autonomous_locked
		FROM incidents WHERE id = ?`, id)
	return scanIncident(row)
}

// FindOpenByTitle returns the first open/investigating/agent_fixed incident for
// this title so duplicate reports collapse into it.
func (d *DB) FindOpenByTitle(ctx context.Context, title string) (*Incident, error) {
	row := d.sql.QueryRowContext(ctx, `
		SELECT id, created_at, updated_at, status, source, reported_by, what, title,
		       COALESCE(jellyfin_item_id,''), COALESCE(details,''),
		       COALESCE(finding,''), COALESCE(recommended_actions,''),
		       action_count, autonomous_locked
		FROM incidents
		WHERE title = ? AND status NOT IN ('resolved','reopened')
		ORDER BY created_at DESC LIMIT 1`, title)
	inc, err := scanIncident(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return inc, err
}

func (d *DB) ListIncidents(ctx context.Context, statusFilter string, limit, offset int) ([]*Incident, error) {
	q := `SELECT id, created_at, updated_at, status, source, reported_by, what, title,
	             COALESCE(jellyfin_item_id,''), COALESCE(details,''),
	             COALESCE(finding,''), COALESCE(recommended_actions,''),
	             action_count, autonomous_locked
	      FROM incidents`
	args := []any{}
	if statusFilter != "" {
		q += " WHERE status = ?"
		args = append(args, statusFilter)
	}
	q += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (d *DB) CountOpenIncidents(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE status NOT IN ('resolved')`,
	).Scan(&n)
	return n, err
}

func (d *DB) UpdateIncidentStatus(ctx context.Context, id string, status IncidentStatus) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE incidents SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now(), id)
	return err
}

func (d *DB) SetIncidentFinding(ctx context.Context, id string, finding, actions any) error {
	fb, _ := json.Marshal(finding)
	ab, _ := json.Marshal(actions)
	_, err := d.sql.ExecContext(ctx,
		`UPDATE incidents SET finding = ?, recommended_actions = ?, updated_at = ? WHERE id = ?`,
		string(fb), string(ab), time.Now(), id)
	return err
}

func (d *DB) IncrementActionCount(ctx context.Context, id string) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`UPDATE incidents SET action_count = action_count + 1, updated_at = ? WHERE id = ?
		 RETURNING action_count`,
		time.Now(), id).Scan(&n)
	return n, err
}

func (d *DB) SetAutonomousLocked(ctx context.Context, id string, locked bool) error {
	v := 0
	if locked {
		v = 1
	}
	_, err := d.sql.ExecContext(ctx,
		`UPDATE incidents SET autonomous_locked = ?, updated_at = ? WHERE id = ?`,
		v, time.Now(), id)
	return err
}

// --- Reporters ---

func (d *DB) AddReporter(ctx context.Context, incidentID, reporter, source string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO incident_reporters (incident_id, reporter, source, reported_at)
		 VALUES (?, ?, ?, ?)`,
		incidentID, reporter, source, time.Now())
	return err
}

func (d *DB) ListReporters(ctx context.Context, incidentID string) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT reporter FROM incident_reporters WHERE incident_id = ? ORDER BY reported_at`,
		incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- Actions ---

type ActionStatus string

const (
	ActionPending  ActionStatus = "pending"
	ActionApplied  ActionStatus = "applied"
	ActionFailed   ActionStatus = "failed"
	ActionApproved ActionStatus = "approved"
	ActionDenied   ActionStatus = "denied"
)

type ActionLog struct {
	ID          string       `json:"id"`
	IncidentID  string       `json:"incident_id"`
	Action      string       `json:"action"`
	Params      any          `json:"params,omitempty"`
	TriggeredBy string       `json:"triggered_by"`
	Status      ActionStatus `json:"status"`
	AppliedAt   *time.Time   `json:"applied_at,omitempty"`
	Result      string       `json:"result,omitempty"`
	Error       string       `json:"error,omitempty"`
}

func (d *DB) LogAction(ctx context.Context, a *ActionLog) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	pb, _ := json.Marshal(a.Params)
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO actions_log (id, incident_id, action, params, triggered_by, status)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, a.IncidentID, a.Action, string(pb), a.TriggeredBy, a.Status)
	return err
}

func (d *DB) UpdateAction(ctx context.Context, id string, status ActionStatus, result, errMsg string) error {
	now := time.Now()
	_, err := d.sql.ExecContext(ctx,
		`UPDATE actions_log SET status = ?, applied_at = ?, result = ?, error = ? WHERE id = ?`,
		status, now, result, errMsg, id)
	return err
}

func (d *DB) ListActions(ctx context.Context, incidentID string) ([]*ActionLog, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, incident_id, action, COALESCE(params,''), triggered_by, status,
		       applied_at, COALESCE(result,''), COALESCE(error,'')
		FROM actions_log WHERE incident_id = ? ORDER BY rowid`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ActionLog
	for rows.Next() {
		a := &ActionLog{}
		var paramsStr string
		var appliedAt sql.NullTime
		if err := rows.Scan(&a.ID, &a.IncidentID, &a.Action, &paramsStr,
			&a.TriggeredBy, &a.Status, &appliedAt, &a.Result, &a.Error); err != nil {
			return nil, err
		}
		if paramsStr != "" && paramsStr != "null" {
			_ = json.Unmarshal([]byte(paramsStr), &a.Params)
		}
		if appliedAt.Valid {
			a.AppliedAt = &appliedAt.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Settings ---

func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, err
}

func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

func (d *DB) IsAutonomousPaused(ctx context.Context) (bool, error) {
	v, err := d.GetSetting(ctx, "autonomous_paused")
	if err != nil {
		return false, err
	}
	return v == "true", nil
}

// --- helpers ---

type scanner interface {
	Scan(dest ...any) error
}

func scanIncident(s scanner) (*Incident, error) {
	inc := &Incident{}
	var findingStr, actionsStr string
	var locked int
	err := s.Scan(
		&inc.ID, &inc.CreatedAt, &inc.UpdatedAt, &inc.Status,
		&inc.Source, &inc.ReportedBy, &inc.What, &inc.Title,
		&inc.JellyfinItemID, &inc.Details,
		&findingStr, &actionsStr,
		&inc.ActionCount, &locked,
	)
	if err != nil {
		return nil, err
	}
	inc.AutonomousLocked = locked == 1
	if findingStr != "" && findingStr != "null" {
		_ = json.Unmarshal([]byte(findingStr), &inc.Finding)
	}
	if actionsStr != "" && actionsStr != "null" {
		_ = json.Unmarshal([]byte(actionsStr), &inc.RecommendedActions)
	}
	return inc, nil
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

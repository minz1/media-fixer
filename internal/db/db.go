package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // register SQLite driver
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// nullJSON is the literal string stored by [json.Marshal] for nil/empty values.
const nullJSON = "null"

const findByStatusLimit = 100

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
	incident_id     TEXT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
	reporter        TEXT NOT NULL,
	source          TEXT NOT NULL,
	discord_user_id TEXT,
	reported_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
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

CREATE TABLE IF NOT EXISTS conversation_history (
	incident_id TEXT PRIMARY KEY REFERENCES incidents(id) ON DELETE CASCADE,
	messages    TEXT NOT NULL,
	updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- last_disruption is a single-row table (id is CHECK'd to 1) recording the most
-- recent service-wide disruptive action (restart_jellyfin, jellyfin_library_scan,
-- etc.) taken by any incident. A run starting shortly after one gets a note in its
-- prompt so it doesn't mistake that disruption's transient side effects for its
-- own incident's root cause. See Agent.disruptionNote.
CREATE TABLE IF NOT EXISTS last_disruption (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	action      TEXT NOT NULL,
	incident_id TEXT NOT NULL,
	at          DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_incidents_status ON incidents(status);
CREATE INDEX IF NOT EXISTS idx_incidents_title  ON incidents(title);
CREATE INDEX IF NOT EXISTS idx_actions_incident ON actions_log(incident_id);
`

// dedupReportersByDiscordID removes duplicate reporter rows for the same Discord
// user within an incident (keeping the earliest), so a unique index can be built.
// Rows without a discord_user_id are left alone — they dedup by the PK (reporter).
const dedupReportersByDiscordID = `
DELETE FROM incident_reporters
WHERE discord_user_id IS NOT NULL AND discord_user_id != ''
  AND rowid NOT IN (
    SELECT MIN(rowid) FROM incident_reporters
    WHERE discord_user_id IS NOT NULL AND discord_user_id != ''
    GROUP BY incident_id, discord_user_id
  );`

// createReporterDiscordIndex enforces one row per (incident, Discord user) so that
// AddReporter's INSERT OR IGNORE dedups the person at write time. It is partial:
// non-empty discord_user_id only, so non-Discord reporters are unaffected.
const createReporterDiscordIndex = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_reporters_discord
ON incident_reporters(incident_id, discord_user_id)
WHERE discord_user_id IS NOT NULL AND discord_user_id != '';`

// DB wraps [sql.DB] with schema management and typed query methods.
type DB struct {
	sql    *sql.DB
	yearRE *regexp.Regexp
}

// Open creates or opens the SQLite database at path, applying the schema.
func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	conn.SetMaxOpenConns(1) // SQLite WAL handles readers, but serialise writes
	if _, err = conn.ExecContext(context.Background(), schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	const migrateDiscordUserID = `ALTER TABLE incident_reporters ADD COLUMN discord_user_id TEXT`
	if _, err = conn.ExecContext(context.Background(), migrateDiscordUserID); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return nil, fmt.Errorf("migrate incident_reporters.discord_user_id: %w", err)
		}
	}
	const migrateLastHeartbeat = `ALTER TABLE incidents ADD COLUMN last_heartbeat DATETIME`
	if _, err = conn.ExecContext(context.Background(), migrateLastHeartbeat); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return nil, fmt.Errorf("migrate incidents.last_heartbeat: %w", err)
		}
	}
	// pending_outcome/pending_outcome_next_check track an async fix (currently
	// arr_search_missing) from trigger through resolution in durable storage —
	// a download can take hours, far longer than any in-memory verification
	// loop should hold a goroutine or survive a process restart. Kept as two
	// columns rather than one JSON blob so FindDuePendingOutcomes can filter
	// on next_check_at with a plain indexed WHERE instead of a JSON extract.
	const migratePendingOutcome = `ALTER TABLE incidents ADD COLUMN pending_outcome TEXT`
	if _, err = conn.ExecContext(context.Background(), migratePendingOutcome); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return nil, fmt.Errorf("migrate incidents.pending_outcome: %w", err)
		}
	}
	const migratePendingOutcomeNextCheck = `ALTER TABLE incidents ADD COLUMN pending_outcome_next_check DATETIME`
	if _, err = conn.ExecContext(context.Background(), migratePendingOutcomeNextCheck); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return nil, fmt.Errorf("migrate incidents.pending_outcome_next_check: %w", err)
		}
	}
	// A Discord user's identity for notification is discord_user_id, not the
	// display-name text in the PK. Enforce one reporter row per (incident, discord
	// user) structurally so every reader dedups for free. The DELETE clears any
	// pre-existing duplicates (a unique index fails to build otherwise); it is a
	// no-op once deduplicated, so it is safe to run on every startup.
	if _, err = conn.ExecContext(context.Background(), dedupReportersByDiscordID); err != nil {
		return nil, fmt.Errorf("dedup incident_reporters by discord_user_id: %w", err)
	}
	if _, err = conn.ExecContext(context.Background(), createReporterDiscordIndex); err != nil {
		return nil, fmt.Errorf("create incident_reporters discord index: %w", err)
	}
	return &DB{
		sql:    conn,
		yearRE: regexp.MustCompile(`\s*\(\d{4}\)\s*$`),
	}, nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error { return d.sql.Close() }

// normalizeTitle strips trailing " (YYYY)" year suffixes for fuzzy deduplication.
func (d *DB) normalizeTitle(title string) string {
	return strings.TrimSpace(d.yearRE.ReplaceAllString(title, ""))
}

// --- Incidents ---

// IncidentStatus is the lifecycle state of an incident.
type IncidentStatus string

const (
	StatusOpen             IncidentStatus = "open"
	StatusInvestigating    IncidentStatus = "investigating"
	StatusAgentFixed       IncidentStatus = "agent_fixed"
	StatusManualTestNeeded IncidentStatus = "manual_test_needed"
	StatusResolved         IncidentStatus = "resolved"
	StatusReopened         IncidentStatus = "reopened"
	// StatusBlocked marks an incident the agent will not act on autonomously
	// (e.g. locked due to a suspected systemic failure) until the owner intervenes.
	StatusBlocked IncidentStatus = "blocked"
	// StatusVerifying marks an incident where a non-destructive fix was applied
	// and the system is waiting/re-checking whether it resolved the problem.
	StatusVerifying IncidentStatus = "verifying"
)

// Incident is a single tracked playback problem.
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

// CreateIncident inserts a new incident, setting ID and timestamps.
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

// GetIncident retrieves an incident by ID.
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
// this title so duplicate reports collapse into it. Comparison is
// case-insensitive and ignores trailing year suffixes like " (2024)".
// Returns ErrNotFound when no matching open incident exists.
func (d *DB) FindOpenByTitle(ctx context.Context, title string) (*Incident, error) {
	norm := d.normalizeTitle(title)
	row := d.sql.QueryRowContext(ctx, `
		SELECT id, created_at, updated_at, status, source, reported_by, what, title,
		       COALESCE(jellyfin_item_id,''), COALESCE(details,''),
		       COALESCE(finding,''), COALESCE(recommended_actions,''),
		       action_count, autonomous_locked
		FROM incidents
		WHERE (LOWER(title) = LOWER(?) OR LOWER(title) LIKE LOWER(?) || ' (%)')
		  AND status NOT IN ('resolved','reopened')
		ORDER BY created_at DESC LIMIT 1`, norm, norm)
	inc, err := scanIncident(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return inc, err
}

// ListIncidents returns paginated incidents, optionally filtered by status.
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

	var (
		out []*Incident
		inc *Incident
	)
	for rows.Next() {
		inc, err = scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// CountOpenIncidents returns the number of non-resolved incidents.
func (d *DB) CountOpenIncidents(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE status NOT IN ('resolved')`,
	).Scan(&n)
	return n, err
}

// UpdateIncidentStatus sets the status of an incident.
func (d *DB) UpdateIncidentStatus(ctx context.Context, id string, status IncidentStatus) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE incidents SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now(), id)
	return err
}

// TransitionStatus atomically sets an incident's status to `to`, but only when its
// current status is one of allowedFrom. It returns true iff this call actually
// performed the change. Callers gate side effects (notifications) on the result so
// that concurrent runs racing to the same terminal state notify exactly once: the
// first transition wins, and any later caller sees a status not in allowedFrom and
// gets false. Passing no allowedFrom transitions unconditionally.
func (d *DB) TransitionStatus(
	ctx context.Context, id string, to IncidentStatus, allowedFrom ...IncidentStatus,
) (bool, error) {
	args := []any{to, time.Now(), id}
	q := `UPDATE incidents SET status = ?, updated_at = ? WHERE id = ?`
	if len(allowedFrom) > 0 {
		placeholders := make([]string, len(allowedFrom))
		for i, s := range allowedFrom {
			placeholders[i] = "?"
			args = append(args, s)
		}
		//nolint:gosec // only literal "?" placeholders are concatenated; all status values are parameterized via args
		q += " AND status IN (" + strings.Join(placeholders, ",") + ")"
	}
	res, err := d.sql.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// SetIncidentFinding persists the diagnostic result for an incident.
func (d *DB) SetIncidentFinding(ctx context.Context, id string, finding, actions any) error {
	fb, _ := json.Marshal(finding)
	ab, _ := json.Marshal(actions)
	_, err := d.sql.ExecContext(ctx,
		`UPDATE incidents SET finding = ?, recommended_actions = ?, updated_at = ? WHERE id = ?`,
		string(fb), string(ab), time.Now(), id)
	return err
}

// IncrementActionCount atomically increments the action counter and returns the new value.
func (d *DB) IncrementActionCount(ctx context.Context, id string) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`UPDATE incidents SET action_count = action_count + 1, updated_at = ? WHERE id = ?
		 RETURNING action_count`,
		time.Now(), id).Scan(&n)
	return n, err
}

// SetAutonomousLocked sets or clears the autonomous-action lock on an incident.
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

// AddReporter records a reporter for an incident (INSERT OR IGNORE).
func (d *DB) AddReporter(ctx context.Context, incidentID, reporter, source, discordUserID string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO incident_reporters (incident_id, reporter, source, discord_user_id, reported_at)
		 VALUES (?, ?, ?, ?, ?)`,
		incidentID, reporter, source, nullStr(discordUserID), time.Now())
	return err
}

// ListReporters returns all reporter names for an incident.
func (d *DB) ListReporters(ctx context.Context, incidentID string) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT reporter FROM incident_reporters WHERE incident_id = ? ORDER BY reported_at`,
		incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		out []string
		r   string
	)
	for rows.Next() {
		err = rows.Scan(&r)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListDiscordReporterIDs returns the distinct Discord user IDs for an incident.
// A single user can end up with multiple incident_reporters rows (their
// display name/nickname differs between reports, or a retried /report
// interaction), so this groups by discord_user_id to guarantee each person
// is notified exactly once.
func (d *DB) ListDiscordReporterIDs(ctx context.Context, incidentID string) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT discord_user_id FROM incident_reporters
		 WHERE incident_id = ? AND discord_user_id IS NOT NULL AND discord_user_id != ''
		 GROUP BY discord_user_id
		 ORDER BY MIN(reported_at)`,
		incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		out []string
		id  string
	)
	for rows.Next() {
		err = rows.Scan(&id)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- Actions ---

// ActionStatus is the lifecycle state of a logged action.
type ActionStatus string

const (
	ActionPending  ActionStatus = "pending"
	ActionApplied  ActionStatus = "applied"
	ActionFailed   ActionStatus = "failed"
	ActionApproved ActionStatus = "approved"
	ActionDenied   ActionStatus = "denied"
)

// ActionLog records a single action taken (or proposed) for an incident.
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

// LogAction inserts an action record, generating an ID if absent. Callers
// (Dispatcher.logAction) only log an action after the underlying operation has
// already succeeded, so "now" is an accurate applied_at — there is no separate
// "pending" phase in this codebase's usage, unlike the applied_at/UpdateAction
// pair the schema was originally built for.
func (d *DB) LogAction(ctx context.Context, a *ActionLog) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	if a.AppliedAt == nil {
		now := time.Now()
		a.AppliedAt = &now
	}
	pb, _ := json.Marshal(a.Params)
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO actions_log (id, incident_id, action, params, triggered_by, status, applied_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.IncidentID, a.Action, string(pb), a.TriggeredBy, a.Status, a.AppliedAt)
	return err
}

// UpdateAction updates the status and result of an action.
func (d *DB) UpdateAction(ctx context.Context, id string, status ActionStatus, result, errMsg string) error {
	now := time.Now()
	_, err := d.sql.ExecContext(ctx,
		`UPDATE actions_log SET status = ?, applied_at = ?, result = ?, error = ? WHERE id = ?`,
		status, now, result, errMsg, id)
	return err
}

// ListActions returns all logged actions for an incident ordered by insertion.
func (d *DB) ListActions(ctx context.Context, incidentID string) ([]*ActionLog, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, incident_id, action, COALESCE(params,''), triggered_by, status,
		       applied_at, COALESCE(result,''), COALESCE(error,'')
		FROM actions_log WHERE incident_id = ? ORDER BY rowid`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		out       []*ActionLog
		paramsStr string
		appliedAt sql.NullTime
	)
	for rows.Next() {
		a := &ActionLog{}
		err = rows.Scan(&a.ID, &a.IncidentID, &a.Action, &paramsStr,
			&a.TriggeredBy, &a.Status, &appliedAt, &a.Result, &a.Error)
		if err != nil {
			return nil, err
		}
		if paramsStr != "" && paramsStr != nullJSON {
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

// GetSetting retrieves a settings value by key.
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, err
}

// SetSetting upserts a settings key/value pair.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// IsAutonomousPaused returns true when the autonomous_paused setting is "true".
func (d *DB) IsAutonomousPaused(ctx context.Context) (bool, error) {
	v, err := d.GetSetting(ctx, "autonomous_paused")
	if err != nil {
		return false, err
	}
	return v == "true", nil
}

// --- Conversation history ---

// SaveConversation upserts the serialised conversation for an incident.
func (d *DB) SaveConversation(ctx context.Context, incidentID string, data json.RawMessage) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO conversation_history (incident_id, messages, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(incident_id) DO UPDATE SET messages = excluded.messages, updated_at = excluded.updated_at`,
		incidentID, string(data), time.Now())
	return err
}

// LoadConversation retrieves the stored conversation for an incident.
// Returns ErrNotFound when no conversation has been saved yet.
func (d *DB) LoadConversation(ctx context.Context, incidentID string) (json.RawMessage, error) {
	var s string
	err := d.sql.QueryRowContext(ctx,
		`SELECT messages FROM conversation_history WHERE incident_id = ?`, incidentID,
	).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(s), nil
}

// FindByStatus returns all incidents in the given status (up to findByStatusLimit).
func (d *DB) FindByStatus(ctx context.Context, status IncidentStatus) ([]*Incident, error) {
	return d.ListIncidents(ctx, string(status), findByStatusLimit, 0)
}

// TouchHeartbeat records that an active agent run is still making progress.
// Called once per LLM round (Agent.logTurn) — best-effort, a failed write
// here should never abort a diagnosis.
func (d *DB) TouchHeartbeat(ctx context.Context, id string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE incidents SET last_heartbeat = ? WHERE id = ?`, time.Now(), id)
	return err
}

// FindStaleInvestigating returns incidents stuck in "investigating" whose
// heartbeat (or, if a run never got far enough to write one, updated_at —
// set when the run entered "investigating") is older than staleBefore.
// RecoverZombies only catches a crashed process (nothing left running to
// check); this catches a hung one — a run whose goroutine is still alive but
// stopped making progress, which zombie recovery cannot see at all.
func (d *DB) FindStaleInvestigating(ctx context.Context, staleBefore time.Time) ([]*Incident, error) {
	q := `SELECT id, created_at, updated_at, status, source, reported_by, what, title,
	             COALESCE(jellyfin_item_id,''), COALESCE(details,''),
	             COALESCE(finding,''), COALESCE(recommended_actions,''),
	             action_count, autonomous_locked
	      FROM incidents
	      WHERE status = ? AND COALESCE(last_heartbeat, updated_at) < ?`
	rows, err := d.sql.QueryContext(ctx, q, StatusInvestigating, staleBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Incident
	for rows.Next() {
		inc, scanErr := scanIncident(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// --- Pending outcome tracking (async fixes, e.g. arr_search_missing) ---

// PendingOutcome tracks an asynchronous fix from trigger through resolution.
// Currently only produced for arr_search_missing (a triggered
// search+download can take minutes to hours), but the shape isn't
// arr-specific. Season/Episode of -1 mean "not applicable" (movie, or a
// whole-series search), matching the sentinel used throughout the client/
// agent packages for the same reason.
type PendingOutcome struct {
	MediaType string `json:"media_type"`
	Title     string `json:"title"`
	Season    int    `json:"season"`
	Episode   int    `json:"episode"`

	StartedAt       time.Time `json:"started_at"`
	LastStage       string    `json:"last_stage,omitempty"`
	LastProgressAt  time.Time `json:"last_progress_at"`
	LastProgressPct float64   `json:"last_progress_pct"`

	// GrabNotified is set once the reporter has been told "found it,
	// downloading" — a one-time milestone DM, not a per-check spam.
	GrabNotified bool `json:"grab_notified"`
	// KeepSearching/KeepSearchingUntil are set by Service.KeepSearching after
	// an owner explicitly opts to keep watching for a release past the
	// initial grace period (e.g. brand-new content with no release yet).
	KeepSearching bool `json:"keep_searching"`
	// KeepSearchingUntil's zero value ("0001-01-01...") is emitted as-is when
	// KeepSearching is false — encoding/json's omitempty has no effect on a
	// struct-typed field (time.Time is never considered "empty"), so it's
	// omitted here rather than left in place implying an effect it doesn't have.
	KeepSearchingUntil time.Time `json:"keep_searching_until"`
}

// SetPendingOutcome upserts the pending-outcome state for an incident and
// schedules its next sweep check.
func (d *DB) SetPendingOutcome(ctx context.Context, id string, po *PendingOutcome, nextCheck time.Time) error {
	b, err := json.Marshal(po)
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx,
		`UPDATE incidents SET pending_outcome = ?, pending_outcome_next_check = ?, updated_at = ? WHERE id = ?`,
		string(b), nextCheck, time.Now(), id)
	return err
}

// ClearPendingOutcome removes any pending-outcome state for an incident
// (called once it resolves, so the sweeper stops touching it).
func (d *DB) ClearPendingOutcome(ctx context.Context, id string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE incidents SET pending_outcome = NULL, pending_outcome_next_check = NULL, updated_at = ? WHERE id = ?`,
		time.Now(), id)
	return err
}

// GetPendingOutcome returns the current pending-outcome state for an
// incident, or ErrNotFound if it has none.
func (d *DB) GetPendingOutcome(ctx context.Context, id string) (*PendingOutcome, error) {
	var s sql.NullString
	err := d.sql.QueryRowContext(ctx,
		`SELECT pending_outcome FROM incidents WHERE id = ?`, id,
	).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !s.Valid) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var po PendingOutcome
	if unmarshalErr := json.Unmarshal([]byte(s.String), &po); unmarshalErr != nil {
		return nil, unmarshalErr
	}
	return &po, nil
}

// FindDuePendingOutcomes returns incidents in StatusVerifying with a pending
// outcome whose next check time has passed. Scoped to StatusVerifying
// (rather than "has a pending_outcome at all") so an incident escalated to
// manual_test_needed after a no-release-found timeout stops being swept
// automatically — it only resumes once Service.KeepSearching explicitly
// re-arms it, which transitions status back to verifying.
func (d *DB) FindDuePendingOutcomes(ctx context.Context, before time.Time) ([]*Incident, error) {
	q := `SELECT id, created_at, updated_at, status, source, reported_by, what, title,
	             COALESCE(jellyfin_item_id,''), COALESCE(details,''),
	             COALESCE(finding,''), COALESCE(recommended_actions,''),
	             action_count, autonomous_locked
	      FROM incidents
	      WHERE status = ? AND pending_outcome IS NOT NULL AND pending_outcome_next_check <= ?`
	rows, err := d.sql.QueryContext(ctx, q, StatusVerifying, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Incident
	for rows.Next() {
		inc, scanErr := scanIncident(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// --- Disruption tracking ---

// Disruption is the most recent service-wide disruptive action recorded by
// RecordDisruption.
type Disruption struct {
	Action     string
	IncidentID string
	At         time.Time
}

// RecordDisruption upserts the single last_disruption row. Called whenever a
// service-wide action (restart, scan, sweep, cleanup — see
// Dispatcher.disruptiveActions) is logged, so any run starting shortly after
// can be warned it may be looking at that disruption's side effects rather
// than its own incident's root cause.
func (d *DB) RecordDisruption(ctx context.Context, action, incidentID string) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO last_disruption (id, action, incident_id, at) VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET action = excluded.action, incident_id = excluded.incident_id,
		                               at = excluded.at`,
		action, incidentID, time.Now())
	return err
}

// LastDisruption returns the most recent recorded disruption, or ErrNotFound
// if none has ever been recorded.
func (d *DB) LastDisruption(ctx context.Context) (*Disruption, error) {
	var disp Disruption
	err := d.sql.QueryRowContext(ctx,
		`SELECT action, incident_id, at FROM last_disruption WHERE id = 1`,
	).Scan(&disp.Action, &disp.IncidentID, &disp.At)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &disp, nil
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
	if findingStr != "" && findingStr != nullJSON {
		_ = json.Unmarshal([]byte(findingStr), &inc.Finding)
	}
	if actionsStr != "" && actionsStr != nullJSON {
		_ = json.Unmarshal([]byte(actionsStr), &inc.RecommendedActions)
	}
	return inc, nil
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

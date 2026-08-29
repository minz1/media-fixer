package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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

// readPoolSize bounds concurrent readers (dashboard pages, SSE streams, the
// live-check suite). SQLite's WAL mode lets any number of readers run
// alongside the single writer without blocking each other; this just caps
// how many physical connections the read pool opens for a single-user local
// admin tool where a handful is already generous headroom.
const readPoolSize = 4

// busyTimeoutMS is how long a connection waits on SQLITE_BUSY before
// returning an error, applied via the DSN so every physical connection in
// both pools gets it (see applyQueryParams in modernc.org/sqlite: pragmas
// passed via query params are re-applied on every new connection, not just
// the first).
const busyTimeoutMS = 5000

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

// DB wraps two [sql.DB] pools over the same SQLite file with schema
// management and typed query methods. Splitting write/read matters because
// SQLite only ever allows one writer at a time — write is capped at one
// connection for that reason — while WAL mode (see dsn) lets any number of
// readers run concurrently with that writer without blocking. Before this,
// a single one-connection pool serialized everything, including long-lived
// SSE readers, behind the agent's own writes.
type DB struct {
	write  *sql.DB
	read   *sql.DB
	yearRE *regexp.Regexp
}

// dsn builds the SQLite connection string for path. _pragma values use
// modernc.org/sqlite's own syntax (`_pragma=name(value)`), not
// mattn/go-sqlite3's `?_journal_mode=...&_foreign_keys=...` — the two
// drivers' DSN query keys don't overlap, and unknown keys are silently
// ignored by modernc.org/sqlite rather than erroring, so a mismatch here
// fails silent, not loud. See applyQueryParams in the vendored driver.
func dsn(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(" +
		strconv.Itoa(busyTimeoutMS) + ")"
}

// Open creates or opens the SQLite database at path, applying the schema and
// running any migrations that haven't been applied yet.
func Open(path string) (*DB, error) {
	ctx := context.Background()
	d := dsn(path)

	write, err := sql.Open("sqlite", d)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s (write): %w", path, err)
	}
	write.SetMaxOpenConns(1) // SQLite permits exactly one writer regardless of pool size

	read, err := sql.Open("sqlite", d)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("open sqlite %s (read): %w", path, err)
	}
	read.SetMaxOpenConns(readPoolSize)

	if err = migrate(ctx, write); err != nil {
		_ = write.Close()
		_ = read.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}

	return &DB{
		write:  write,
		read:   read,
		yearRE: regexp.MustCompile(`\s*\(\d{4}\)\s*$`),
	}, nil
}

// Close closes both underlying connection pools.
func (d *DB) Close() error {
	writeErr := d.write.Close()
	readErr := d.read.Close()
	if writeErr != nil {
		return writeErr
	}
	return readErr
}

// runTx runs fn inside a write transaction on conn, committing on success and
// rolling back on any error (including a panic, which is re-thrown after
// rollback). All multi-statement writes — a migration, or (from
// internal/journal) an event append plus its projection — go through this
// rather than issuing bare sequential ExecContext calls, so a crash or error
// mid-sequence can never leave the database in a state no single write ever
// asked for. A free function (not a *DB method) because migrate runs before
// a *DB exists — it only has the raw write pool.
func runTx(ctx context.Context, conn *sql.DB, fn func(ctx context.Context, tx *sql.Tx) error) (err error) {
	txn, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = txn.Rollback()
			panic(p)
		}
		if err != nil {
			_ = txn.Rollback()
			return
		}
		err = txn.Commit()
	}()
	err = fn(ctx, txn)
	return err
}

// normalizeTitle strips trailing " (YYYY)" year suffixes for fuzzy deduplication.
func (d *DB) normalizeTitle(title string) string {
	return strings.TrimSpace(d.yearRE.ReplaceAllString(title, ""))
}

// --- Schema migrations ---
//
// Migrations 1-6 recreate, verbatim, what Open used to do unconditionally on
// every startup: apply the base schema, then four ALTER TABLE ADD COLUMNs,
// then a one-time dedup + unique index. There was no schema_version table
// before this, so the production database already has all six applied but
// has never recorded that fact anywhere. Each carries an "already" check —
// a structural marker (does the table/column/index exist) rather than a
// trusted assumption — so bootstrapping against that database reconciles
// correctly regardless of exactly which subset was actually applied,
// without needing to guess a baseline version number. New migrations added
// after this system exists (see internal/journal) need no "already" check:
// schema_version alone is authoritative for them.
const (
	migBaseSchema                       = 1
	migReportersDiscordUserID           = 2
	migIncidentsLastHeartbeat           = 3
	migIncidentsPendingOutcome          = 4
	migIncidentsPendingOutcomeNextCheck = 5
	migReportersDedupDiscordIndex       = 6
	migIncidentEvents                   = 7
	migBackfillIncidentEvents           = 8
	migDropLastHeartbeat                = 9
	migDropConversationHistory          = 10
	migDropLastDisruption               = 11
)

type migration struct {
	version int
	name    string
	// already reports whether this migration's effect is already present in
	// the database, for reconciling a pre-schema_version (legacy) database.
	// nil means "not applicable" — always run exec, relying on its own
	// idempotent SQL (IF NOT EXISTS, etc.); used for genuinely new
	// migrations where schema_version is trusted outright.
	already func(ctx context.Context, conn *sql.DB) (bool, error)
	exec    func(ctx context.Context, tx *sql.Tx) error
}

// migrations returns the full ordered list: the six legacy, pre-schema_version
// migrations (see legacyMigrations) followed by the five that introduced the
// event log (see eventLogMigrations). Split into two functions purely to stay
// under a reasonable single-function length — they're one logical list.
func migrations() []migration {
	return append(legacyMigrations(), eventLogMigrations()...)
}

func legacyMigrations() []migration {
	return []migration{
		{
			version: migBaseSchema,
			name:    "base_schema",
			already: func(ctx context.Context, conn *sql.DB) (bool, error) { return tableExists(ctx, conn, "incidents") },
			exec: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, schema)
				return err
			},
		},
		{
			version: migReportersDiscordUserID,
			name:    "incident_reporters_discord_user_id",
			already: func(ctx context.Context, conn *sql.DB) (bool, error) {
				return columnExists(ctx, conn, "incident_reporters", "discord_user_id")
			},
			exec: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `ALTER TABLE incident_reporters ADD COLUMN discord_user_id TEXT`)
				return err
			},
		},
		{
			version: migIncidentsLastHeartbeat,
			name:    "incidents_last_heartbeat",
			already: func(ctx context.Context, conn *sql.DB) (bool, error) {
				return columnExists(ctx, conn, "incidents", "last_heartbeat")
			},
			exec: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `ALTER TABLE incidents ADD COLUMN last_heartbeat DATETIME`)
				return err
			},
		},
		{
			version: migIncidentsPendingOutcome,
			name:    "incidents_pending_outcome",
			already: func(ctx context.Context, conn *sql.DB) (bool, error) {
				return columnExists(ctx, conn, "incidents", "pending_outcome")
			},
			exec: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `ALTER TABLE incidents ADD COLUMN pending_outcome TEXT`)
				return err
			},
		},
		{
			version: migIncidentsPendingOutcomeNextCheck,
			name:    "incidents_pending_outcome_next_check",
			already: func(ctx context.Context, conn *sql.DB) (bool, error) {
				return columnExists(ctx, conn, "incidents", "pending_outcome_next_check")
			},
			exec: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `ALTER TABLE incidents ADD COLUMN pending_outcome_next_check DATETIME`)
				return err
			},
		},
		{
			// A Discord user's identity for notification is discord_user_id, not
			// the display-name text in the PK. This enforces one reporter row per
			// (incident, Discord user) structurally so every reader dedups for
			// free; the DELETE clears any pre-existing duplicates first, since the
			// unique index fails to build otherwise.
			version: migReportersDedupDiscordIndex,
			name:    "incident_reporters_dedup_discord_index",
			already: func(ctx context.Context, conn *sql.DB) (bool, error) {
				return indexExists(ctx, conn, "idx_reporters_discord")
			},
			exec: func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, dedupReportersByDiscordID); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, createReporterDiscordIndex)
				return err
			},
		},
	}
}

// eventLogMigrations replace four hand-rolled, single-purpose mechanisms
// (last_disruption, last_heartbeat, conversation_history, and
// actions_log-re-reading for attempt history) with one append-only event log
// that backs all of them plus the live dashboard, the full thought-process
// transcript, and the JSON export (see internal/journal). None of these five
// need an "already" check — unlike legacyMigrations, they postdate
// schema_version, so it alone is authoritative for whether they've run.
func eventLogMigrations() []migration {
	return []migration{
		{
			version: migIncidentEvents,
			name:    "incident_events",
			exec: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, incidentEventsSchema)
				return err
			},
		},
		{
			// Recovers what's actually recoverable from the pre-event-log schema:
			// incident_created from incidents.created_at, reporter_added from
			// incident_reporters, action_applied from actions_log (this alone is
			// enough for the collapsed last_disruption query to keep working over
			// historical data — see journal.LastDisruption), and one
			// conversation_imported per incident that had a conversation_history
			// row. conversation_history is keyed incident_id PRIMARY KEY and
			// upserted, so every rerun already destroyed the previous run's
			// conversation — only the latest one survives to backfill; per-round
			// history for old incidents was never recoverable. Tagged
			// source='backfill' throughout so the UI can label these timestamps
			// approximate rather than claim a precision the data doesn't have.
			version: migBackfillIncidentEvents,
			name:    "backfill_incident_events",
			exec: func(ctx context.Context, tx *sql.Tx) error {
				for _, q := range backfillIncidentEventsSQL() {
					if _, err := tx.ExecContext(ctx, q); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			version: migDropLastHeartbeat,
			name:    "drop_last_heartbeat",
			exec: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `ALTER TABLE incidents DROP COLUMN last_heartbeat`)
				return err
			},
		},
		{
			version: migDropConversationHistory,
			name:    "drop_conversation_history",
			exec: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `DROP TABLE conversation_history`)
				return err
			},
		},
		{
			version: migDropLastDisruption,
			name:    "drop_last_disruption",
			exec: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `DROP TABLE last_disruption`)
				return err
			},
		},
	}
}

const incidentEventsSchema = `
CREATE TABLE IF NOT EXISTS incident_events (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	incident_id TEXT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
	at          DATETIME NOT NULL,
	kind        TEXT NOT NULL,
	payload     TEXT NOT NULL,
	source      TEXT NOT NULL DEFAULT 'live'
);
CREATE INDEX IF NOT EXISTS idx_events_incident_seq ON incident_events(incident_id, seq);
CREATE INDEX IF NOT EXISTS idx_events_kind_at ON incident_events(kind, at);
`

// backfillIncidentEventsSQL recreates, from the tables that held it, every
// event kind that can honestly be reconstructed from pre-event-log data. Run
// once by the backfill_incident_events migration, in its own transaction, while
// conversation_history and last_disruption still exist (both are dropped by
// later migrations in the same list).
func backfillIncidentEventsSQL() []string {
	return []string{
		`INSERT INTO incident_events (incident_id, at, kind, payload, source)
	 SELECT id, created_at, 'incident_created',
	        json_object('source', source, 'reported_by', reported_by, 'what', what, 'title', title),
	        'backfill'
	 FROM incidents ORDER BY created_at`,

		`INSERT INTO incident_events (incident_id, at, kind, payload, source)
	 SELECT incident_id, reported_at, 'reporter_added',
	        json_object('reporter', reporter, 'source', source, 'discord_user_id', discord_user_id),
	        'backfill'
	 FROM incident_reporters ORDER BY reported_at`,

		`INSERT INTO incident_events (incident_id, at, kind, payload, source)
	 SELECT incident_id, COALESCE(applied_at, CURRENT_TIMESTAMP), 'action_applied',
	        json_object('action', action, 'params', json(COALESCE(params, 'null')),
	                     'triggered_by', triggered_by, 'status', status,
	                     'result', COALESCE(result, ''), 'error', COALESCE(error, '')),
	        'backfill'
	 FROM actions_log ORDER BY applied_at`,

		`INSERT INTO incident_events (incident_id, at, kind, payload, source)
	 SELECT incident_id, updated_at, 'conversation_imported', json_object('messages', json(messages)), 'backfill'
	 FROM conversation_history ORDER BY updated_at`,
	}
}

const createSchemaVersionTable = `
CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

// migrate applies every migration not yet recorded in schema_version, each
// in its own transaction (so a failure partway through never leaves a
// migration half-applied and unrecorded, which would otherwise make it
// silently re-run — and half-apply again — on the next restart).
func migrate(ctx context.Context, write *sql.DB) error {
	if _, err := write.ExecContext(ctx, createSchemaVersionTable); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	applied, err := loadAppliedVersions(ctx, write)
	if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}

	for _, m := range migrations() {
		if applied[m.version] {
			continue
		}
		if applyErr := applyMigration(ctx, write, m); applyErr != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, applyErr)
		}
	}
	return nil
}

// loadAppliedVersions returns the set of migration versions already
// recorded in schema_version.
func loadAppliedVersions(ctx context.Context, write *sql.DB) (map[int]bool, error) {
	rows, err := write.QueryContext(ctx, `SELECT version FROM schema_version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if scanErr := rows.Scan(&v); scanErr != nil {
			return nil, scanErr
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// applyMigration runs one migration's exec (unless its "already" check finds
// the effect already present) and records it in schema_version, all inside a
// single transaction.
func applyMigration(ctx context.Context, write *sql.DB, m migration) error {
	skipExec := false
	if m.already != nil {
		ok, err := m.already(ctx, write)
		if err != nil {
			return fmt.Errorf("check already-applied: %w", err)
		}
		skipExec = ok
	}

	// A legacy database's very first reconciliation is also the first time
	// foreign-key enforcement has ever been turned on for it (Open's DSN
	// never actually enabled it before — see dsn's doc comment). Verify
	// there's nothing for the newly-enforced ON DELETE CASCADEs to trip over,
	// rather than assume a clean history.
	if m.version == migBaseSchema && skipExec {
		if err := checkNoForeignKeyViolations(ctx, write); err != nil {
			return err
		}
	}

	return runTx(ctx, write, func(ctx context.Context, tx *sql.Tx) error {
		if !skipExec {
			if err := m.exec(ctx, tx); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_version (version, name) VALUES (?, ?)`, m.version, m.name)
		return err
	})
}

// checkNoForeignKeyViolations fails loudly if the database has rows that
// violate a foreign key now being enforced for the first time, rather than
// silently letting SQLite start rejecting writes that touch them.
func checkNoForeignKeyViolations(ctx context.Context, conn *sql.DB) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New(
			"foreign_key_check found existing violations; " +
				"resolve orphaned rows before enabling foreign key enforcement")
	}
	return rows.Err()
}

// tableExists reports whether a table with this name exists in the database.
func tableExists(ctx context.Context, conn *sql.DB, name string) (bool, error) {
	var n int
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n)
	return n > 0, err
}

// indexExists reports whether an index with this name exists in the database.
func indexExists(ctx context.Context, conn *sql.DB, name string) (bool, error) {
	var n int
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name,
	).Scan(&n)
	return n > 0, err
}

// columnExists reports whether table has a column with this name, via
// PRAGMA table_info rather than trying the ALTER and pattern-matching the
// driver's "duplicate column name" error string. table is always one of a
// fixed set of internal literals passed by migrations above, never user
// input, so building the PRAGMA statement by concatenation is safe.
func columnExists(ctx context.Context, conn *sql.DB, table, column string) (bool, error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return false, err
	}
	dest := make([]any, len(cols))
	nameIdx := -1
	for i, c := range cols {
		if c == "name" {
			nameIdx = i
		}
		dest[i] = new(any)
	}
	if nameIdx < 0 {
		return false, errors.New("PRAGMA table_info: no name column in result")
	}
	for rows.Next() {
		if scanErr := rows.Scan(dest...); scanErr != nil {
			return false, scanErr
		}
		if v, ok := (*dest[nameIdx].(*any)).(string); ok && v == column {
			return true, nil
		}
	}
	return false, rows.Err()
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

	_, err := d.write.ExecContext(ctx, `
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
	row := d.read.QueryRowContext(ctx, `
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
	row := d.read.QueryRowContext(ctx, `
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

	rows, err := d.read.QueryContext(ctx, q, args...)
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
	err := d.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE status NOT IN ('resolved')`,
	).Scan(&n)
	return n, err
}

// UpdateIncidentStatus sets the status of an incident.
func (d *DB) UpdateIncidentStatus(ctx context.Context, id string, status IncidentStatus) error {
	_, err := d.write.ExecContext(ctx,
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
	res, err := d.write.ExecContext(ctx, q, args...)
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
	_, err := d.write.ExecContext(ctx,
		`UPDATE incidents SET finding = ?, recommended_actions = ?, updated_at = ? WHERE id = ?`,
		string(fb), string(ab), time.Now(), id)
	return err
}

// IncrementActionCount atomically increments the action counter and returns the new value.
func (d *DB) IncrementActionCount(ctx context.Context, id string) (int, error) {
	var n int
	err := d.write.QueryRowContext(ctx,
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
	_, err := d.write.ExecContext(ctx,
		`UPDATE incidents SET autonomous_locked = ?, updated_at = ? WHERE id = ?`,
		v, time.Now(), id)
	return err
}

// --- Reporters ---

// AddReporter records a reporter for an incident (INSERT OR IGNORE).
func (d *DB) AddReporter(ctx context.Context, incidentID, reporter, source, discordUserID string) error {
	_, err := d.write.ExecContext(ctx,
		`INSERT OR IGNORE INTO incident_reporters (incident_id, reporter, source, discord_user_id, reported_at)
		 VALUES (?, ?, ?, ?, ?)`,
		incidentID, reporter, source, nullStr(discordUserID), time.Now())
	return err
}

// ListReporters returns all reporter names for an incident.
func (d *DB) ListReporters(ctx context.Context, incidentID string) ([]string, error) {
	rows, err := d.read.QueryContext(ctx,
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
	rows, err := d.read.QueryContext(ctx,
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
// only log an action after the underlying operation has already succeeded,
// so "now" is an accurate applied_at — there is no separate "pending" phase
// in this codebase's usage, unlike the applied_at/UpdateAction pair the
// schema was originally built for. Production code now writes actions_log
// exclusively via internal/journal (an action_applied event's projection,
// written atomically with the event via InsertActionLog below) — this
// remains for direct/test use as a plain standalone write.
func (d *DB) LogAction(ctx context.Context, a *ActionLog) error {
	return d.RunTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return InsertActionLog(ctx, tx, a)
	})
}

// InsertActionLog inserts one actions_log row within tx, generating an ID and
// applied_at if absent. A free function (not a *DB method) so
// internal/journal can call it from inside its own RunTx closure, writing an
// action_applied event and its actions_log projection atomically — both
// succeed or both roll back.
func InsertActionLog(ctx context.Context, tx *sql.Tx, a *ActionLog) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	if a.AppliedAt == nil {
		now := time.Now()
		a.AppliedAt = &now
	}
	pb, _ := json.Marshal(a.Params)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO actions_log (id, incident_id, action, params, triggered_by, status, applied_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.IncidentID, a.Action, string(pb), a.TriggeredBy, a.Status, a.AppliedAt)
	return err
}

// UpdateAction updates the status and result of an action.
func (d *DB) UpdateAction(ctx context.Context, id string, status ActionStatus, result, errMsg string) error {
	now := time.Now()
	_, err := d.write.ExecContext(ctx,
		`UPDATE actions_log SET status = ?, applied_at = ?, result = ?, error = ? WHERE id = ?`,
		status, now, result, errMsg, id)
	return err
}

// ListActions returns all logged actions for an incident ordered by insertion.
func (d *DB) ListActions(ctx context.Context, incidentID string) ([]*ActionLog, error) {
	rows, err := d.read.QueryContext(ctx, `
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
	err := d.read.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, err
}

// SetSetting upserts a settings key/value pair.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.write.ExecContext(ctx,
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

// FindByStatus returns all incidents in the given status (up to findByStatusLimit).
func (d *DB) FindByStatus(ctx context.Context, status IncidentStatus) ([]*Incident, error) {
	return d.ListIncidents(ctx, string(status), findByStatusLimit, 0)
}

// FindStaleInvestigating returns incidents stuck in "investigating" whose most
// recent incident_events activity (or, if nothing has been logged yet,
// updated_at — set when the run entered "investigating") is older than
// staleBefore. Formerly driven by a dedicated last_heartbeat column touched
// once per LLM round; any event append now serves the same purpose (see
// internal/journal), so no separate heartbeat write exists — this is a plain
// read over incidents joined against incident_events, both this package's own
// tables. RecoverZombies only catches a crashed process (nothing left running
// to check); this catches a hung one — a run whose goroutine is still alive
// but stopped making progress, which zombie recovery cannot see at all.
func (d *DB) FindStaleInvestigating(ctx context.Context, staleBefore time.Time) ([]*Incident, error) {
	q := `SELECT i.id, i.created_at, i.updated_at, i.status, i.source, i.reported_by, i.what, i.title,
	             COALESCE(i.jellyfin_item_id,''), COALESCE(i.details,''),
	             COALESCE(i.finding,''), COALESCE(i.recommended_actions,''),
	             i.action_count, i.autonomous_locked
	      FROM incidents i
	      LEFT JOIN (
	        SELECT incident_id, MAX(at) AS last_activity FROM incident_events GROUP BY incident_id
	      ) e ON e.incident_id = i.id
	      WHERE i.status = ? AND COALESCE(e.last_activity, i.updated_at) < ?`
	rows, err := d.read.QueryContext(ctx, q, StatusInvestigating, staleBefore)
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
	_, err = d.write.ExecContext(ctx,
		`UPDATE incidents SET pending_outcome = ?, pending_outcome_next_check = ?, updated_at = ? WHERE id = ?`,
		string(b), nextCheck, time.Now(), id)
	return err
}

// ClearPendingOutcome removes any pending-outcome state for an incident
// (called once it resolves, so the sweeper stops touching it).
func (d *DB) ClearPendingOutcome(ctx context.Context, id string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE incidents SET pending_outcome = NULL, pending_outcome_next_check = NULL, updated_at = ? WHERE id = ?`,
		time.Now(), id)
	return err
}

// GetPendingOutcome returns the current pending-outcome state for an
// incident, or ErrNotFound if it has none.
func (d *DB) GetPendingOutcome(ctx context.Context, id string) (*PendingOutcome, error) {
	var s sql.NullString
	err := d.read.QueryRowContext(ctx,
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
	rows, err := d.read.QueryContext(ctx, q, StatusVerifying, before)
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

// --- Events (append-only incident_events log; see internal/journal) ---
//
// db only stores and retrieves event rows — internal/journal owns what each
// Kind means, how its payload is shaped, and what projection (if any) an
// append also writes (e.g. an action_applied event's actions_log row). This
// table replaces last_disruption, last_heartbeat, and conversation_history;
// see the migBackfillIncidentEvents migration above for how existing data
// carried over.

// Event is one row of incident_events.
type Event struct {
	Seq        int64           `json:"seq"`
	IncidentID string          `json:"incident_id"`
	At         time.Time       `json:"at"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	Source     string          `json:"source"`
}

// AppendEvent inserts one event row within tx (see (*DB).RunTx) and returns
// its assigned seq. A free function, not a *DB method, so a caller composing
// an event insert with a projection write (both must land in the same
// transaction) isn't tempted to reach for a second, unrelated *DB write pool
// mid-transaction.
func AppendEvent(ctx context.Context, tx *sql.Tx, e *Event) (int64, error) {
	if e.Source == "" {
		e.Source = "live"
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO incident_events (incident_id, at, kind, payload, source) VALUES (?, ?, ?, ?, ?)`,
		e.IncidentID, e.At, e.Kind, string(e.Payload), e.Source)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EventsSince returns every event for incidentID with seq > afterSeq, in seq
// order — the full history when afterSeq is 0. Backs both the live SSE
// stream's replay-on-reconnect (afterSeq = the client's Last-Event-ID) and a
// full-history read (the transcript page, the JSON export).
func (d *DB) EventsSince(ctx context.Context, incidentID string, afterSeq int64) ([]*Event, error) {
	rows, err := d.read.QueryContext(ctx, `
		SELECT seq, incident_id, at, kind, payload, source
		FROM incident_events WHERE incident_id = ? AND seq > ? ORDER BY seq`,
		incidentID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// EventsByKind returns the most recent events of a given kind across every
// incident, newest first, up to limit. Used for queries that aren't scoped to
// one incident — e.g. journal.LastDisruption, which needs the latest
// disruptive action_applied event regardless of which incident logged it.
// Filtering on anything inside payload (e.g. "was this one disruptive") is
// left to the caller — db has no opinion on what a kind's payload means.
func (d *DB) EventsByKind(ctx context.Context, kind string, limit int) ([]*Event, error) {
	rows, err := d.read.QueryContext(ctx, `
		SELECT seq, incident_id, at, kind, payload, source
		FROM incident_events WHERE kind = ? ORDER BY seq DESC LIMIT ?`,
		kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]*Event, error) {
	var out []*Event
	for rows.Next() {
		e := &Event{}
		var payload string
		if err := rows.Scan(&e.Seq, &e.IncidentID, &e.At, &e.Kind, &payload, &e.Source); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RunTx runs fn inside a write transaction against d's write pool. Exported
// for internal/journal, which composes an event insert with its projection
// write (e.g. the actions_log row an action_applied event backs) atomically —
// both succeed or both roll back, so the log and its derived read-optimized
// tables can never drift apart.
func (d *DB) RunTx(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	return runTx(ctx, d.write, fn)
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

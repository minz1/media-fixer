package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/operation"
)

const migOperations = 22

const operationsTableDDL = `CREATE TABLE operations (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	subject_kind TEXT     NOT NULL CHECK (subject_kind IN ('incident', 'download')),
	subject_id   TEXT     NOT NULL CHECK (length(subject_id) BETWEEN 1 AND 200),
	kind         TEXT     NOT NULL CHECK (kind IN ('replace', 'torrent_readd', 'regrab')),
	title        TEXT     NOT NULL DEFAULT '',
	source       TEXT     NOT NULL CHECK (json_valid(source)),
	steps        TEXT     NOT NULL CHECK (json_valid(steps) AND json_type(steps) = 'array'
	                                      AND json_array_length(steps) BETWEEN 1 AND 2000),
	results      TEXT     NOT NULL CHECK (json_valid(results) AND json_type(results) = 'array'),
	step_index   INTEGER  NOT NULL CHECK (step_index >= 0),
	status       TEXT     NOT NULL CHECK (status IN ('running', 'done', 'failed_at_step')),
	cause        TEXT     NOT NULL DEFAULT '' CHECK (cause IN ('', 'error', 'restart', 'store')),
	version      INTEGER  NOT NULL DEFAULT 0 CHECK (version >= 0),
	created_at   DATETIME NOT NULL,
	updated_at   DATETIME NOT NULL,
	CONSTRAINT operations_results_len CHECK (json_array_length(results) = json_array_length(steps)),
	CONSTRAINT operations_index_range CHECK (step_index < json_array_length(steps)),
	CONSTRAINT operations_cause_iff_failed CHECK ((status = 'failed_at_step') = (cause <> '')),
	CONSTRAINT operations_done_at_last CHECK (status <> 'done' OR step_index = json_array_length(steps) - 1),
	CONSTRAINT operations_regrab_download CHECK ((kind = 'regrab') = (subject_kind = 'download'))
)`

const operationsSubjectIndexDDL = `CREATE INDEX operations_subject ON operations (subject_kind, subject_id, id)`

const operationsOneRunningDDL = `CREATE UNIQUE INDEX operations_one_running
	ON operations (subject_kind, subject_id) WHERE status = 'running'`

func operationsMigrations() []migration {
	return []migration{{
		version: migOperations,
		name:    "operations",
		exec: func(ctx context.Context, tx *sql.Tx) error {
			for _, q := range []string{operationsTableDDL, operationsSubjectIndexDDL, operationsOneRunningDDL} {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			return nil
		},
	}}
}

const operationColumns = `id, subject_kind, subject_id, kind, title, source, steps, results, step_index, status,
	cause, version, created_at, updated_at`

type operationRaw struct {
	subjectKind, subjectID, kind, status, cause string
	source, steps, results                      string
}

func scanOperation(sc scanner) (operation.Op, error) {
	var (
		op operation.Op
		r  operationRaw
	)
	err := sc.Scan(&op.ID, &r.subjectKind, &r.subjectID, &r.kind, &op.Title, &r.source, &r.steps, &r.results,
		&op.Index, &r.status, &r.cause, &op.Version, &op.CreatedAt, &op.UpdatedAt)
	if err != nil {
		return op, err
	}
	op.Subject = operation.Subject{Kind: operation.SubjectKind(r.subjectKind), ID: r.subjectID}
	op.Kind, op.Status, op.Cause = operation.Kind(r.kind), operation.Status(r.status), operation.Cause(r.cause)
	op.Source = json.RawMessage(r.source)
	if err = json.Unmarshal([]byte(r.steps), &op.Steps); err != nil {
		return op, fmt.Errorf("operation %d: decode steps: %w", op.ID, err)
	}
	if err = json.Unmarshal([]byte(r.results), &op.Results); err != nil {
		return op, fmt.Errorf("operation %d: decode results: %w", op.ID, err)
	}
	return op, nil
}

func loadOperation(ctx context.Context, q querier, id int64) (operation.Op, error) {
	op, err := scanOperation(q.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return operation.Op{}, ErrNotFound
	}
	return op, err
}

func insertOperation(ctx context.Context, tx *sql.Tx, op operation.Op) (int64, error) {
	steps, err := json.Marshal(op.Steps)
	if err != nil {
		return 0, err
	}
	results, err := json.Marshal(op.Results)
	if err != nil {
		return 0, err
	}
	source := string(op.Source)
	if source == "" {
		source = nullJSON
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO operations (subject_kind, subject_id, kind, title, source, steps,
		results, step_index, status, cause, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(op.Subject.Kind), op.Subject.ID, string(op.Kind), op.Title, source, string(steps), string(results),
		op.Index, string(op.Status), string(op.Cause), op.Version, op.CreatedAt, op.UpdatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func updateOperation(ctx context.Context, tx *sql.Tx, op operation.Op) error {
	results, err := json.Marshal(op.Results)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE operations SET results = ?, step_index = ?, status = ?, cause = ?,
		version = ?, updated_at = ? WHERE id = ?`,
		string(results), op.Index, string(op.Status), string(op.Cause), op.Version, op.UpdatedAt, op.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return fmt.Errorf("operation %d: update touched %d rows", op.ID, n)
	}
	return err
}

func (d *DB) ApplyOperation(
	ctx context.Context,
	id int64,
	decide func(operation.Op) (operation.Op, error),
) (operation.Op, error) {
	var out operation.Op
	err := runTx(ctx, d.write, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		out, err = applyOperationTx(ctx, tx, id, decide)
		return err
	})
	if err != nil {
		d.log.WarnContext(ctx, "operation change refused", "op_id", id, "error", err)
		return operation.Op{}, err
	}
	return out, nil
}

func applyOperationTx(
	ctx context.Context,
	tx *sql.Tx,
	id int64,
	decide func(operation.Op) (operation.Op, error),
) (operation.Op, error) {
	var cur operation.Op
	if id != 0 {
		var err error
		if cur, err = loadOperation(ctx, tx, id); err != nil {
			return operation.Op{}, err
		}
	}
	next, err := decide(cur)
	if err != nil {
		return operation.Op{}, err
	}
	if err = operation.Check(next); err != nil {
		return operation.Op{}, err
	}
	return writeOperation(ctx, tx, cur, next)
}

func writeOperation(ctx context.Context, tx *sql.Tx, cur, next operation.Op) (operation.Op, error) {
	if cur.ID == 0 {
		return createOperation(ctx, tx, next)
	}
	if next.ID != cur.ID || next.Version != cur.Version+1 {
		return operation.Op{}, fmt.Errorf("operation %d: version %d to %d, id %d", cur.ID, cur.Version, next.Version,
			next.ID)
	}
	return next, updateOperation(ctx, tx, next)
}

func createOperation(ctx context.Context, tx *sql.Tx, next operation.Op) (operation.Op, error) {
	if next.ID != 0 {
		return operation.Op{}, fmt.Errorf("operation: a new op arrived with id %d", next.ID)
	}
	id, err := insertOperation(ctx, tx, next)
	next.ID = id
	return next, err
}

func (d *DB) Operation(ctx context.Context, id int64) (operation.Op, error) {
	return loadOperation(ctx, d.read, id)
}

func (d *DB) LatestOperation(ctx context.Context, subject operation.Subject) (operation.Op, error) {
	op, err := scanOperation(d.read.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations
		WHERE subject_kind = ? AND subject_id = ? ORDER BY id DESC LIMIT 1`, string(subject.Kind), subject.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return operation.Op{}, ErrNotFound
	}
	return op, err
}

type opQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryOperations(ctx context.Context, q opQuerier, where string, args ...any) ([]operation.Op, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []operation.Op
	for rows.Next() {
		op, scanErr := scanOperation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (d *DB) ResumableDownloadOperations(ctx context.Context) ([]operation.Op, error) {
	ops, err := queryOperations(ctx, d.read, `subject_kind = 'download' AND id IN
		(SELECT MAX(id) FROM operations WHERE subject_kind = 'download' GROUP BY subject_id)`)
	if err != nil {
		return nil, err
	}
	var out []operation.Op
	for _, op := range ops {
		if operation.Resumable(op) {
			out = append(out, op)
		}
	}
	return out, nil
}

func (d *DB) InterruptRunningOperations(ctx context.Context, now time.Time) ([]operation.Op, error) {
	var out []operation.Op
	err := runTx(ctx, d.write, func(ctx context.Context, tx *sql.Tx) error {
		running, err := queryOperations(ctx, tx, `status = 'running'`)
		if err != nil {
			return err
		}
		ev := operation.Event{Kind: operation.Interrupted, At: now, Cause: operation.CauseRestart}
		for _, op := range running {
			next, nextErr := interruptOne(ctx, tx, op, ev)
			if nextErr != nil {
				return nextErr
			}
			out = append(out, next)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func interruptOne(ctx context.Context, tx *sql.Tx, op operation.Op, ev operation.Event) (operation.Op, error) {
	next, err := operation.Next(op, ev)
	if err != nil {
		return operation.Op{}, fmt.Errorf("operation %d: %w", op.ID, err)
	}
	if err = operation.Check(next); err != nil {
		return operation.Op{}, err
	}
	return next, updateOperation(ctx, tx, next)
}

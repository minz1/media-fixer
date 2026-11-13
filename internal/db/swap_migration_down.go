package db

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/operation"
)

const swapDownJournal = `INSERT INTO incident_events (incident_id, at, kind, payload, source)
SELECT subject_id, :now, 'migration_cleared',
       json_object('migration', -27, 'op_id', id, 'kind', kind, 'status', status,
                   'steps', json(steps), 'results', json(results)),
       'migration'
FROM operations WHERE kind = 'swap_release' AND status <> 'done'`

const swapIncidentIDs = `SELECT id FROM incidents
WHERE (status = 'verifying' AND json_valid(verify_spec) AND json_extract(verify_spec, '$.swap') IS NOT NULL)
   OR (json_valid(finding) AND json_extract(finding, '$.escalate_action') = 'swap_release')
ORDER BY id`

func migrateDown27On(ctx context.Context, conn *sql.Conn, path string, out io.Writer) error {
	if err := exclusiveGuard(ctx, conn); err != nil {
		return err
	}
	var maxVersion sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&maxVersion); err != nil {
		return fmt.Errorf("%w: read schema_version: %w", ErrDownRefused, err)
	}
	switch {
	case maxVersion.Int64 == migNotifyOutbox:
		_, _ = fmt.Fprintln(out, "already at schema 26")
		return nil
	case maxVersion.Int64 != migSwapRelease:
		return fmt.Errorf("%w: migration %d is applied; 27 must be the newest to roll it back",
			ErrDownRefused, maxVersion.Int64)
	}
	swaps, err := queryOperations(ctx, conn, `kind = ?`, operation.KindSwapRelease)
	if err != nil {
		return err
	}
	incidents, err := queryIDs(ctx, conn, swapIncidentIDs)
	if err != nil {
		return err
	}
	backup, err := backupInto(ctx, conn, path, migSwapRelease)
	if err != nil {
		return err
	}
	err = runConnTx(ctx, conn, func(ctx context.Context, tx *sql.Tx) error {
		if xerr := runStatements(ctx, tx, []stmt{
			{now: true, sql: swapDownJournal},
			{sql: `DELETE FROM operations WHERE kind = 'swap_release'`},
		}, time.Now()); xerr != nil {
			return xerr
		}
		if xerr := rebuildOperations(ctx, tx, operationsTableDDL); xerr != nil {
			return xerr
		}
		_, xerr := tx.ExecContext(ctx, `DELETE FROM schema_version WHERE version = 27`)
		return xerr
	})
	if err != nil {
		return fmt.Errorf("migrate down 27 (backup %s): %w", backup, err)
	}
	printSwapDown(out, swaps, incidents, backup)
	return nil
}

func printSwapDown(out io.Writer, swaps []operation.Op, incidents []string, backup string) {
	journaled := 0
	for _, op := range swaps {
		if op.Status == operation.Done {
			continue
		}
		journaled++
		if op.Status == operation.Running {
			_, _ = fmt.Fprintf(out, "running swap dropped: %s\n", swapLine(op))
		}
	}
	_, _ = fmt.Fprintf(out, "swap operations deleted: %d\nmigration_cleared events: %d\n", len(swaps), journaled)
	_, _ = fmt.Fprintf(
		out,
		"incidents with a swap verify or finding: %d %s\n",
		len(incidents),
		strings.Join(incidents, " "),
	)
	_, _ = fmt.Fprintf(out, "schema_version max: 26\nbackup: %s\n", backup)
}

func swapLine(op operation.Op) string {
	var series, season, episode int
	for _, s := range op.Steps {
		if series == 0 {
			series = s.MediaID
		}
		if episode == 0 && s.Episode > 0 {
			season, episode = s.Season, s.Episode
		}
	}
	return fmt.Sprintf("%s series %d S%02dE%02d", op.Title, series, season, episode)
}

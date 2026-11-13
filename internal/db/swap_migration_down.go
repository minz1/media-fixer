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
FROM operations WHERE kind IN ('swap_release', 'resync_subtitle') AND status <> 'done'`

const swapIncidentIDs = `SELECT id FROM incidents
WHERE (status = 'verifying' AND json_valid(verify_spec)
       AND (json_extract(verify_spec, '$.swap') IS NOT NULL OR json_extract(verify_spec, '$.resync') IS NOT NULL))
   OR (json_valid(finding) AND json_extract(finding, '$.escalate_action') IN ('swap_release', 'resync_subtitle'))
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
	swaps, err := queryOperations(ctx, conn, `kind IN (?, ?)`, operation.KindSwapRelease, operation.KindResyncSubtitle)
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
			{sql: `DELETE FROM operations WHERE kind IN ('swap_release', 'resync_subtitle')`},
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
			_, _ = fmt.Fprintf(out, "running %s dropped: %s\n", dropLabel(op), swapLine(op))
		}
	}
	_, _ = fmt.Fprintf(
		out,
		"swap and resync operations deleted: %d\nmigration_cleared events: %d\n",
		len(swaps),
		journaled,
	)
	_, _ = fmt.Fprintf(
		out,
		"incidents with a swap or resync verify or finding: %d %s\n",
		len(incidents),
		strings.Join(incidents, " "),
	)
	if len(incidents) > 0 {
		_, _ = fmt.Fprintln(out, "dismiss or re-triage these incidents: their swap or resync operation rows are gone")
	}
	_, _ = fmt.Fprintf(out, "schema_version max: 26\nbackup: %s\n", backup)
}

func dropLabel(op operation.Op) string {
	if op.Kind == operation.KindResyncSubtitle {
		return "resync"
	}
	return "swap"
}

func swapLine(op operation.Op) string {
	if op.Kind == operation.KindResyncSubtitle {
		st := op.Steps[0]
		return fmt.Sprintf("%s %s %s", op.Title, st.Name, st.Path)
	}
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

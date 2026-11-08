package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
)

type m24Counts struct {
	old, incidents, sqCards, fk int
	integrity                   string
}

func m24Read(t *testing.T, path string) m24Counts {
	t.Helper()
	raw := rawOpen(t, path)
	defer raw.Close()
	c := m24Counts{
		old:       countInt(t, raw, `SELECT COUNT(*) FROM stuck_queue_downloads`),
		incidents: countInt(t, raw, `SELECT COUNT(*) FROM incidents`),
		sqCards:   countInt(t, raw, `SELECT COUNT(*) FROM discord_cards WHERE subject LIKE 'sq:%'`),
		fk:        countInt(t, raw, `SELECT COUNT(*) FROM pragma_foreign_key_check`),
	}
	if err := raw.QueryRow(`PRAGMA integrity_check`).Scan(&c.integrity); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMigration24_RehearsalOnSnapshot(t *testing.T) {
	t.Parallel()
	src := os.Getenv("MF_SNAPSHOT")
	if src == "" {
		t.Skip("MF_SNAPSHOT not set")
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	copyFile(t, src, path)
	before := m24Read(t, path)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	after := m24Read(t, path)
	raw := rawOpen(t, path)
	defer raw.Close()
	version := countInt(t, raw, `SELECT MAX(version) FROM schema_version`)
	total := countInt(t, raw, `SELECT COUNT(*) FROM stuck_downloads`)
	notified := countInt(t, raw, `SELECT COUNT(*) FROM stuck_downloads WHERE state = 'notified'`)
	waiting := countInt(t, raw, `SELECT COUNT(*) FROM stuck_downloads WHERE state = 'waiting'`)
	emptyFp := countInt(t, raw, `SELECT COUNT(*) FROM stuck_downloads WHERE targets_fp = '' AND offered = ''`)
	withOp := countInt(t, raw, `SELECT COUNT(*) FROM stuck_downloads WHERE acted_op_id IS NOT NULL`)
	reran := countInt(t, raw, `SELECT COUNT(*) FROM incidents WHERE stuck_read_reran = 1`)
	t.Logf("before: stuck_queue_downloads %d | incidents %d | sq: cards %d | integrity %s | fk %d",
		before.old, before.incidents, before.sqCards, before.integrity, before.fk)
	t.Logf("after:  schema %d | stuck_queue_downloads %d | stuck_downloads %d / notified %d / waiting %d | "+
		"empty targets_fp+offered %d | acted_op_id %d | incidents %d / reran %d | sq: cards %d | integrity %s | fk %d",
		version, after.old, total, notified, waiting, emptyFp, withOp,
		after.incidents, reran, after.sqCards, after.integrity, after.fk)
	if version != 24 || after.old != before.old || total != before.old || notified != before.old || waiting != 0 ||
		emptyFp != total || withOp != 0 || after.incidents != before.incidents || reran != 0 ||
		after.sqCards != before.sqCards || after.integrity != "ok" || after.fk != 0 {
		t.Errorf("migration 24 rehearsal deviates: before %+v after %+v", before, after)
	}
}

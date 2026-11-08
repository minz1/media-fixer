package stuckqueue_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func TestMigration24_RehearsalRowThroughGo(t *testing.T) {
	t.Parallel()
	src := os.Getenv("MF_SNAPSHOT")
	if src == "" {
		t.Skip("MF_SNAPSHOT not set")
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err = os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	checked := 0
	for _, app := range []string{"sonarr", "radarr"} {
		rows, lerr := d.ListStuck(ctx, app)
		if lerr != nil {
			t.Fatal(lerr)
		}
		for _, r := range rows {
			got, ok, gerr := d.GetStuck(ctx, app, r.DownloadID)
			if gerr != nil || !ok {
				t.Fatalf("%s %s: ok %v err %v", app, r.DownloadID, ok, gerr)
			}
			snap := stuckqueue.FromRowForTest(got)
			if cerr := stuckqueue.Check(snap); cerr != nil {
				t.Errorf("%s %s: %v", app, r.DownloadID, cerr)
			}
			checked++
		}
	}
	t.Logf("rows read back through GetStuck, fromRow and Check: %d", checked)
	if checked == 0 {
		t.Error("no seeded row to read back")
	}
}

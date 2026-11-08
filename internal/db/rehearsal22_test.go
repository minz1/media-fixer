package db_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
)

func formatTableCounts(c map[string]int) string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(c[k]))
	}
	return strings.Join(parts, " ")
}

func sameExcept(a, b map[string]int, skip ...string) []string {
	var diff []string
	for k, v := range a {
		if slices.Contains(skip, k) {
			continue
		}
		if b[k] != v {
			diff = append(diff, k)
		}
	}
	slices.Sort(diff)
	return diff
}

func rehearsalStep(t *testing.T, path, label string) map[string]int {
	t.Helper()
	raw := rawOpen(t, path)
	defer raw.Close()
	c := tableCounts(t, raw)
	t.Logf("%-10s schema_version max %d | %s", label,
		countInt(t, raw, `SELECT MAX(version) FROM schema_version`), formatTableCounts(c))
	logCounts(t, label, rehearsalSnapshot(t, raw))
	return c
}

func TestRehearsal_Migration22(t *testing.T) {
	t.Parallel()
	src := os.Getenv("MF_REHEARSAL_DB")
	if src == "" {
		t.Skip("MF_REHEARSAL_DB not set")
	}
	path := filepath.Join(t.TempDir(), "rehearsal.db")
	copyFile(t, src, path)
	before := rehearsalStep(t, path, "snapshot")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	d.Close()
	up := rehearsalStep(t, path, "up 22")
	if up["operations"] != 0 {
		t.Errorf("operations after up = %d", up["operations"])
	}
	skip := []string{"operations", "schema_version", "incident_events", "schema_saved_ddl"}
	if diff := sameExcept(before, up, skip...); len(diff) > 0 {
		t.Errorf("up changed row counts of %v", diff)
	}
	strip23(t, path)
	var out bytes.Buffer
	if err = db.MigrateDown(context.Background(), path, 22, &out); err != nil {
		t.Fatalf("down: %v", err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if !strings.HasPrefix(line, "backup:") {
			t.Logf("down: %s", line)
		}
	}
	down := rehearsalStep(t, path, "down 21")
	if _, ok := down["operations"]; ok {
		t.Error("operations table still present after down")
	}
	if diff := sameExcept(up, down, "operations", "schema_version"); len(diff) > 0 {
		t.Errorf("down changed row counts of %v", diff)
	}
	if d, err = db.Open(path); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	d.Close()
	again := rehearsalStep(t, path, "re-up 22")
	if diff := sameExcept(up, again); len(diff) > 0 {
		t.Errorf("re-up differs from up in %v", diff)
	}
}

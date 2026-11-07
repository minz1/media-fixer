package server_test

import (
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/server"
)

func TestActionLog_StatusTones(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)
	cases := map[db.ActionStatus]string{
		db.ActionApplied: "gray",
		db.ActionRefused: "gray",
		db.ActionFailed:  "red",
		"bogus":          "gray",
	}
	for status, tone := range cases {
		if got := server.ActionTone(status); got != tone {
			t.Errorf("actionTone(%s) = %s, want %s", status, got, tone)
		}
		var b strings.Builder
		data := map[string]any{"Actions": []db.ActionLog{{Action: "restart_jellyfin", Status: status}}}
		if err := srv.ExecuteTemplateForTest(&b, "incident_actions", data); err != nil {
			t.Fatal(err)
		}
		if want := `tone-` + tone + `">` + string(status); !strings.Contains(b.String(), want) {
			t.Errorf("%s badge: %s missing from %s", status, want, b.String())
		}
	}
}

package server_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/livecheck"
	"github.com/minz1/mediafixer/internal/server"
)

func TestStatusTone(t *testing.T) {
	t.Parallel()
	for in, want := range map[db.IncidentStatus]string{
		db.StatusOpen: "yellow", db.StatusInvestigating: "blue", db.StatusAgentFixed: "green",
		db.StatusManualTestNeeded: "orange", db.StatusResolved: "gray", db.StatusReopened: "red",
		db.StatusBlocked: "red", db.StatusVerifying: "purple", "unknown": "gray",
		lifecycle.AwaitingDownload: "purple", lifecycle.ExecutingPlan: "blue", lifecycle.None: "gray",
	} {
		if got := server.StatusTone(in); got != want {
			t.Errorf("StatusTone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckTone(t *testing.T) {
	t.Parallel()
	for in, want := range map[livecheck.Status]string{
		livecheck.StatusOK: "green", livecheck.StatusDegraded: "yellow", livecheck.StatusFail: "red",
		livecheck.StatusMissing: "red", livecheck.StatusUnconfigured: "orange", livecheck.StatusSkipped: "gray",
		"other": "gray",
	} {
		if got := server.CheckTone(in); got != want {
			t.Errorf("CheckTone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfidenceTone(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"high": "green", "Medium": "yellow", "low": "red", "": "gray"} {
		if got := server.ConfidenceTone(in); got != want {
			t.Errorf("ConfidenceTone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSweepTone(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"done": "green", "running": "blue", "failed": "red", "refused": "red",
		"paused": "yellow", "interrupted": "yellow", "other": "gray",
	} {
		if got := server.SweepTone(in); got != want {
			t.Errorf("SweepTone(%q) = %q, want %q", in, got, want)
		}
	}
}

var pillRe = regexp.MustCompile(`<span\b[^>]*\btext-xs font-medium\b`)

func TestBadges_OneStyle(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, `{{define "badge"}}`)
	if start < 0 {
		t.Fatal(`no {{define "badge"}}`)
	}
	end := start + strings.Index(src[start:], "{{end}}")
	rest := src[:start] + src[end:]
	if n := strings.Count(rest, "rounded-full"); n != 0 {
		t.Errorf("%d rounded-full outside the badge define", n)
	}
	for _, m := range pillRe.FindAllString(rest, -1) {
		t.Errorf("pill utilities on a span: %s", m)
	}
}

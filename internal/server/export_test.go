package server

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/livecheck"

	"github.com/minz1/mediafixer/internal/stuckqueue"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"

	openai "github.com/sashabaranov/go-openai"
)

// SeerrIssueTypeToWhat exports the internal mapping for use in package server_test.
func SeerrIssueTypeToWhat(s string) string { return seerrIssueTypeToWhat(s) }

// BuildIncidentPageDataForTest exposes buildIncidentPageData for direct
// template-execution-error tests (see dashboard_test.go) — production
// handlers discard ExecuteTemplate's error, so an HTTP-level test alone could
// miss a template bug that only manifests partway through rendering.
func (s *Server) BuildIncidentPageDataForTest(ctx context.Context, id string, full bool) (map[string]any, error) {
	return s.buildIncidentPageData(ctx, id, full)
}

// ExecuteTemplateForTest exposes the parsed template set's ExecuteTemplate
// with its error propagated, instead of the production handlers' `_ =`.
func (s *Server) ExecuteTemplateForTest(w io.Writer, name string, data any) error {
	return s.tmpl.t.ExecuteTemplate(w, name, data)
}

func (s *Server) UseListenerForTest(l net.Listener) { s.listener = l }

func (s *Server) StuckCardDataForTest(
	res stuckqueue.Result,
	set stuckqueue.Settings,
	unreadable string,
) map[string]any {
	return s.stuckCardData(&res, false, set, nil, "", "", unreadable)
}

func (s *Server) TimersCardForTest(ctx context.Context) map[string]any { return s.timersCardFor(ctx) }

func StatusTone(s db.IncidentStatus) string { return string(statusMeta(s).Tone) }

func StatusWord(s db.IncidentStatus) string { return statusMeta(s).Label }

func IncidentLive(s db.IncidentStatus) bool               { return incidentLive(s) }
func CheckTone(s livecheck.Status) string                 { return checkTone(s) }
func ConfidenceTone(c string) string                      { return confidenceTone(c) }
func ActionTone(s db.ActionStatus) string                 { return actionTone(s) }
func SweepMeta(s sweepstate.RunStatus) sweepstate.RunMeta { return sweepMeta(s) }

func RelTime(now, t time.Time) string { return relTime(now, t) }

func ParseRows(vals []string) ([]stuckqueue.MappedFile, error) { return parseRows(vals) }

func SummarizeArgs(raw string) string { return summarizeArgs(raw) }

func FirstLine(s string, limit int) string { return firstLine(s, limit) }

func CondensedTurnsForTest(msgs []openai.ChatCompletionMessage, full bool) ([]transcriptTurn, int) {
	return condenseTurns(buildTranscriptTurns(msgs, false), full)
}

func LiveTurnsForTest(msgs []openai.ChatCompletionMessage) []transcriptTurn {
	return buildTranscriptTurns(msgs, true)
}

func (s *Server) SetSelftestTimeoutForTest(d time.Duration) { s.selftestTimeout = d }

func (s *Server) BeginShutdownForTest() { close(s.shutdown) }

func ParseSonarrForTest(body []byte) (SonarrEventForTest, error) {
	ev, err := parseSonarr(body)
	eps := make([][2]int, 0, len(ev.episodes))
	for _, e := range ev.episodes {
		eps = append(eps, [2]int{e.season, e.episode})
	}
	if len(eps) == 0 {
		eps = nil
	}
	return SonarrEventForTest{Kind: string(ev.kind), Title: ev.title, Episodes: eps, DownloadID: ev.downloadID}, err
}

type SonarrEventForTest struct {
	Kind, Title, DownloadID string
	Episodes                [][2]int
}

func SonarrConsumersForTest(kind string) []string { return sonarrConsumers(sonarrKind(kind)) }

package agent

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/client"
)

const desyncReason = "Subtitles reported out of sync; not fixed by Bazarr. "

var desyncPattern = regexp.MustCompile(
	`(?i)(out[ -]of[ -]sync|not in sync|desync|\b(subs?|subtitles?)\b.{0,40}\b(late|early|ahead|behind|delay(ed)?|lag(s|ging)?|timing)\b)`,
)

func IsDesyncReport(text string) bool { return desyncPattern.MatchString(text) }

func lastAlignment(messages []openai.ChatCompletionMessage) *Alignment {
	calls := map[string]bool{}
	var last *Alignment
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = calls[tc.ID] || tc.Function.Name == toolStreamAlignment
		}
		if m.Role != openai.ChatMessageRoleTool || !calls[m.ToolCallID] {
			continue
		}
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(m.Content), &probe) != nil || probe[keyError] != nil {
			continue
		}
		var al Alignment
		if json.Unmarshal([]byte(m.Content), &al) == nil {
			last = &al
		}
	}
	return last
}

func escalateDesync(result *DiagnosticResult, streams *Alignment) {
	result.SubtitleDesync = true
	result.SubtitleStreams = streams
	result.RequiresApproval = true
	result.EscalateAction = EscalateManualInvestigation
	result.EscalateParams = nil
	result.VerifyAfterSeconds = 0
	result.PrimaryReason = desyncReason + result.PrimaryReason
	result.Outcome = DiagnosisOutcome{Kind: DiagnosisManual, Reason: result.PrimaryReason}
}

func (a *Agent) ladderSucceeded(
	ctx context.Context, incidentID string, result *DiagnosticResult, search *BazarrSearchResult,
	resync *BazarrResyncResult,
) bool {
	want, _, ok := langCodes(result.VerifySubtitleLanguage)
	if !ok || result.SubtitleItemID == "" || a.disp == nil || a.disp.Jellyfin == nil {
		return false
	}
	info, err := a.disp.Jellyfin.ItemInfo(ctx, result.SubtitleItemID)
	if err != nil {
		return false
	}
	movie := info.Type == itemMovie
	if resync != nil && resync.Synced && sameLanguage(resync.Language, want) &&
		(movie && resync.coversMovie(info) || !movie && resync.coversEpisode(info)) {
		return true
	}
	return search != nil && search.Found && sameLanguage(search.Language, want) &&
		(movie && search.coversMovie(info) || !movie && search.coversEpisode(info)) &&
		!a.searchedElsewhereOrUnknown(ctx, incidentID, search)
}

func sameLanguage(lang, iso1 string) bool {
	got, _, ok := langCodes(lang)
	return ok && got == iso1
}

func (d *Dispatcher) searchedElsewhere(ctx context.Context, incidentID string, s *BazarrSearchResult) (bool, error) {
	if d.Attempts == nil {
		return false, errAttemptsUnavailable
	}
	attempts, err := d.Attempts.AttemptsSince(ctx, toolBazarrSearch, time.Now().Add(-resyncGuardWindow))
	if err != nil {
		return false, err
	}
	for _, at := range attempts {
		lang, _ := at.Params[paramLanguage].(string)
		if at.IncidentID != incidentID && sameLanguage(lang, s.Language) && sameSearchTarget(at.Params, s) {
			return true, nil
		}
	}
	return false, nil
}

func sameSearchTarget(params map[string]any, s *BazarrSearchResult) bool {
	if target, _ := params[paramTarget].(string); target != "" {
		return target == s.Target
	}
	title, _ := params[paramTitle].(string)
	mediaType, _ := params[paramMediaType].(string)
	if !strings.EqualFold(title, s.Title) || (mediaType == client.ReplaceMediaMovie) != s.Movie {
		return false
	}
	return s.Movie || intArgOrSentinel(params, paramSeason) == s.Season &&
		intArgOrSentinel(params, paramEpisode) == s.Episode
}

func (a *Agent) searchedElsewhereOrUnknown(ctx context.Context, incidentID string, s *BazarrSearchResult) bool {
	found, err := a.disp.searchedElsewhere(ctx, incidentID, s)
	return found || err != nil
}

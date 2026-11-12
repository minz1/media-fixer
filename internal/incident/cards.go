package incident

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

const (
	verbResolve    = "resolve"
	verbRerun      = "rerun"
	verbKeep       = "keep"
	verbUnlock     = "unlock"
	verbPreview    = "preview"
	verbRefresh    = "refresh"
	verbApprove    = "approve"
	fieldRootCause = "Root cause"
	fieldAction    = "Action needed"
	fieldReason    = "Reason"
)

func ownerCard(inc *db.Incident, tone card.Tone, title, body string, fields ...card.Field) card.Card {
	return card.Card{Tone: tone, Title: title, Body: body, Fields: fields, Path: "/incidents/" + inc.ID}
}

func reporterCard(tone card.Tone, text string) card.Card {
	return card.Card{Tone: tone, Body: text}
}

func escalationVerbs() []string {
	var verbs []string
	for _, sp := range cardSpecs(cardPlan{}, nil) {
		verbs = append(verbs, sp.verb)
	}
	return verbs
}

const (
	fieldCandidates     = "Subtitle candidates"
	fieldDownloaded     = "Subtitles downloaded"
	candidatePick       = "Pick one in Bazarr, then Rerun"
	maxCandidates       = 5
	fieldValueLimit     = 1024
	candidateMultibyteB = 8
)

func subtitleSearchField(r *agent.BazarrSearchResult) (card.Field, bool) {
	if r == nil {
		return card.Field{}, false
	}
	if r.Found {
		return card.Field{Name: fieldDownloaded, Value: filepath.Base(r.Path)}, true
	}
	if len(r.Candidates) == 0 {
		return card.Field{}, false
	}
	cands := slices.Clone(r.Candidates)
	slices.SortStableFunc(cands, func(a, b client.BazarrCandidate) int { return cmp.Compare(b.Score, a.Score) })
	cands = cands[:min(len(cands), maxCandidates)]
	room := (fieldValueLimit-len(candidatePick))/len(cands) - 1 - candidateMultibyteB
	lines := make([]string, 0, len(cands)+1)
	for i, c := range cands {
		line := []rune(fmt.Sprintf("%d. %s · %s · %g", i+1, c.Provider, c.Release, c.Score))
		lines = append(lines, string(line[:min(len(line), room)]))
	}
	lines = append(lines, candidatePick)
	return card.Field{Name: fieldCandidates, Value: strings.Join(lines, "\n")}, true
}

func withSubtitleSearch(inc *db.Incident, result *agent.DiagnosticResult, fields ...card.Field) []card.Field {
	if inc.What != "subtitles" || result == nil {
		return fields
	}
	if f, ok := subtitleSearchField(result.SubtitleSearch); ok {
		return append(fields, f)
	}
	return fields
}

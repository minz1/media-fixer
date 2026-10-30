package incident

import (
	"github.com/minz1/mediafixer/internal/card"
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
)

func ownerCard(inc *db.Incident, tone card.Tone, title, body string, fields ...card.Field) card.Card {
	return card.Card{Tone: tone, Title: title, Body: body, Fields: fields, Path: "/incidents/" + inc.ID}
}

func reporterCard(tone card.Tone, text string) card.Card {
	return card.Card{Tone: tone, Body: text}
}

func escalationVerbs() []string {
	return []string{verbResolve, verbRerun, verbKeep, verbUnlock, verbPreview, verbRefresh, verbApprove}
}

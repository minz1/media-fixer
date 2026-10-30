package card

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const (
	idPrefix     = "mf1"
	idSep        = ":"
	idMinParts   = 3
	fpLen        = 12
	kindIncident = "inc"
	kindStuck    = "sq"
	kindSweep    = "sweep"
)

func Encode(a Action) (string, bool) {
	parts := append([]string{idPrefix, a.Kind, a.Verb}, a.Args...)
	for _, p := range parts {
		if strings.Contains(p, idSep) {
			return "", false
		}
	}
	id := strings.Join(parts, idSep)
	return id, len(id) <= MaxIDLen
}

func Decode(id string) (Action, error) {
	parts := strings.Split(id, idSep)
	if len(parts) < idMinParts || parts[0] != idPrefix {
		return Action{}, fmt.Errorf("%w: %q", ErrUnknownID, id)
	}
	return Action{Kind: parts[1], Verb: parts[2], Args: parts[idMinParts:]}, nil
}

func Fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:fpLen]
}

func IncidentSubject(id string) string {
	return kindIncident + idSep + id
}

func StuckSubject(app, downloadID string) string {
	return kindStuck + idSep + app + idSep + strings.ToLower(downloadID)
}

func SweepSubject(runID int64) string {
	return kindSweep + idSep + strconv.FormatInt(runID, 10)
}

func SubjectOf(a Action) string {
	switch {
	case a.Kind == kindIncident && len(a.Args) >= 1:
		return IncidentSubject(a.Args[0])
	case a.Kind == kindStuck && len(a.Args) >= 2:
		return StuckSubject(a.Args[0], a.Args[1])
	default:
		return ""
	}
}

func Kind(subject string) string {
	kind, _, _ := strings.Cut(subject, idSep)
	return kind
}

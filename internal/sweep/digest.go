package sweep

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
)

const (
	digestKey   = "maintenance.pack_sweep_digest"
	digestLines = 10
)

type digestState struct {
	SentAt time.Time `json:"sent_at"`
	Stuck  []string  `json:"stuck"`
}

func Stuck(rows map[string]db.SweepTarget, after int) []db.SweepTarget {
	var out []db.SweepTarget
	for _, t := range rows {
		if t.Streak >= after {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b db.SweepTarget) int {
		return cmp.Or(
			cmp.Compare(b.Streak, a.Streak),
			cmp.Compare(a.SeriesTitle, b.SeriesTitle),
			cmp.Compare(a.Season, b.Season),
			cmp.Compare(a.Target, b.Target),
		)
	})
	return out
}

func digestCard(stuck []db.SweepTarget) card.Card {
	c := card.Card{Path: maintenancePath, Title: "Stuck seasons"}
	if len(stuck) == 0 {
		c.Tone, c.Body = card.Info, "No seasons are stuck now."
		return c
	}
	c.Tone = card.Warn
	lines := make([]string, 0, digestLines+1)
	for _, t := range stuck[:min(len(stuck), digestLines)] {
		lines = append(lines, fmt.Sprintf("%s S%02d: %d missing, %d attempts, last %s, next %s",
			t.SeriesTitle, t.Season, t.Missing, t.Attempts,
			t.LastAttempt.UTC().Format(time.DateOnly), t.NextEligible.UTC().Format(time.DateOnly)))
	}
	if len(stuck) > digestLines {
		lines = append(lines, fmt.Sprintf("…and %d more", len(stuck)-digestLines))
	}
	c.Body = strings.Join(lines, "\n")
	return c
}

func (s *Scheduler) digest(set Settings, now time.Time) error {
	now = now.UTC()
	if now.Hour() < set.DigestHourUTC || s.r.Notify == nil {
		return nil
	}
	var prev digestState
	raw, err := s.d.GetSetting(s.base, digestKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		if err = json.Unmarshal([]byte(raw), &prev); err != nil {
			return fmt.Errorf("stored digest state is unreadable: %w", err)
		}
	}
	if prev.SentAt.UTC().Truncate(day).Equal(now.Truncate(day)) {
		return nil
	}
	rows, err := s.d.ListSweepTargets(s.base)
	if err != nil {
		return err
	}
	stuck := Stuck(rows, set.StuckAfter)
	keys := make([]string, len(stuck))
	for i, t := range stuck {
		keys[i] = t.Target
	}
	slices.Sort(keys)
	next := digestState{SentAt: now, Stuck: keys}
	var sendErr error
	if !slices.Equal(keys, prev.Stuck) {
		sendErr = s.r.Notify(s.base, digestCard(stuck))
	}
	if sendErr != nil && !errors.Is(sendErr, card.ErrUntracked) {
		next.Stuck = prev.Stuck
	} else {
		sendErr = nil
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return errors.Join(sendErr, s.d.SetSetting(s.base, digestKey, string(b)))
}

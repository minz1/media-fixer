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
	digestKey    = "maintenance.pack_sweep_digest"
	digestLines  = 10
	digestRawLog = 200
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
	c := card.Card{Path: "/monitoring", Title: "Stuck seasons"}
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
	prev, err := s.loadDigest(now)
	if err != nil || prev.SentAt.UTC().Truncate(day).Equal(now.Truncate(day)) {
		return err
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
	if err = s.saveDigest(digestState{SentAt: now, Stuck: prev.Stuck}); err != nil || slices.Equal(keys, prev.Stuck) {
		return err
	}
	if err = s.r.Notify(s.base, digestCard(stuck)); err != nil && !errors.Is(err, card.ErrUntracked) {
		return err
	}
	return s.saveDigest(digestState{SentAt: now, Stuck: keys})
}

func (s *Scheduler) loadDigest(now time.Time) (digestState, error) {
	var st digestState
	raw, err := s.d.GetSetting(s.base, digestKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return st, nil
	case err != nil:
		return st, err
	}
	uerr := json.Unmarshal([]byte(raw), &st)
	if uerr == nil {
		return st, nil
	}
	s.wmu.Lock()
	logged := s.badDigest == raw
	s.badDigest = raw
	s.wmu.Unlock()
	if !logged {
		s.r.logger().WarnContext(s.base, "pack sweep scheduler",
			"err", fmt.Errorf("stored digest state is unreadable, resetting: %w", uerr),
			"raw", raw[:min(len(raw), digestRawLog)])
	}
	reset := digestState{SentAt: now}
	return reset, s.saveDigest(reset)
}

func (s *Scheduler) saveDigest(st digestState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.d.SetSetting(s.base, digestKey, string(b))
}

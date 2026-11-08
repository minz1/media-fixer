package babysit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/stuckqueue"
	"github.com/minz1/mediafixer/internal/sweep"
)

type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

type Service struct {
	DB     *db.DB
	Sonarr *client.ArrClient
	State  func() (stuckqueue.Result, bool, bool)
	LLM    Completer
	Model  string
	Now    func() time.Time
	Log    *slog.Logger

	mu        sync.Mutex
	running   map[int]bool
	proposing map[string]bool
	finding   map[int]bool
}

var (
	ErrNotStuck       = errors.New("this show is not stuck")
	ErrExplainRunning = errors.New("an explanation for this show is already running")
	ErrProposeRunning = errors.New("a proposal for this download is already running")
)

func (s *Service) logger() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func claim[K comparable](mu *sync.Mutex, m *map[K]bool, k K) (func(), bool) {
	mu.Lock()
	defer mu.Unlock()
	if (*m)[k] {
		return nil, false
	}
	if *m == nil {
		*m = map[K]bool{}
	}
	(*m)[k] = true
	return func() {
		mu.Lock()
		defer mu.Unlock()
		delete(*m, k)
	}, true
}

type Row struct {
	SeriesID     int
	Title        string
	Seasons      int
	Missing      int
	StuckSeasons int
	QueueItems   int
	Latest       *db.SweepItem
}

type List struct {
	Rows               []Row
	Excluded           []client.Series
	Pickable           []client.Series
	SonarrErr          string
	SettingsUnreadable bool
}

type SeasonCounts struct {
	Season, Aired, OnDisk, Missing, Queued int
}

type Show struct {
	Series             *client.Series
	SonarrErr          string
	Seasons            []SeasonCounts
	Targets            []db.SweepTarget
	Items              []db.SweepItem
	Queue              []stuckqueue.Item
	Live               []LiveStuck
	Stuck              bool
	Excluded           bool
	Cached             *db.BabysitExplanation
	CacheFresh         bool
	SettingsUnreadable bool
}

type LiveStuck struct {
	Download stuckqueue.Download
	Last     *stuckqueue.Item
}

type Explanation struct {
	Text      string
	CreatedAt time.Time
	FromCache bool
}

func sonarrItems(res stuckqueue.Result) []stuckqueue.Item {
	var out []stuckqueue.Item
	for _, a := range res.Apps {
		if a.App == stuckqueue.AppSonarr {
			out = append(out, a.Items...)
		}
	}
	return out
}

func itemSeries(it stuckqueue.Item) map[int]bool {
	ids := map[int]bool{}
	for _, r := range it.Records {
		if r.SeriesID > 0 {
			ids[r.SeriesID] = true
		}
	}
	return ids
}

func (s *Service) queueItems() []stuckqueue.Item {
	if s.State == nil {
		return nil
	}
	res, _, _ := s.State()
	return sonarrItems(res)
}

type local struct {
	targets    []db.SweepTarget
	queue      []stuckqueue.Item
	stuck      bool
	excluded   bool
	unreadable bool
}

func (s *Service) local(ctx context.Context, seriesID int) (local, error) {
	all, err := s.DB.ListSweepTargets(ctx)
	if err != nil {
		return local{}, err
	}
	set, setErr := sweep.LoadSettings(ctx, s.DB)
	l := local{unreadable: setErr != nil}
	for _, t := range all {
		if t.SeriesID == seriesID {
			l.targets = append(l.targets, t)
			l.stuck = l.stuck || t.Stuck(set.StuckAfter)
		}
	}
	slices.SortFunc(l.targets, func(a, b db.SweepTarget) int { return cmp.Compare(a.Season, b.Season) })
	for _, it := range s.queueItems() {
		if itemSeries(it)[seriesID] {
			l.queue = append(l.queue, it)
		}
	}
	l.stuck = l.stuck || len(l.queue) > 0
	l.excluded = slices.Contains(set.ExcludedSeries, seriesID)
	return l, nil
}

func (s *Service) readSonarr(
	ctx context.Context, seriesID int,
) (client.Series, []SeasonCounts, []client.QueueEntry, error) {
	series, err := s.Sonarr.GetSeries(ctx, seriesID)
	if err != nil {
		return client.Series{}, nil, nil, err
	}
	eps, err := s.Sonarr.GetEpisodes(ctx, seriesID, -1)
	if err != nil {
		return client.Series{}, nil, nil, err
	}
	entries, err := s.Sonarr.QueueEntries(ctx)
	if err != nil {
		return client.Series{}, nil, nil, err
	}
	queued := map[int]bool{}
	for _, d := range entries {
		if d.SeriesID == seriesID {
			queued[d.EpisodeID] = true
		}
	}
	now := s.Now()
	missing := map[int]bool{}
	for _, e := range sweep.Missing(eps, queued, now) {
		missing[e.ID] = true
	}
	bySeason := map[int]*SeasonCounts{}
	for _, e := range eps {
		if e.SeasonNumber <= 0 || !e.Monitored || e.AirDateUtc == nil || !e.AirDateUtc.Before(now) {
			continue
		}
		c := bySeason[e.SeasonNumber]
		if c == nil {
			c = &SeasonCounts{Season: e.SeasonNumber}
			bySeason[e.SeasonNumber] = c
		}
		c.Aired++
		switch {
		case e.HasFile:
			c.OnDisk++
		case missing[e.ID]:
			c.Missing++
		case queued[e.ID]:
			c.Queued++
		}
	}
	out := make([]SeasonCounts, 0, len(bySeason))
	for _, c := range bySeason {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b SeasonCounts) int { return cmp.Compare(a.Season, b.Season) })
	return series, out, entries, nil
}

func (s *Service) facts(
	ctx context.Context,
	series client.Series,
	seasons []SeasonCounts,
	targets []db.SweepTarget,
	items []db.SweepItem,
) (string, error) {
	hist, err := s.Sonarr.SeriesHistory(ctx, series.ID)
	if err != nil {
		return "", err
	}
	return buildFacts(bundleInput{Series: series, Seasons: seasons, Targets: targets, Items: items, History: hist}), nil
}

func (s *Service) Show(ctx context.Context, seriesID int) (Show, error) {
	l, err := s.local(ctx, seriesID)
	if err != nil {
		return Show{}, err
	}
	items, err := s.DB.SeriesSweepItems(ctx, seriesID, maxItems)
	if err != nil {
		return Show{}, err
	}
	sh := Show{
		Targets:            l.targets,
		Items:              items,
		Queue:              l.queue,
		Stuck:              l.stuck,
		Excluded:           l.excluded,
		SettingsUnreadable: l.unreadable,
	}
	if c, getErr := s.DB.GetBabysitExplanation(ctx, seriesID); getErr == nil {
		sh.Cached = &c
	} else if !errors.Is(getErr, db.ErrNotFound) {
		return Show{}, getErr
	}
	switch err = s.fillSonarr(ctx, &sh, seriesID); {
	case errors.Is(err, client.ErrNotFound):
		return Show{}, err
	case err != nil:
		sh.SonarrErr = err.Error()
	}
	return sh, nil
}

func (s *Service) fillSonarr(ctx context.Context, sh *Show, seriesID int) error {
	series, seasons, entries, err := s.readSonarr(ctx, seriesID)
	if err != nil {
		return err
	}
	sh.Series, sh.Seasons = &series, seasons
	sh.Live = liveStuck(entries, seriesID, sh.Queue)
	if !sh.Stuck && sh.Cached == nil {
		return nil
	}
	facts, err := s.facts(ctx, series, seasons, sh.Targets, sh.Items)
	if err != nil {
		return err
	}
	sh.CacheFresh = sh.Cached != nil && sh.Cached.Fingerprint == fingerprint(s.Model, facts)
	return nil
}

func liveStuck(entries []client.QueueEntry, seriesID int, last []stuckqueue.Item) []LiveStuck {
	var out []LiveStuck
	for _, d := range stuckqueue.Group(stuckqueue.AppSonarr, entries) {
		if !d.Stuck() || !d.OwnedBy(seriesID) {
			continue
		}
		l := LiveStuck{Download: d}
		for i := range last {
			if strings.EqualFold(last[i].DownloadID, d.DownloadID) {
				l.Last = &last[i]
				break
			}
		}
		out = append(out, l)
	}
	return out
}

func (s *Service) List(ctx context.Context) (List, error) {
	targets, err := s.DB.ListSweepTargets(ctx)
	if err != nil {
		return List{}, err
	}
	latest, err := s.DB.LatestSweepItems(ctx)
	if err != nil {
		return List{}, err
	}
	set, setErr := sweep.LoadSettings(ctx, s.DB)
	queue := s.queueItems()
	rows := collectRows(targets, latest, queue, set.StuckAfter)

	l := List{SettingsUnreadable: setErr != nil}
	live, err := s.Sonarr.ListSeries(ctx)
	if err != nil {
		l.SonarrErr = err.Error()
	}
	byID := map[int]client.Series{}
	for _, sr := range live {
		byID[sr.ID] = sr
	}
	title := func(id int, fallback string) string {
		return cmp.Or(byID[id].Title, fallback, fmt.Sprintf("Series %d", id))
	}

	for _, id := range set.ExcludedSeries {
		sr := byID[id]
		sr.ID, sr.Title = id, title(id, "")
		if r := rows[id]; r != nil {
			sr.Title = title(id, r.Title)
			delete(rows, id)
		}
		l.Excluded = append(l.Excluded, sr)
	}
	for id, r := range rows {
		r.Title = title(id, r.Title)
		l.Rows = append(l.Rows, *r)
	}
	for _, sr := range live {
		if !l.SettingsUnreadable && sr.Monitored && !slices.Contains(set.ExcludedSeries, sr.ID) {
			l.Pickable = append(l.Pickable, sr)
		}
	}
	sortList(&l)
	return l, nil
}

func collectRows(
	targets map[string]db.SweepTarget, latest map[int]db.SweepItem, queue []stuckqueue.Item, stuckAfter int,
) map[int]*Row {
	rows := map[int]*Row{}
	row := func(id int, title string) *Row {
		r := rows[id]
		if r == nil {
			r = &Row{SeriesID: id}
			rows[id] = r
		}
		if r.Title == "" {
			r.Title = title
		}
		return r
	}
	for _, t := range targets {
		r := row(t.SeriesID, t.SeriesTitle)
		r.Seasons++
		r.Missing += t.Missing
		if t.Stuck(stuckAfter) {
			r.StuckSeasons++
		}
	}
	for id, it := range latest {
		row(id, it.SeriesTitle).Latest = &it
	}
	for _, it := range queue {
		for id := range itemSeries(it) {
			row(id, "").QueueItems++
		}
	}
	return rows
}

func sortList(l *List) {
	byTitle := func(a, b client.Series) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)), cmp.Compare(a.ID, b.ID))
	}
	slices.SortFunc(l.Excluded, byTitle)
	slices.SortFunc(l.Pickable, byTitle)
	slices.SortFunc(l.Rows, func(a, b Row) int {
		return cmp.Or(
			cmp.Compare(boolInt(a.StuckSeasons == 0), boolInt(b.StuckSeasons == 0)),
			cmp.Compare(boolInt(a.QueueItems == 0), boolInt(b.QueueItems == 0)),
			cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)),
			cmp.Compare(a.SeriesID, b.SeriesID),
		)
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

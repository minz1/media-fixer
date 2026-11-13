package stuckqueue_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const swapRoot = "/data/downloads/sonarr"

type swapCase struct {
	series, episode, season, fileID int
	brokenPath                      string
	cand                            client.ImportCandidate
	eps                             []client.Episode
	queue                           []client.QueueEntry
}

func baseSwap() swapCase {
	return swapCase{
		series: 7, episode: 6, season: 1, fileID: 40, brokenPath: "/tv/Show/S01E06.mkv",
		cand: client.ImportCandidate{
			Path: swapRoot + "/Show.S01E06.1080p/Show.S01E06.mkv", SeriesID: 7, SeasonNumber: 1,
			Episodes: []client.CandidateEpisode{{ID: 6, SeasonNumber: 1, EpisodeNumber: 6, HasFile: true}},
		},
		eps: []client.Episode{
			{SeriesID: 7, ID: 5, SeasonNumber: 1, EpisodeNumber: 5, HasFile: true, EpisodeFileID: 39},
			{SeriesID: 7, ID: 6, SeasonNumber: 1, EpisodeNumber: 6, HasFile: true, EpisodeFileID: 40},
			{SeriesID: 7, ID: 90, SeasonNumber: 0, EpisodeNumber: 1, HasFile: true, EpisodeFileID: 41},
			{SeriesID: 8, ID: 6, SeasonNumber: 1, EpisodeNumber: 6, HasFile: true, EpisodeFileID: 40},
		},
		queue: []client.QueueEntry{{DownloadID: "X", EpisodeID: 5, SeriesID: 7}},
	}
}

func (c swapCase) run() error {
	return stuckqueue.SwapGuard(c.series, c.episode, c.season, c.fileID, c.brokenPath, swapRoot, c.cand, c.eps, c.queue)
}

func TestD2_SwapGuard(t *testing.T) {
	t.Parallel()
	special := baseSwap()
	special.episode, special.season, special.fileID = 90, 0, 41
	special.cand.SeasonNumber = 0
	special.cand.Episodes = []client.CandidateEpisode{{ID: 90, SeasonNumber: 0, EpisodeNumber: 1, HasFile: true}}
	specialAsSeason := special
	specialAsSeason.season = 1
	cases := []struct {
		name   string
		mutate func(*swapCase)
		want   string
		base   *swapCase
	}{
		{name: "one episode ok"},
		{name: "special ok", base: &special},
		{name: "special under a season incident", base: &specialAsSeason, want: "season"},
		{name: "two episodes", want: "exactly one episode", mutate: func(c *swapCase) {
			c.cand.Episodes = append(c.cand.Episodes, client.CandidateEpisode{ID: 5, SeasonNumber: 1, EpisodeNumber: 5})
		}},
		{name: "other episode", want: "not the incident's episode", mutate: func(c *swapCase) {
			c.cand.Episodes = []client.CandidateEpisode{{ID: 5, SeasonNumber: 1, EpisodeNumber: 5}}
		}},
		{name: "other series", want: "another series", mutate: func(c *swapCase) { c.cand.SeriesID = 8 }},
		{name: "season mismatch", want: "season", mutate: func(c *swapCase) { c.cand.SeasonNumber = 2 }},
		{
			name:   "current file differs",
			want:   "no longer the episode's file",
			mutate: func(c *swapCase) { c.fileID = 99 },
		},
		{name: "episode unknown", want: "not in this show", mutate: func(c *swapCase) {
			c.episode = 77
			c.cand.Episodes[0].ID = 77
		}},
		{name: "queued download", want: "another download is queued", mutate: func(c *swapCase) {
			c.queue = append(c.queue, client.QueueEntry{DownloadID: "Y", EpisodeID: 6, SeriesID: 7})
		}},
		{name: "broken file covers two", want: "more than one episode", mutate: func(c *swapCase) {
			c.eps[0].EpisodeFileID = 40
		}},
		{name: "same path as the broken file", want: "is the broken file", mutate: func(c *swapCase) {
			c.brokenPath = c.cand.Path
		}},
		{name: "outside the root", want: "not under", mutate: func(c *swapCase) {
			c.cand.Path = swapRoot + "/../../tv/Show/S01E06.mkv"
		}},
		{name: "sibling of the root", want: "not under", mutate: func(c *swapCase) {
			c.cand.Path = swapRoot + "-x/a.mkv"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := baseSwap()
			if tc.base != nil {
				c = *tc.base
			}
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			err := c.run()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, stuckqueue.ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

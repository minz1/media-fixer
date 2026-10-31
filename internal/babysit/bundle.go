package babysit

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const (
	maxHistory     = 200
	maxItems       = 50
	maxBundleBytes = 64 << 10
	omittedReserve = 64
)

func clean(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "<", "‹"), ">", "›")
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

type bundleInput struct {
	Series  client.Series
	Seasons []SeasonCounts
	Targets []db.SweepTarget
	Items   []db.SweepItem
	History []client.SeriesEvent
}

func intPtr(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func buildFacts(in bundleInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "series: %s | type=%s | monitored=%t | added=%s\n",
		clean(in.Series.Title), clean(in.Series.SeriesType), in.Series.Monitored, stamp(in.Series.Added))

	b.WriteString("seasons (monitored aired episodes):\n")
	for _, c := range slices.SortedFunc(slices.Values(in.Seasons), func(a, b SeasonCounts) int { return cmp.Compare(a.Season, b.Season) }) {
		fmt.Fprintf(
			&b,
			"season %d | aired=%d | on_disk=%d | missing=%d | queued=%d\n",
			c.Season,
			c.Aired,
			c.OnDisk,
			c.Missing,
			c.Queued,
		)
	}

	b.WriteString("sweep targets:\n")
	for _, t := range slices.SortedFunc(slices.Values(in.Targets), func(a, b db.SweepTarget) int {
		return cmp.Or(cmp.Compare(a.Season, b.Season), cmp.Compare(a.Target, b.Target))
	}) {
		fmt.Fprintf(
			&b,
			"season %d | streak=%d | missing_at_last_attempt=%d | attempts=%d | last_attempt=%s | next_eligible=%s\n",
			t.Season,
			t.Streak,
			t.Missing,
			t.Attempts,
			stamp(t.LastAttempt),
			stamp(t.NextEligible),
		)
	}

	b.WriteString("sweep items (newest first):\n")
	items := slices.SortedFunc(slices.Values(in.Items), func(a, b db.SweepItem) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(b.ID, a.ID))
	})
	for _, it := range items[:min(len(items), maxItems)] {
		fmt.Fprintf(
			&b,
			"%s | season=%s | %s | episodes=%s | release=%s | indexer=%s | lookup_results=%s\n",
			stamp(
				it.At,
			),
			intPtr(it.Season),
			clean(it.Result),
			clean(it.Episodes),
			clean(it.ReleaseTitle),
			clean(it.Indexer),
			intPtr(it.LookupResults),
		)
	}

	history := slices.SortedFunc(slices.Values(in.History), func(a, b client.SeriesEvent) int {
		return cmp.Or(b.Date.Compare(a.Date), cmp.Compare(b.ID, a.ID))
	})
	b.WriteString("history (newest first):\n")
	budget := maxBundleBytes - b.Len() - omittedReserve
	kept := 0
	for _, h := range history[:min(len(history), maxHistory)] {
		line := historyLine(h)
		if len(line) > budget {
			break
		}
		budget -= len(line)
		b.WriteString(line)
		kept++
	}
	if left := len(history) - kept; left > 0 {
		fmt.Fprintf(&b, "%d older history records left out\n", left)
	}
	return b.String()
}

func historyLine(h client.SeriesEvent) string {
	season, episode := "", ""
	if h.Episode != nil {
		season, episode = strconv.Itoa(h.Episode.SeasonNumber), strconv.Itoa(h.Episode.EpisodeNumber)
	}
	return fmt.Sprintf("%s | %s | season=%s episode=%s | %s | indexer=%s | message=%s\n",
		stamp(h.Date), clean(h.EventType), season, episode, clean(h.SourceTitle), clean(h.Indexer), clean(h.Message))
}

func buildQueue(items []stuckqueue.Item) string {
	if len(items) == 0 {
		return "none\n"
	}
	sorted := slices.SortedFunc(slices.Values(items), func(a, b stuckqueue.Item) int {
		return cmp.Or(cmp.Compare(a.DownloadID, b.DownloadID), cmp.Compare(a.Title, b.Title))
	})
	var b strings.Builder
	for _, it := range sorted {
		var msgs []string
		for _, r := range it.Records {
			msgs = append(msgs, r.Messages...)
		}
		slices.Sort(msgs)
		msgs = slices.Compact(msgs)
		for i := range msgs {
			msgs[i] = clean(msgs[i])
		}
		fmt.Fprintf(&b, "%s | verdict=%s | %s\n", clean(it.Title), clean(string(it.Verdict)), strings.Join(msgs, " ; "))
	}
	return b.String()
}

func userMessage(facts, queue string) string {
	return "<facts>\n" + facts + "</facts>\n<queue>\n" + queue + "</queue>\n"
}

func fingerprint(model, facts string) string {
	sum := sha256.Sum256([]byte(systemPrompt + "\x00" + model + "\x00" + facts))
	return hex.EncodeToString(sum[:])
}

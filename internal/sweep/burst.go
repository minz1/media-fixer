package sweep

import (
	"strconv"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

const burstWindow = 7 * day

func Bursts(
	recs []client.HistoryRecord, targets map[string]db.SweepTarget, now time.Time, threshold int,
) map[string]bool {
	floor := now.Add(-burstWindow)
	downloads := map[string]map[string]bool{}
	for _, r := range recs {
		if r.EventType != client.FailedEventType || r.Episode == nil {
			continue
		}
		key := seasonTargetKey(r.SeriesID, r.Episode.SeasonNumber)
		after := floor
		if last := targets[key].LastAttempt; last.After(after) {
			after = last
		}
		if !r.Date.After(after) {
			continue
		}
		id := r.DownloadID
		if id == "" {
			id = "id:" + strconv.Itoa(r.ID)
		}
		if downloads[key] == nil {
			downloads[key] = map[string]bool{}
		}
		downloads[key][id] = true
	}
	out := map[string]bool{}
	for key, ids := range downloads {
		if len(ids) >= threshold {
			out[key] = true
		}
	}
	return out
}

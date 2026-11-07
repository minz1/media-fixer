package lifecycle

import (
	"errors"
	"fmt"
	"time"
)

var errNoDownload = errors.New("lifecycle: awaiting download without a pending outcome")

func (d *Download) Label() string {
	switch {
	case d.Season >= 0 && d.Episode >= 0:
		return fmt.Sprintf("%s S%02dE%02d", d.Title, d.Season, d.Episode)
	case d.Season >= 0:
		return fmt.Sprintf("%s S%02d", d.Title, d.Season)
	default:
		return d.Title
	}
}

func downloadNotice(d *Download, p DownloadProblem, errText string) Notice {
	return Notice{Kind: NoticeDownload, Problem: p, Target: d.Label(), Err: errText}
}

func stepDownloadObserved(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	if cur.Download == nil {
		return Decision{Next: cur}, errNoDownload
	}
	next := cur
	switch {
	case ev.Err != "" || ev.Obs == nil:
		return downloadFailed(cur, next, ev.Err, now)
	case ev.Obs.HasFile:
		return toFixed(next)
	case ev.Obs.InQueue:
		return downloadQueued(cur, next, *ev.Obs, now)
	}
	return downloadMissing(cur, next, now)
}

func recheck(s *Snapshot, at time.Time, effs ...Effect) (Decision, error) {
	s.NextCheck = at
	return moved(*s, append(effs, check(s.Generation, at, CheckDownload))...)
}

func downloadFailed(cur, next Snapshot, errText string, now time.Time) (Decision, error) {
	d, l := next.Download, next.In.Limits
	limit := l.TrackCap
	if d.KeepSearching && !d.GrabNotified {
		limit += l.KeepSearchingCheck
	}
	if now.Sub(d.StartedAt) > limit {
		return toOwner(cur, next, ReasonDownloadProblem, downloadNotice(d, ProblemLostTrack, errText))
	}
	return recheck(&next, now.Add(l.CheckEvery))
}

func downloadQueued(cur, next Snapshot, obs Observation, now time.Time) (Decision, error) {
	d, l := next.Download, next.In.Limits
	var effs []Effect
	if !d.GrabNotified {
		d.GrabNotified = true
		if d.KeepSearching {
			d.StartedAt, d.LastProgressAt = now, now
		}
		effs = append(effs, note(NoteFound))
	}
	if obs.ProgressPct > d.LastProgressPct {
		d.LastProgressAt = now
	}
	d.Stage, d.QueueStatus, d.LastProgressPct = StageDownloading, obs.QueueStatus, obs.ProgressPct
	switch {
	case now.Sub(d.LastProgressAt) > l.Stall:
		return toOwner(cur, next, ReasonDownloadProblem, downloadNotice(d, ProblemStalled, ""))
	case now.Sub(d.StartedAt) > l.TrackCap:
		return toOwner(cur, next, ReasonDownloadProblem, downloadNotice(d, ProblemOverlong, ""))
	}
	return recheck(&next, now.Add(l.CheckEvery), effs...)
}

func downloadMissing(cur, next Snapshot, now time.Time) (Decision, error) {
	d, l := next.Download, next.In.Limits
	if d.GrabNotified {
		if now.Sub(d.LastProgressAt) > l.Stall {
			return toOwner(cur, next, ReasonDownloadProblem, downloadNotice(d, ProblemNeverImported, ""))
		}
		d.Stage = StageImporting
		return recheck(&next, now.Add(l.CheckEvery))
	}
	d.Stage, d.QueueStatus = StageSearching, ""
	if d.KeepSearching {
		if now.After(d.KeepSearchingUntil) {
			return toOwner(cur, next, ReasonNoRelease, downloadNotice(d, ProblemSearchEnded, ""))
		}
		resetTrack(d, now)
		return recheck(&next, now.Add(l.KeepSearchingCheck))
	}
	if now.Sub(d.StartedAt) > l.NoReleaseGrace {
		return toOwner(cur, next, ReasonNoRelease, downloadNotice(d, ProblemNoRelease, ""))
	}
	return recheck(&next, now.Add(l.CheckEvery))
}

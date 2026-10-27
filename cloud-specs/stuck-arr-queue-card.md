# Stuck Sonarr/Radarr queue items: a Maintenance card

Spec only. Written 2026-10-27 against `main` at `b3e6afb` plus the `cloud/backlog-2026-10-26` branch.
Source notes: the 2026-10-13 media-0 investigation (Sonarr 4.0.19.2979, decypharr `minz@5d09c8d7`).

## Problem

A finished download can sit in the arr queue forever as `trackedDownloadState=importBlocked`,
`trackedDownloadStatus=warning`. Nobody files a Seerr issue (the episodes usually exist from another
release), so media-fixer never sees it. It shows up only as queue clutter, as decypharr's hourly
`manual import keeps failing` WARN, or as an upgrade that never lands. The JoJo case left 11 such
records. Three kinds, from the source notes:

| Kind | Signal | Can it import? |
|---|---|---|
| Missing output folder | `outputPath` does not exist; `manualimport?downloadId=` returns `[]` | No |
| Unknown series | only visible with `includeUnknownSeriesItems=true`; `seriesId` null; candidates rejected "Unknown Series" | Only by hand-mapping |
| All candidates rejected | candidates exist, every one has rejections | No |

## Scope

A card on `/maintenance`, next to the pack sweep card, that lists stuck items and lets the owner remove
each one with one click. The owner's click is the approval. Out of scope for v1: agent tools
(`arr_queue`, `arr_queue_remove`, `arr_manual_import`), opening incidents, the "unknown series" manual
import (rare now that arr grabs are no longer split; listed as a follow-up).

## Design

### Client (`internal/client`)

- `arr_sweep.go`: new `StuckQueueItem` type and `ArrClient.StuckQueue(ctx) ([]StuckQueueItem, error)`:
  `GET /api/v3/queue` with `pageSize=250`, `includeUnknownSeriesItems=true`,
  `includeUnknownMovieItems=true`, through `do()`. Keep only records with
  `trackedDownloadState == "importBlocked"` or `trackedDownloadStatus == "warning"`. Fields: `id`,
  `downloadId`, `title`, `status`, `trackedDownloadStatus`, `trackedDownloadState`,
  `statusMessages[].{title,messages}`, `outputPath`, `seriesId`, `episodeId`, `movieId`,
  `downloadClient`, `added`.
  - Separate from `QueueDetails` (used by the pack sweep runner) and `GetQueue` (used by the
    pending-outcome sweeper) so neither changes behaviour.
- `ArrClient.ManualImportCandidates(ctx, downloadID)`: `GET /api/v3/manualimport?downloadId=…&filterExistingFiles=false`.
  Returns per file: `path`, `seriesId`/`movieId`, `seasonNumber`, `episodes[].id`, `quality.quality.name`,
  `rejections[].reason`. Never sends `seriesId` (it makes Sonarr return library files instead).
- Removal reuses the existing `ArrClient.RemoveQueueItem(ctx, id, removeFromClient, blocklist, skipRedownload)`.

### Classification (new package `internal/stuckqueue`)

- `Scan(ctx, deps, now, minAge) ([]Item, error)` with `deps` holding Sonarr, Radarr (either may be
  nil) and the media-agent client.
- For each stuck record older than `minAge` (default 6 h, from `added`):
  1. `outputPath` via `MediaAgentClient.ListDirectory`: ENOENT → `KindMissingFolder`.
  2. `seriesId`/`movieId` zero → `KindUnknownSeries`.
  3. Otherwise `ManualImportCandidates`: none, or every candidate has rejections → `KindAllRejected`.
  4. Anything else → `KindOther` (listed, no suggested action).
- `Item.EpisodesHaveFiles`: from `GetEpisodes(seriesId, season)` (`hasFile`) for the record's
  episodes, or `GetMovieFiles` for a movie. Decides the suggested action, as in the source notes'
  decision guide:
  - files exist → suggest **Remove** (`removeFromClient=true, blocklist=false, skipRedownload=true`);
  - missing folder and files missing → suggest **Remove and grab again** (`blocklist=false,
    skipRedownload=false`);
  - all rejected for a release-level reason (sample, wrong content) → offer **Remove and blocklist**;
    for "not an upgrade" offer plain Remove.
- A failed media-agent or manualimport call does not drop the item: it is listed as `KindOther` with
  the error, so the owner still sees it.

### Server (`internal/server/maintenance.go`, `templates/dashboard.html`)

- `GET /maintenance/stuck-queue` → `stuckQueueFragment`. The card loads lazily (`hx-get`,
  `hx-trigger="load"`) because a scan makes several arr and media-agent calls. It renders a
  `stuck_queue_card` define: one row per item with app, title, kind, age, the status message titles,
  `outputPath`, and the offered buttons.
- `POST /maintenance/stuck-queue/{app}/{id}/remove` with form `mode=keep|regrab|blocklist` →
  `stuckQueueRemove`. Inside the existing `requireSameOrigin` group.
  - Re-reads the queue first and refuses unless the record still exists with the same `downloadId`
    and still classifies as stuck. Same TOCTOU rule as the escalation preview: the button removes
    what the owner saw, not whatever now has that id.
  - Maps `mode` to the three flags; nothing else is accepted.
  - Runs on `context.WithoutCancel(r.Context())` (same reason as S1: a closed tab must not drop it).
  - Logs at INFO with app, id, downloadId, title, mode; re-renders the card.
- `SetStuckQueue(deps)` on `Server`, wired in `main.go` next to `startSweep`, only when at least one arr
  and the media-agent are configured. Otherwise the card says what is missing (like `sweepConfigured`).

## Tests

- `internal/client`: `StuckQueue` sends `includeUnknownSeriesItems`/`includeUnknownMovieItems` and
  filters to the stuck states; `ManualImportCandidates` sends `downloadId` and
  `filterExistingFiles=false` and never `seriesId`; both keep the base URL out of error text.
- `internal/stuckqueue`: table test with a fake arr (httptest) and a fake media-agent for each kind,
  the `minAge` cut-off, the `EpisodesHaveFiles` → suggested-mode mapping, and "a failed call still
  lists the item".
- `internal/server`: the fragment renders through `ExecuteTemplateForTest` with every kind; remove
  without `Sec-Fetch-Site` is refused; remove sends exactly the flags for each mode (fake arr records
  the DELETE query); remove refuses when the record is gone or its `downloadId` changed; remove
  survives a cancelled request context.

## Live facts to check before building

1. Sonarr/Radarr versions on media-0 now; re-read `QueueController`/`ManualImportController` if either
   moved far past 4.0.19.
2. Whether `added` is present on queue records in this version (age source). If not, use the matching
   grab's history date.
3. The path namespace of `outputPath`: is it the path media-agent sees (`/mnt/decypharr/...`), or does
   it need a remote-path mapping?
4. Radarr's field names (`movieId`, `includeUnknownMovieItems`) and its `manualimport` response shape.
5. With `removeFromClient=true` on a split season hash decypharr never stored: does decypharr's qBit
   delete return success, or an error that makes Sonarr keep the record?
6. Current count and kinds of stuck records on Sonarr and Radarr (the JoJo leftovers may be cleaned up
   already); the live acceptance needs at least one.
7. The exact `statusMessages[].title` strings for each kind, so the card can show them verbatim.

## Follow-ups (not v1)

- `arr_queue` / `arr_queue_remove` agent tools and an incident trigger (periodic sweep on the same
  `Scan`), per the source notes' "Missing" list.
- Manual import for the unknown-series kind (`POST manualimport` reprocess with a chosen series, drop
  anything with rejections, submit via the `ManualImport` command), owner-approved.

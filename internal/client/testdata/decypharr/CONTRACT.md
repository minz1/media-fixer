# decypharr contract (owned by media-fixer)

What media-fixer relies on from decypharr. Shapes were taken from decypharr's
production source at 54a71c18 (pkg/server/routes.go, pkg/server/api.go,
pkg/storage/types.go, pkg/storage/repair.go, pkg/repair/service.go). The JSON
files next to this one are the recorded responses; media-fixer's contract tests
(internal/client/decypharr_contract_test.go,
internal/livecheck/decypharr_contract_test.go) fail when a field listed here
leaves them.

decypharr: before shipping any change that touches a line below, post to
mediafixer on the board. Your own contract test can serve these fixtures'
fields from your real types and fail when one goes.

## Transport

- Base URL from media-fixer's `[decypharr] url`; every call sends
  `Authorization: Bearer <token>` when a token is set.
- Any status >= 300 is an error. A 404 on `GET /api/torrents` means the route is
  gone (never read as "no torrents").

## Endpoints

| Call | media-fixer use | Request | Response fields read |
|---|---|---|---|
| `GET /api/torrents?search=&state=&page=&limit=100` | get_torrent_state tool, list_alternatives (TorrentByHash: search by lower-case info hash, paged), livecheck fixtures | query only; `search` matches name + info hash | `torrents[]`, `total`; per entry `name`, `info_hash`, `category`, `state`, `size`, `progress`, `added_on`, `bad`, `active_provider`, `original_filename` |
| `POST /api/repair/run` | refresh_decypharr_links (the dead-torrent route) | JSON `{"auto_repair": true, "unrestrict_link": true}` | `run_id` |
| `POST /api/repair/run` | decypharr_repair_sweep | JSON `{"auto_repair": true}` | `run_id` |
| `GET /api/repair/status` | get_repair_status tool (passed through raw), livecheck "is a repair running" | none | `active_run` present and not null = running; `active_run.stage` |
| `GET /api/repair/health` | get_repair_health tool (raw), livecheck entry discovery | none | array; per entry `entry_name`, `status` |
| `POST /api/repair/health/{name}/check[?fix=true]` | decypharr_recheck | name path-escaped, no body | status only |
| `POST /api/mount/cache/cleanup` | decypharr_cache_cleanup | no body | status only |

media-fixer no longer deletes or adds torrents itself: a dead torrent goes to
the repair sweep below, which re-adds it inside decypharr.

## Repair semantics media-fixer counts on

- The dead-torrent route is the repair sweep with both `auto_repair` and
  `unrestrict_link`. Without `unrestrict_link` a hash the provider dropped reads
  as a probe error, not broken, and is not healed.
- With both flags, repair re-inserts a dead torrent itself: it activates a
  completed placement on another provider, else re-submits the magnet (or a
  hash-only magnet built from info hash and name) on each configured provider,
  cached content only, then deletes the old provider torrent. When every
  provider refuses, the entry is marked bad and queued for the Arr recheck.
  Torrents only; NZB entries go to Arr reacquisition.
- The targeted `POST /api/repair/health/{name}/check?fix=true` runs the same
  re-insert but has no `unrestrict_link` today, so it misses a dropped hash
  unless the entry is already bad. media-fixer stays on the sweep until
  decypharr ships an `unrestrict_link` flag for the targeted check.
- With `auto_repair` (sweep) or `fix=true`, an entry still broken after the
  re-insert gets its Arr library file deleted and reacquired by decypharr,
  behind its repair breaker. That is decypharr's fix path, not media-fixer's.

## Paths and folder layout

- FUSE mount `/mnt/decypharr` (must be a mount point); content under
  `/mnt/decypharr/__all__/<folder>/`, older layout `/mnt/decypharr/<folder>/`.
- Library files under `/data/library/{tv,anime,movies}/` are symlinks into
  `/mnt/decypharr/__all__/<folder>/`.
- Arr download folders: `/data/downloads/<category>/<folder>` (categories
  `sonarr`, `radarr`). A torrent folder is named after the debrid torrent name
  (folder_naming `original_no_ext`), not Sonarr's grab title; an NZB folder is
  the NZB file name without extension. Match by download id, not by title.
- Cache directory `/var/cache/decypharr`; systemd unit `decypharr.service`
  (restart_decypharr, via media-agent on media-0).

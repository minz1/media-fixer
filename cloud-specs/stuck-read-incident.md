# Stuck reads become incidents

Spec only. Written 2026-10-27 against `main` at `b3e6afb` plus the `cloud/backlog-2026-10-26` branch.
Source: notes on the removed ffprobe-monitor (`nixfiles/pkgs/ffprobe-monitor/ffprobe-monitor.sh`).

## Problem

A read on a media file can wedge in uninterruptible IO wait (state `D`) on the decypharr FUSE mount.
The old ffprobe-monitor saw this (15 ALERTs on one Mushoku Tensei episode on 2026-10-09) but only
wrote to syslog, so nobody ever acted on it. media-fixer already handles "this file is unreadable"
(refresh links, recheck, delete-and-readd), but only once a person reports it.

## Decision: where detection lives

- **media-agent** does the detection. It is the only media-fixer process on media-0, already runs as
  root with `ProtectProc=default`, `ProcSubset=all`, `PrivatePIDs=no`, and already serves `/ls`,
  `/dd-test`, `/disk`. A new endpoint keeps the host change at zero.
- media-agent stays **stateless**: an on-demand scan, not a background loop. media-fixer core polls
  it and owns all state (persistence across polls, dedupe, incidents). That matches the pull model
  of the existing endpoints, and makes both halves testable on their own.
- **Dropped** from the old monitor: the poke (unproven; on FUSE a second read queues behind the first),
  `/tmp` rate-limit files, md5 path keys, the `ffprobe`-only match.
- **Kept**: the 30 s minimum age and the per-file threshold idea.

## Design

### media-agent (`internal/mediaagent`, `internal/mediaagentapi`)

- `mediaagentapi`: `StuckReader{PID, TID int; Comm string; AgeSeconds int; Path string; Via string}`
  (`Via` is `"syscall"` or `"fd"`) and `StuckReadsResult{Readers []StuckReader}`.
- `Ops` gains `StuckReads(minAge time.Duration) (*StuckReadsResult, error)`. `RealOps` gets a
  `procRoot string` field (default `/proc`) so tests point it at a fixture tree.
- Scan, for every numeric `procRoot/<pid>/task/<tid>`:
  1. Parse `stat` after the last `)`: field 3 is the state, field 22 the start time in clock ticks.
     Keep `D` only. Age = `/proc/uptime` − start time / `CLK_TCK` (100 unless the live read says
     otherwise); drop under `minAge`. Task age is a floor on time stuck, not the exact time; the
     core's persistence rule covers the gap.
  2. Find the file. First `task/<tid>/syscall`: for `read`/`pread64`/`readv`/`preadv` (x86_64 numbers
     0/17/19/295) the first argument is the fd; `readlink <pid>/fd/<fd>`. If `syscall` is unreadable
     or another syscall, fall back to every `fd/*` link of the process.
  3. Keep only regular-file targets under the configured mounts (`resolveUnderMounts`, the same roots
     as `/ls`: `/mnt/decypharr`, `/data`). Sockets, pipes, `anon_inode:` and deleted files are skipped.
  4. Skip decypharr's own processes (by `comm`, configurable): its FUSE workers can sit in `D` legitimately.
- `GET /stuck-reads?min_age_s=30` behind the existing `bearerAuth`. The handler never opens a media
  file, so the scan itself cannot wedge.
- `client.MediaAgentClient.StuckReads(ctx, minAge)` mirrors `ListDirectory`.

### media-fixer core (new package `internal/stuckread`)

- `Watcher{agent, svc *incident.Service, db, log, now}` with `Poll(ctx)`. Started from `main.go` on a
  one-minute ticker when the media-agent is configured, under the process-lifetime context like
  `startSweep`.
- Per poll: group readers by `Path`. Remember each `(pid, tid, path)` with the first time it was seen.
  A path becomes a **finding** when either:
  - 3 or more distinct tasks are stuck on it in this poll, or
  - one task has been seen stuck on it in polls at least 5 minutes apart.
  Thresholds are constants, overridable from a new `[stuckread]` config block.
- A path that has no stuck tasks in a poll is forgotten. A cleared path logs once at INFO.
- One incident per path:
  - Before calling `svc.Handle`, check an in-memory `path → incident ID` map and that the incident is
    still active (`db.GetIncident`; status open, investigating, verifying or reopened, the set
    `db.activeIncidentStatuses` defines, which would need an exported accessor). `Handle`'s own title
    dedupe adds a reporter row on every call, so the watcher must not call it on every poll.
  - Report: `Source: "media-agent"`, `ReportedBy: "media-agent"`, `What: "stuck_read"`, `Title`: the
    torrent folder name from the path (`/mnt/decypharr/__all__/<torrent>/…`), else the file's base
    name. `Details`: path, readers (comm, pid, age), how it was found (`Via`).
  - The map is rebuilt on start from active incidents with `Source == "media-agent"` (path is in
    `Details`), so a restart does not open a second incident for the same file.
- Notifications go only through the existing incident flow (owner DMs, no reporter DMs: there is no
  Discord reporter).

### Agent (`internal/agent/agent.go`)

- New prompt section `--- Stuck read (what=stuck_read) ---`:
  - Do **not** run `dd_readability_test` on the stuck path: the read would wedge in `D` too and park a
    media-agent goroutine. `list_directory` on its folder and `get_torrent_state` are safe.
  - Go to `get_repair_status` → `decypharr_recheck` by name → `refresh_decypharr_links`; if those were
    already tried on this incident, escalate `delete_torrent_readd` (existing approval flow).
- Verification: the existing `VerifyResolved` path, plus a watcher check that the path has had no
  stuck tasks for two polls before the incident can resolve. Wire this as a `stuck_read` branch in
  `Service.runVerification` that asks the watcher (`Watcher.Clear(path) bool`).

## Tests

- `internal/mediaagent` (fake `/proc` under `t.TempDir()`: `uptime`, `<pid>/task/<tid>/{stat,syscall}`,
  `<pid>/fd/<n>` symlinks to real files under a fake mount root):
  - three `D` tasks on one file older than 30 s → three readers, one path;
  - a `D` task younger than 30 s → none; state `S` → none;
  - fd pointing outside the mounts, or at `socket:[123]` → none;
  - unreadable `syscall` → falls back to the fd table (`Via == "fd"`);
  - a decypharr `comm` → skipped;
  - `comm` containing `)` and spaces parses correctly.
- `internal/stuckread` (fake media-agent via httptest, real `incident.Service` on a temp DB, fake agent):
  - the same file over several polls opens exactly one incident and one reporter row;
  - one reader seen in polls under 5 min apart → no incident; 5 min apart → one;
  - a restart (new Watcher, same DB) does not open a second incident;
  - after a readd and verify, the incident resolves only once the watcher has seen the path clear.
- `internal/agent`: the system prompt's `stuck_read` section forbids `dd_readability_test` on the path.

## Live facts to check before building

1. `getconf CLK_TCK` and the architecture on media-0 (syscall numbers above are x86_64).
2. Can media-agent read `/proc/<pid>/task/<tid>/syscall` for other users' processes under its unit's
   hardening (`ProtectProc`, `CapabilityBoundingSet`)? It needs `CAP_SYS_PTRACE`-level access.
3. Which processes actually sit in `D` during a hang on the hanwen FUSE backend: ffprobe, Jellyfin
   ffmpeg, Sonarr/Radarr .NET threads, decypharr's own workers? This sets the decypharr exclusion.
4. Whether a reader's fd links show `/mnt/decypharr/__all__/<torrent>/<file>` or a `/data/library`
   path (the mount roots in the filter must cover both).
5. How many concurrent Jellyfin readers of one file are normal (sets the "3 tasks" threshold).
6. The live acceptance from the notes: SIGSTOP decypharr (or read a known-dead entry), confirm an
   ffprobe in `D`, the endpoint reports it, one incident opens, resume, the incident resolves.
   nixfiles coordinates the pause with the owner, since it stops all playback.

# Cloud backlog 2026-10-26: report

Branch `cloud/backlog-2026-10-26`, based on `main` at `b3e6afb`. One commit per item, in the task's
order, then the two specs, then this report. No PR, no tags, nothing pushed but this branch.

## Tooling and gate

- Go 1.27.0 (auto toolchain). golangci-lint **v2.14.0** built with go1.27.0: that is the version the
  repo's `flake.lock` nixpkgs pins. The latest release (v2.15.0) reports one finding that is already
  on `main` (`exhaustive` on `internal/livecheck/render.go:95`, and it renames the `exhaustive`
  `map` option), so 2.15 would fail the gate on untouched `main`. Worth knowing for the next flake bump.
- Before every commit, from the repo root with `set -o pipefail` and each exit code checked:
  `go build ./... && go vet ./... && go test -race ./... && golangci-lint run ./...`. All green at
  every commit. (My first gate script read `PIPESTATUS` too late and would have passed a lint failure;
  that run is the I1 golines finding, which was fixed before I1 was committed. The script was then
  corrected; every commit's lint run printed `0 issues.`)

## Items

### S1. Dashboard Resolve, Keep searching and Approve survive a dropped request: `e785a53`
- Tests: `TestActionResolve_SurvivesCancelledRequest`, `TestActionKeepSearching_SurvivesCancelledRequest`
  (`dashboard_test.go`), `TestApproveEscalation_SurvivesCancelledRequest` (`escalation_test.go`).
  All three failed first with HTTP 500 `context canceled`, then passed.
- Fix: the three handlers pass `context.WithoutCancel(r.Context())`, as `actionRerun` does.
- Choice: added a `serveCancelled` test helper for the new tests; the existing Rerun test is unchanged.
  `ApproveEscalation`'s own inner `WithoutCancel` is left in place (now redundant, harmless).

### S2. SSE initial-render test flake: `3813e7d`
- Flake proof: 6 parallel copies of the race binary, `-test.run` on the test. **330 runs, 0 failures**
  (210 plain; 120 more with 8 extra busy loops on the 4-core box). The cause is clear in the code:
  `dashboardEvents`' first `render()` uses the request context, so when the 150 ms deadline expires
  before `ListIncidents` returns, nothing is written. Squeezing the old timeout to 1 ns failed
  every time with an empty body, which confirms the mechanism.
- Fix (test only): read the stream from a real `httptest.Server` until the incidents-list event naming
  the incident arrives, under a 10 s deadline. After the fix: 210 parallel runs, 0 failures.
- Not touched: `TestIncidentEvents_OpenIncidentStreamsThenClosesOnContextDone` still uses the shared
  150 ms `sseTestTimeout` for its initial render and could flake the same way. Out of the item's scope.

### S3. Server.Start shutdown test flake: `c82f927`
- Flake proof: same method, **210 runs, 0 failures** before the fix. The race is clear in the code
  (`freePort` released the port before `Start` bound it; its own comment said "Racy in principle").
- Test first: the test now binds `127.0.0.1:0` itself and calls `srv.UseListenerForTest(l)`; it failed
  to compile until the seam existed.
- Seam: an unexported `listener net.Listener` field on `Server`; `Start` calls `http.Server.Serve` on it
  when set, else `ListenAndServe` as before. Set only via `UseListenerForTest` in `export_test.go`.
  `freePort` is gone (no other user). After the fix: 210 parallel runs, 0 failures.

### I1. Recover incidents left `reopened`: `c36444a`
- Tests: `TestFindStaleReopened` (db, failed to compile first), `TestRecoverZombies_RerunsReopenedIncident`
  and `TestSweepStaleRuns_RerunsStaleReopenedIncident` (both failed first: timed out waiting for a
  rerun), plus guards `TestSweepStaleRuns_LeavesFreshReopenedIncident` and
  `TestSweepStaleRuns_LeavesReopenedIncidentWhilePaused` (new file `internal/incident/recovery_test.go`).
- Fix: `db.FindStaleReopened` (status `reopened` and `updated_at` before the cutoff, no event join, as
  asked). `RecoverZombies` loops over `investigating` and `reopened`. `SweepStaleRuns` also sweeps stale
  `reopened` incidents with `rerunStale`, using the same `staleRunThreshold`.
- Choice not in the brief: the `reopened` sweep is **skipped while autonomous actions are paused**.
  Without it, a paused system loops: `runAgent` returns early on pause and leaves the incident
  `reopened`, and every rerun bumps `updated_at`, so the owner would get a "stopped making progress"
  DM about every 20 min per incident until unpaused. I checked that the paused test fails with
  the guard removed. The `investigating` sweep is unchanged.
- Also: a failed `investigating` query no longer returns early, so the `reopened` sweep still runs.
- Not done: a `reopened` incident whose live run is still queued on the global diagnostic slot for over
  15 min is rerun (which cancels the queued run and re-queues it). This is the spec's criterion; I
  did not add a "has a live run" exception.

### I2. Rerun that loses its run on a DB error: `d39c452`
- Tests (new file `internal/incident/rerun_dberror_test.go`, real DB, existing `blockingAgent`):
  `TestRerun_ReadFailureLeavesTheRunningRunAlone` (closed DB; failed first: the old run was cancelled)
  and `TestRerun_StatusWriteFailureAfterCancelLogsError` (a SQLite `BEFORE UPDATE OF status` trigger
  refuses only the status write; failed first: no ERROR log).
- Fix: `rerun` reads the incident before cancelling the old run. `runManager.supersede` is `begin` plus
  a "replaced a live run" flag (`begin` now wraps it). When the status write fails after a live run was
  cancelled, it logs ERROR `rerun cancelled the running run and started no replacement`.
- Choice: since the incident is now read before the lock is cleared, the in-memory copy gets
  `AutonomousLocked = false` (only if the clear succeeded) and `Status = reopened`, so `runAgent` sees
  what the old post-write read saw.
- Failing-DB seam: done in the tests (closed DB, SQLite trigger), no production seam needed.

### L1. Loki mTLS keeps the cloned transport: `21b374d`
- Test: `TestNewLoki_MTLSKeepsTheClonedTransport` (new `loki_mtls_test.go`, generates a throwaway
  cert/key). Failed first: Proxy nil, TLS handshake/idle/expect-continue timeouts 0, DialContext nil.
- Fix: set `TLSClientConfig` on the cloned `*http.Transport`. The bare-transport path stays only for
  the case where the transport isn't an `*http.Transport` (never true today).
- Added `internal/client/export_test.go` (the package had none) with `HTTPClientForTest`.

### W1. Corrupt sweep settings row: warn once: `b23a478`
- Test: `TestScheduler_CorruptSettingsWarnsOncePerDistinctError` (new `scheduler_warn_test.go`). Failed
  first: 3 WARNs for 3 ticks, 6 after the error changed, no INFO when cleared.
- Fix: `Scheduler.Tick` remembers the last warned error text (under its own mutex). It warns only when
  the text changes, and logs INFO `pack sweep scheduler error cleared` once when a tick succeeds again.
- Choice: this applies to every error `Tick` warns about (all go through the same WARN), not only the
  settings row. `ErrRunActive`/`ErrPaused` stay silent, and they count as "no error" for clearing.

### B1. `buffering` incident type: `832cd68`
- Tests: `TestReportCommand_OffersBuffering` (discord; failed first, compile), the system-prompt test
  `TestSystemPrompt_BufferingSectionLeadsWithReadSpeed` (agent; failed first, no section), and
  `TestHandle_CreatesBufferingIncident` (incident). The last one **passed before the change**:
  `Handle` doesn't validate `What`, so it is a pinning test.
- Fix: `/report` gets "It plays but buffers / low quality" → `buffering` (command built by a new
  `reportCommand()`, exposed via `ReportCommandForTest`). The system prompt's playback protocol now
  covers `what=buffering`, and a new "Buffering / low quality" section judges Step 4 by `speed_mb_s`:
  rough needs (1 MB/s 1080p web, 5 MB/s 1080p remux, 10+ MB/s 4K, under 10 if unknown), slow →
  `refresh_decypharr_links` with deferred verification, fast → `manual_investigation`, EIO/not_found
  follow cant_play rules, never `remove_and_search` for slowness alone. The `Report.What` comment lists
  the new value.
- Choice: the guidance lives in the system prompt (keyed on `what=`, like every other type), so
  reruns seeded by `BuildSummarySeed` get it too. The MB/s figures are my estimates; tune them live.
- Seerr ingest: **left alone**. Seerr issue types are VIDEO/AUDIO/SUBTITLES/OTHER, and none of them
  means "buffers".
- Dashboard: shows `.What` raw, no label mapping, so there was nothing to change.
- Caveat: verification for a buffering fix uses the same `VerifyResolved` signature as cant_play.
  It does not measure speed, so a refreshed-but-unchanged item may end as "could not be verified".

### T1. Self-test single-file override: `6102b2b`
- Tests (new `override_test.go`, through the real `discoverTorrentAndSample` via a new export hook):
  `TestOverride_SingleFileTorrentListsItsFolder` failed first (`TorrentIsFile` false, `list_directory`
  ENOENT on `/mnt/decypharr/<file>`). `TestOverride_MultiFileTorrentListsTheTorrentFolder` passed
  before and after (guard).
- Fix: when `sample_path` is preset and a torrent name is known, read `is_dir` for the torrent's
  `__all__` entry with discovery's own `singleFileTorrentPath` (lists `__all__`, matches name or
  original filename, uses the `is_dir` flag), and set `TorrentIsFile` from it.
- Also fixes the case where only `sample_path` is overridden and the torrent name is discovered.

### C1. Cleanup: `08f4927` (−39 lines net; no test file changed)
- Done: `sweep.RefusedMemory` → `refusedMemory`; `db.stringSet` inlined into `NoPackTargets`;
  every Arr GET passes a relative path plus `url.Values` (the `http://` branch is gone, and the base URL
  is out of error text); `MarkHistoryFailed` goes through `do`.
- **Skipped one sub-bullet: dropping `doAs` / giving `postCommand` do's label.** The existing
  `TestArr_ThinCalls_KeepErrorPrefixes` pins postCommand's `"arr command: "` error prefix, so that
  change isn't a pure refactor under "existing tests must pass unchanged". `doAs` stays with
  `postCommand` as its only direct caller. If you want the label change, update that test's prefix
  to `"arr POST /api/v3/command: "` and the `doAs` removal is a few lines.
- Choice: I read "skip that bullet" as skipping only the part that broke the rule. The other Arr parts
  are separable and pass every existing test. Drop them on cherry-pick if you read it strictly.
- Behaviour notes: `MarkHistoryFailed` failures now wrap `*HTTPStatusError` and say
  `arr POST /api/v3/history/failed/<id>: status N: <body>`. Its only caller (`ExecuteReplace`) just
  returns the error.

## Specs

- D1 `cloud-specs/stuck-arr-queue-card.md`: `53c64e1`
- D2 `cloud-specs/stuck-read-incident.md`: `16ddf06`

## Comments rule

No new comments. Two existing comments were edited so they stay true: the `RecoverZombies` doc
("investigating" → "investigating" or "reopened") and the inline `Report.What` value list. Two were
deleted along with the code they described: the obsolete `get` doc (absolute-URL footgun) and
`freePort`'s doc.

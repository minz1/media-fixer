# Autopilot rules: mediafixer

## Checks
Per item (from the repo root, every step's exit code checked, stop on the first failure):
`go build ./... && go vet ./... && go test -race ./... && golangci-lint run ./...`
Before every push, also `nix build .#media-fixer .#media-agent .#media-fixer-check` (catches a stale vendorHash and Go pin).
If the disk fills ("no space left"), `go clean -cache` and rerun; never commit on a partial gate.
TDD: the item's tests are written first and seen failing (record the failing output in log.md), then pass. Sweep rules
are tested against the real Sonarr fixtures in `internal/sweep/testdata/`; don't replace them with synthetic data.
Final item: the full gate on `main`, `git status` clean apart from the untracked `ROADMAP.md`, `main` == `origin/main`.

## May happen mid-run
- Commits on `main` (the run branch; nixfiles' `mediafixer` input follows github:minz1/media-fixer's default branch).
  Style: `media-fixer: <what>`, one subject line, no Co-Authored-By trailer.
- `git push origin main`, fast-forward only, only at a plan "Ship deploy" item, after its gate and the Opus reviewer pass.
  Never force-push. No tags, no releases, no deploys from here.
- One deploy per board post: `mediafixer-N → nixfiles: <full sha>, what changed, go.mod changed yes/no, migration N
  (additive: an older binary ignores it), host (services-0 media-fixer or media-0 media-agent), what to verify live`.
  Do not push the next deploy until `ack mediafixer-N` appears in nixfiles.md (exit WAIT 20 meanwhile); later tasks
  may be built and committed locally while waiting. Live behaviour arrives as `watch mediafixer-N: <result>`; nothing waits on it.
- Schema changes: additive only (new table, new index, `ADD COLUMN`), timestamps `DATETIME`. Anything else is on the stop list.
- Live reads (Sonarr/Seerr APIs, Loki, the media-fixer DB, a Dry run on the live dashboard) are board requests to nixfiles.
- Never edit ~/projects/nixfiles.

## Reviewer stand-in
Before every push: the `reviewer` agent with `model: opus` on the commit range being pushed, plus its ponytail-review
pass. Fix every correctness/safety finding and re-review; record skipped nits in log.md with the reason. The reviewer
also judges irreversibility: if it says a change can't be undone by a rollback to the previous binary, STOP.

## Fleet
Subagents via the Agent tool only, in-session: Sonnet to implement one plan task (give it the plan's header sections,
the task text and the spec path), Haiku for read-only lookups, Opus for the reviewer. One linear branch, no worktrees.
None outlive the session.

## Stop list
Exit STOP for:
- a schema or data change a rollback to the previous binary can't undo (dropping/renaming columns or tables, rewriting
  rows, changing a stored JSON shape that old code reads)
- a go.mod change (new or bumped dependency)
- live grabs above the configured grab cap; enabling the sweep, a non-dry run, or changing sweep settings on the live
  host (the ramp is the owner's, in the UI)
- changing any Sonarr/Radarr/Seerr setting, including `autoRedownloadFailedFromInteractiveSearch`
- any direct access to services-0 or media-0 (SSH, API calls, Loki, the live DB): live reads go through the board
- a nixfiles nack, or an ack or watch line reporting a regression from our deploy (stop and wait for the owner; nixfiles rolls back)
- weekly utilization above the plan's ceiling
- a design choice the spec/plan doesn't settle (naming, new options, behaviour the owner hasn't approved)
- the rename (handoff item 4) or anything in the Parking lot

## Where to look for more work
Bounded scope: nothing beyond the plan's queue. Findings go to the Parking lot.

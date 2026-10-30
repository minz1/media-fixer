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
- Commits on the run branch `autopilot/<run>`. Style: `media-fixer: <what>`, one subject line, no Co-Authored-By trailer.
- Shipping (paired with nixfiles): after the item's gate and the Opus reviewer pass over the range, push the prefix as
  a review ref, `git push origin <sha>:refs/heads/ship/<id>`, and post one line per deploy:
  `<id> → nixfiles: <full sha> (ship/<id>, item <n>), what changed, go.mod changed yes/no, migration N (additive: an
  older binary ignores it), host (services-0 media-fixer, media-0 media-agent, or both in one post when an item
  changes both), what to verify live, and any nixfiles option to set (e.g. services.media-fixer.publicURL)`.
  nixfiles locks its input to that sha and deploys it. On `ack <id>`, fast-forward local main with
  `git push . <sha>:main`, then `git push origin main` (fast-forward only) once the owner-proxy has ruled the push.
  `ahead-cap` limits unacked work; later items may be built and committed meanwhile. Never force-push, no tags, no
  releases, no deploys from here.
- Schema changes: additive only (new table, new index, `ADD COLUMN`), timestamps `DATETIME`. Anything else is on the stop list.
- Live reads (Sonarr/Seerr APIs, Loki, the media-fixer DB, a Dry run on the live dashboard) are board requests to nixfiles; they are
  peer reads, never questions for the owner.
- Never edit ~/projects/nixfiles.

## Reviewer stand-in
Before every push: the `reviewer` agent with `model: opus` on the commit range being pushed, plus its ponytail-review
pass. Fix every correctness/safety finding and re-review; record skipped nits in log.md with the reason. The reviewer
also judges irreversibility: if it says a change can't be undone by a rollback to the previous binary, STOP.

## Decision stand-in
The owner granted the `owner-proxy` agent L3 (full autonomy) for this run (owner, 2026-10-29). Bring it every
decision the spec/plan doesn't settle, every push at a "Ship deploy" item (after the Opus reviewer pass), and whether a
plan needs another review before execution. Log each RULING in log.md; `[review]` rulings also go to the end summary.
A PARK (its hard floor: money, secrets, destructive or irreversible steps, access changes, posting outside the
owner's repos) is a STOP. It decides; `reviewer` still reviews code.

## Skills
work: superpowers:executing-plans for an item with a plan (log.md is its ledger); superpowers:writing-plans for an
  item with a spec and no plan; superpowers:brainstorming for an item with neither, with the owner-proxy agent
  answering as the owner; superpowers:test-driven-development inside every task (tests first, seen failing)
retry: superpowers:systematic-debugging (root cause before a second attempt)
done: superpowers:verification-before-completion (gate output read in this session before [c]/[x])
review: superpowers:receiving-code-review (verify each reviewer finding against the code before acting)

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
- session or weekly utilization above 0.8
- a design choice the spec/plan doesn't settle, only when the `owner-proxy` agent parks it (see Decision stand-in)
- approving an AI escalation (delete + re-search, torrent re-add), a Discord Confirm, or any other destructive action
  the agent proposes: owner preview and approve only
- the rename (handoff item 4) or anything in the Parking lot

## Where to look for more work
Bounded scope: nothing beyond the plan's queue. Findings go to the Parking lot.

## Long commands
`nix build .#media-fixer .#media-agent .#media-fixer-check` and the full `go test -race ./...` can outlast a tool
timeout: give them an explicit 20-minute timeout, or run detached and exit `WAIT 30 pid=<pid>`.

## Shared tool locks
decypharr's run uses golangci-lint and the Go build cache at the same time: on "parallel golangci-lint is running" or
a cache lock, wait and retry (up to 10 minutes) instead of failing the item.

## Peers
Run `2026-10-29-media` with nixfiles (serves decypharr and mediafixer, one peer deploy per session, on the critical
path for every ship) and decypharr (independent). Board `~/.autopilot/2026-10-29-media/board/`, own file
`mediafixer.md`. Live reads are board requests to nixfiles; answer nothing for decypharr unless addressed.

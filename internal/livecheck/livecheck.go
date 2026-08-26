// Package livecheck drives the real agent.Dispatcher against the real
// configured services (Jellyfin, decypharr, Sonarr/Radarr, Loki, the
// media-agent sidecar) and reports, per tool, whether it actually works —
// the question that's historically only been answered by an incident coming
// in and the agent failing mid-diagnosis.
package livecheck

import (
	"context"
	"sort"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
)

// Status is the outcome of exercising one tool or environment check.
type Status string

const (
	// StatusOK means the tool ran and returned a usable result.
	StatusOK Status = "ok"
	// StatusDegraded means the call succeeded but couldn't be fully
	// exercised — no fixture was available, or the result was empty in a
	// way that more likely indicates a wrong query than a broken service.
	StatusDegraded Status = "degraded"
	// StatusFail means the call errored.
	StatusFail Status = "fail"
	// StatusMissing means the call 404'd — the endpoint isn't present on
	// this build/branch of the service, distinct from a transient failure.
	StatusMissing Status = "missing"
	// StatusSkipped means the check was gated behind -write/-disruptive (or
	// a repair/scan already in progress) and intentionally not run.
	StatusSkipped Status = "skipped"
	// StatusUnconfigured means the client this tool needs isn't configured
	// (e.g. media_agent.url is empty).
	StatusUnconfigured Status = "unconfigured"
)

// Result is the outcome of one tool's check.
type Result struct {
	Tool      string        `json:"tool"`
	Risk      string        `json:"risk"`
	Status    Status        `json:"status"`
	Detail    string        `json:"detail,omitempty"`
	Error     string        `json:"error,omitempty"`
	LatencyMS int64         `json:"latency_ms"`
	Latency   time.Duration `json:"-"`
}

// Report is the full output of one Runner.Run call.
type Report struct {
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration_ns"`
	Options   Options       `json:"options"`
	Fixtures  Fixtures      `json:"fixtures"`
	Results   []Result      `json:"results"`
	Env       []Result      `json:"env"`
}

// Options controls which tiers of checks actually run.
type Options struct {
	// AllowWrite additionally runs safe, idempotent write actions (cache
	// cleanup, rescans, cache refresh, repair sweeps).
	AllowWrite bool `json:"allow_write"`
	// AllowDisruptive additionally runs service restarts and library scans.
	AllowDisruptive bool `json:"allow_disruptive"`
	// Only, if non-empty, restricts the run to these tool names.
	Only []string `json:"only,omitempty"`
	// FixtureOverrides seeds or replaces auto-discovered fixtures (see
	// config.SelfTestConfig).
	FixtureOverrides Fixtures `json:"-"`
}

// Runner exercises every registered check against a live Dispatcher.
type Runner struct {
	disp *agent.Dispatcher
	opts Options
}

// New builds a Runner. disp's clients should be the same ones the real agent
// uses (see internal/clientset), so a live-check result reflects exactly
// what an incident would see.
func New(disp *agent.Dispatcher, opts Options) *Runner {
	return &Runner{disp: disp, opts: opts}
}

// Run discovers fixtures, then runs every registered check (subject to
// Options), and returns the full report.
func (r *Runner) Run(ctx context.Context) *Report {
	start := time.Now()
	report := &Report{StartedAt: start, Options: r.opts}

	fx := discoverFixtures(ctx, r.disp, r.opts.FixtureOverrides)
	report.Fixtures = fx
	report.Env = runEnvChecks(ctx, r.disp)

	only := toSet(r.opts.Only)
	for _, spec := range checkRegistry() {
		if len(only) > 0 && !only[spec.Tool] {
			continue
		}
		report.Results = append(report.Results, r.runOne(ctx, spec, &fx))
	}

	sort.SliceStable(report.Results, func(i, j int) bool {
		return statusRank(report.Results[i].Status) < statusRank(report.Results[j].Status)
	})

	report.Duration = time.Since(start)
	return report
}

// runOne executes a single check, honoring the write/disruptive gates and
// timing the call.
func (r *Runner) runOne(ctx context.Context, spec checkSpec, fx *Fixtures) Result {
	if spec.Tier == tierWrite && !r.opts.AllowWrite {
		return Result{Tool: spec.Tool, Risk: string(spec.Tier), Status: StatusSkipped, Detail: "needs -write"}
	}
	if spec.Tier == tierDisruptive && !r.opts.AllowDisruptive {
		return Result{Tool: spec.Tool, Risk: string(spec.Tier), Status: StatusSkipped, Detail: "needs -disruptive"}
	}

	start := time.Now()
	result := spec.Run(ctx, r.disp, fx, r.opts)
	result.Latency = time.Since(start)
	result.LatencyMS = result.Latency.Milliseconds()
	result.Tool = spec.Tool
	result.Risk = string(spec.Tier)
	return result
}

// statusRank orders results fail-first, most-actionable-first, for display.
// statusDisplayOrder lists statuses fail-first, most-actionable-first; its
// index doubles as the rank statusRank returns.
func statusDisplayOrder() []Status {
	return []Status{StatusFail, StatusMissing, StatusDegraded, StatusUnconfigured, StatusOK, StatusSkipped}
}

func statusRank(s Status) int {
	order := statusDisplayOrder()
	for i, want := range order {
		if s == want {
			return i
		}
	}
	return len(order)
}

func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	set := make(map[string]bool, len(items))
	for _, i := range items {
		set[i] = true
	}
	return set
}

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

type DiagnosisKind string

const (
	DiagnosisFixNow         DiagnosisKind = "fix_now"
	DiagnosisDeferredVerify DiagnosisKind = "deferred_verify"
	DiagnosisAwaitDownload  DiagnosisKind = "await_download"
	DiagnosisNeedsApproval  DiagnosisKind = "needs_approval"
	DiagnosisManual         DiagnosisKind = "manual"
)

func diagnosisKinds() []DiagnosisKind {
	return []DiagnosisKind{
		DiagnosisFixNow, DiagnosisDeferredVerify, DiagnosisAwaitDownload, DiagnosisNeedsApproval, DiagnosisManual,
	}
}

type SearchTarget struct {
	MediaType string `json:"media_type"`
	Title     string `json:"title"`
	Season    int    `json:"season"`
	Episode   int    `json:"episode"`
}

type DiagnosisOutcome struct {
	Kind           DiagnosisKind  `json:"kind"`
	AfterSeconds   int            `json:"after_seconds,omitempty"`
	Target         *SearchTarget  `json:"target,omitempty"`
	SentNotTracked []SearchTarget `json:"sent_not_tracked,omitempty"`
	Plan           string         `json:"plan,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

var (
	errEscalateEnum         = errors.New("escalate_action must be one of " + strings.Join(escalateActionEnum(), ", "))
	errNegativeVerify       = errors.New("verify_after_seconds must be ≥ 0")
	errApprovalAndDeferred  = errors.New("approval and deferred verification are exclusive")
	errApprovalAndDownload  = errors.New("approval and download tracking are exclusive")
	errDownloadAndDeferred  = errors.New("download tracking and deferred verification are exclusive")
	errSearchNotApplied     = errors.New(toolArrSearchMissing + " was not applied in this run")
	errRemoveParamsRequired = errors.New("escalate_params is required for " + EscalateRemoveAndSearch)
	errReaddNameRequired    = errors.New("escalate_params.name is required for " + EscalateDeleteTorrentReadd)
	errDiagnosisInvalid     = errors.New("diagnosis outcome disagrees with its flags")
)

func isPlanKind(ea string) bool {
	return ea == EscalateRemoveAndSearch || ea == EscalateDeleteTorrentReadd
}

type classifyIn struct {
	r       *DiagnosticResult
	search  bool
	applied []SearchTarget
	last    *CallRecord
}

type classifyRow struct {
	match  func(in classifyIn) bool
	result func(in classifyIn) (DiagnosisOutcome, error)
}

func fail(err error) func(classifyIn) (DiagnosisOutcome, error) {
	return func(classifyIn) (DiagnosisOutcome, error) { return DiagnosisOutcome{}, err }
}

func classifyRows() []classifyRow {
	return []classifyRow{
		{func(in classifyIn) bool {
			return in.r.EscalateAction != "" && !slices.Contains(escalateActionEnum(), in.r.EscalateAction)
		}, fail(errEscalateEnum)},
		{func(in classifyIn) bool { return in.r.VerifyAfterSeconds < 0 }, fail(errNegativeVerify)},
		{func(in classifyIn) bool {
			return in.r.RequiresApproval && in.r.VerifyAfterSeconds > 0
		}, fail(errApprovalAndDeferred)},
		{func(in classifyIn) bool { return in.r.RequiresApproval && in.search }, fail(errApprovalAndDownload)},
		{func(in classifyIn) bool {
			return in.search && in.r.VerifyAfterSeconds > 0
		}, fail(errDownloadAndDeferred)},
		{func(in classifyIn) bool {
			return !in.r.RequiresApproval && in.search && len(in.applied) == 0
		}, func(in classifyIn) (DiagnosisOutcome, error) {
			if in.last == nil {
				return DiagnosisOutcome{}, fmt.Errorf("%w: %s", errSearchNotApplied, searchEarlierRunHint)
			}
			return DiagnosisOutcome{}, fmt.Errorf("%w: %s", errSearchNotApplied, lastOutcome(*in.last))
		}},
		{func(in classifyIn) bool {
			return !in.r.RequiresApproval && in.search
		}, func(in classifyIn) (DiagnosisOutcome, error) {
			n := len(in.applied)
			target := in.applied[n-1]
			return DiagnosisOutcome{
				Kind: DiagnosisAwaitDownload, Target: &target, SentNotTracked: slices.Clone(in.applied[:n-1]),
			}, nil
		}},
		{func(in classifyIn) bool {
			return !in.r.RequiresApproval && in.r.VerifyAfterSeconds > 0
		}, func(in classifyIn) (DiagnosisOutcome, error) {
			return DiagnosisOutcome{Kind: DiagnosisDeferredVerify, AfterSeconds: in.r.VerifyAfterSeconds}, nil
		}},
		{func(in classifyIn) bool { return !in.r.RequiresApproval }, func(classifyIn) (DiagnosisOutcome, error) {
			return DiagnosisOutcome{Kind: DiagnosisFixNow}, nil
		}},
		{func(in classifyIn) bool {
			return in.r.EscalateAction == EscalateRemoveAndSearch && len(in.r.EscalateParams) == 0
		}, fail(errRemoveParamsRequired)},
		{func(in classifyIn) bool {
			name, _ := in.r.EscalateParams[paramName].(string)
			return in.r.EscalateAction == EscalateDeleteTorrentReadd && name == ""
		}, fail(errReaddNameRequired)},
		{func(in classifyIn) bool {
			return isPlanKind(in.r.EscalateAction)
		}, func(in classifyIn) (DiagnosisOutcome, error) {
			return DiagnosisOutcome{Kind: DiagnosisNeedsApproval, Plan: in.r.EscalateAction}, nil
		}},
		{func(classifyIn) bool { return true }, func(in classifyIn) (DiagnosisOutcome, error) {
			return DiagnosisOutcome{Kind: DiagnosisManual, Reason: in.r.PrimaryReason}, nil
		}},
	}
}

func lastOutcome(rec CallRecord) string {
	switch rec.Status {
	case outcomeFailed:
		return "failed: " + rec.Err
	case outcomeRefused:
		return "refused: " + rec.Err
	case outcomeApplied, outcomeInterrupted, outcomeInvalid:
	}
	return "outcome unknown (interrupted)"
}

func Classify(r *DiagnosticResult, calls []CallRecord) (DiagnosisOutcome, error) {
	in := classifyIn{r: r, search: r.PrimaryAction == toolArrSearchMissing}
	for i := range calls {
		if calls[i].Tool != toolArrSearchMissing {
			continue
		}
		in.last = &calls[i]
		if calls[i].Status == outcomeApplied {
			in.applied = append(in.applied, searchTargetOf(calls[i].Params))
		}
	}
	for _, row := range classifyRows() {
		if row.match(in) {
			return row.result(in)
		}
	}
	return DiagnosisOutcome{}, errDiagnosisInvalid
}

func searchTargetOf(p map[string]any) SearchTarget {
	mediaType, _ := p[paramMediaType].(string)
	title, _ := p[paramTitle].(string)
	return SearchTarget{
		MediaType: mediaType, Title: title, Season: intParam(p[paramSeason]), Episode: intParam(p[paramEpisode]),
	}
}

func intParam(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}

func CheckDiagnosis(r *DiagnosticResult) error {
	o := r.Outcome
	owner := o.Kind == DiagnosisNeedsApproval || o.Kind == DiagnosisManual
	deferred := o.Kind == DiagnosisDeferredVerify
	checks := []struct {
		ok   bool
		what string
	}{
		{slices.Contains(diagnosisKinds(), o.Kind), "D1 kind " + string(o.Kind)},
		{owner == r.RequiresApproval, "D2 requires_approval"},
		{
			(o.Kind == DiagnosisNeedsApproval) == (o.Plan != "" && o.Plan == r.EscalateAction && isPlanKind(o.Plan)),
			"D3 plan",
		},
		{!deferred || (o.AfterSeconds > 0 && o.AfterSeconds == r.VerifyAfterSeconds), "D4 after_seconds"},
		{deferred || (o.AfterSeconds == 0 && r.VerifyAfterSeconds == 0), "D4 no deferral"},
		{(o.Kind == DiagnosisAwaitDownload) == (o.Target != nil), "D5 target"},
		{o.Kind != DiagnosisManual || !isPlanKind(r.EscalateAction), "D6 manual plan"},
		{o.Kind == DiagnosisAwaitDownload || len(o.SentNotTracked) == 0, "D7 sent_not_tracked"},
	}
	for _, c := range checks {
		if !c.ok {
			return fmt.Errorf("%w: %s", errDiagnosisInvalid, c.what)
		}
	}
	return nil
}

func SearchCalls(targets ...SearchTarget) []CallRecord {
	out := make([]CallRecord, 0, len(targets))
	for _, t := range targets {
		out = append(out, CallRecord{
			Tool:   toolArrSearchMissing,
			Status: outcomeApplied,
			Params: map[string]any{
				paramMediaType: t.MediaType, paramTitle: t.Title, paramSeason: t.Season, paramEpisode: t.Episode,
			},
			Counted: true,
		})
	}
	return out
}

type diagnosisRejectedError struct {
	err    error
	parsed bool
	result DiagnosticResult
}

func (e *diagnosisRejectedError) Error() string { return e.err.Error() }

func (e *diagnosisRejectedError) Unwrap() error { return e.err }

const (
	maxDiagnosisErrors   = 3
	reasonContradiction  = "the diagnosis contradicted itself"
	searchEarlierRunHint = "a search already applied on this incident in an earlier run does not count; " +
		"escalate or choose a different action instead of claiming it"
)

func parseDiagnosis(argsJSON string, calls []CallRecord) (DiagnosticResult, error) {
	var result DiagnosticResult
	if err := json.Unmarshal([]byte(argsJSON), &result); err != nil {
		return DiagnosticResult{}, &diagnosisRejectedError{err: fmt.Errorf("parse complete_diagnosis: %w", err)}
	}
	outcome, err := Classify(&result, calls)
	if err != nil {
		return DiagnosticResult{}, &diagnosisRejectedError{err: err, parsed: true, result: result}
	}
	result.Outcome = outcome
	return result, nil
}

func capDiagnosis(gate Gate, rejected *diagnosisRejectedError) (DiagnosticResult, bool) {
	gate.Record(CallRecord{Tool: toolCompleteDiagnosis, Status: outcomeInvalid, Err: rejected.Error()})
	n := 0
	for _, rec := range gate.Calls() {
		if rec.Tool == toolCompleteDiagnosis && rec.Status == outcomeInvalid {
			n++
		}
	}
	if n < maxDiagnosisErrors {
		return DiagnosticResult{}, false
	}
	reason := reasonContradiction
	if errors.Is(rejected, errSearchNotApplied) {
		reason += ": " + rejected.Error()
	}
	capped := DiagnosticResult{
		Confidence:       "low",
		PrimaryAction:    EscalateManualInvestigation,
		PrimaryReason:    reason,
		EscalateAction:   EscalateManualInvestigation,
		RequiresApproval: true,
		Outcome:          DiagnosisOutcome{Kind: DiagnosisManual, Reason: reason},
	}
	if rejected.parsed {
		capped.RootCause, capped.Confidence = rejected.result.RootCause, rejected.result.Confidence
	}
	return capped, true
}

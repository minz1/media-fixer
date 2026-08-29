// Package journal owns the incident_events append-only log: what each event
// Kind means, how its payload is shaped, what projection (if any) an append
// also writes, and fanning out live updates to subscribers. internal/db only
// stores and retrieves raw event rows — it has no opinion on any of that.
//
// This replaces four previously separate, hand-rolled mechanisms that each
// existed only because there was no log to query: last_disruption (a
// single-row table faking "the latest disruptive action"), last_heartbeat (a
// column touched once per LLM round to detect a hung run), conversation_history
// (a single upserted blob, overwritten every round — which is also why the
// final round's tool results were never persisted; Run returned on
// complete_diagnosis before the old SaveConversation call for that round ever
// ran), and re-deriving "what have we already tried" from actions_log alone.
// See the migBackfillIncidentEvents migration in internal/db for how existing
// data carried over.
package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
)

// Kind identifies what an event means and how to interpret its payload.
type Kind string

const (
	// KindIncidentCreated and KindReporterAdded are emitted once each, from
	// Service.Handle, mirroring incidents.created_at / incident_reporters —
	// purely for a complete export/transcript; nothing reads them back.
	KindIncidentCreated Kind = "incident_created"
	KindReporterAdded   Kind = "reporter_added"
	// KindRunStarted carries the seed messages a diagnostic run began from —
	// the anchor Conversation folds forward from (see its doc comment).
	KindRunStarted Kind = "run_started"
	// KindLLMRound carries one round's assistant message plus every tool
	// result it produced, appended once per round after tool calls are fully
	// processed — not before, which is what let the old per-round
	// SaveConversation call silently drop the final round's tool results.
	KindLLMRound Kind = "llm_round"
	// KindActionApplied backs the actions_log projection (see LogAction) and,
	// when tagged disruptive, LastDisruption.
	KindActionApplied Kind = "action_applied"
	// KindDiagnosisCompleted carries the full DiagnosticResult from
	// complete_diagnosis — a clean terminal marker for the transcript.
	KindDiagnosisCompleted Kind = "diagnosis_completed"
	// KindRunFinished marks a run's exit (resolved, escalated, error, or
	// superseded) and how many rounds it took.
	KindRunFinished Kind = "run_finished"
	// KindConversationImported is backfill-only: one per incident that had a
	// conversation_history row, carrying that row's raw messages verbatim
	// (see the migration). Never emitted live.
	KindConversationImported Kind = "conversation_imported"

	// KindStatusChanged marks a status (or autonomous-lock) transition —
	// purely a live-update signal for incidentEvents (see StatusChanged's
	// doc comment); no reader treats its payload as authoritative.
	KindStatusChanged Kind = "status_changed"

	// KindEscalationPreviewed is reserved for a later pass wiring up
	// escalation preview/approval, per-attempt verification checks, and
	// pending-outcome sweeps as events — see KindEscalationApproved,
	// KindVerificationChecked, and KindPendingOutcomeAdvanced below, all
	// reserved for the same reason. The live page already re-renders on
	// KindStatusChanged/KindActionApplied whenever any of these actually
	// change status or the action log, so none of the four are needed yet.
	KindEscalationPreviewed Kind = "escalation_previewed"
	// KindEscalationApproved is reserved; see KindEscalationPreviewed above.
	KindEscalationApproved Kind = "escalation_approved"
	// KindVerificationChecked is reserved; see KindEscalationPreviewed above.
	KindVerificationChecked Kind = "verification_checked"
	// KindPendingOutcomeAdvanced is reserved; see KindEscalationPreviewed above.
	KindPendingOutcomeAdvanced Kind = "pending_outcome_advanced"
)

// Journal owns the event log: appending (with atomic projection writes) and
// fanning out live updates to subscribers (e.g. an SSE handler).
type Journal struct {
	db *db.DB

	mu      sync.Mutex
	subs    map[string][]chan *db.Event
	allSubs []chan *db.Event
}

// New builds a Journal backed by database.
func New(database *db.DB) *Journal {
	return &Journal{db: database, subs: make(map[string][]chan *db.Event)}
}

// Append writes one event, applying its projection (if any) in the same
// transaction as the event insert, then notifies live subscribers for that
// incident. The lower-level primitive every Append* convenience method below
// funnels through.
func (j *Journal) Append(ctx context.Context, e *db.Event) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	var seq int64
	err := j.db.RunTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var txErr error
		seq, txErr = db.AppendEvent(ctx, tx, e)
		if txErr != nil {
			return txErr
		}
		return applyProjection(ctx, tx, e)
	})
	if err != nil {
		return err
	}
	e.Seq = seq
	j.notify(e)
	return nil
}

// applyProjection writes the read-optimized side effect (if any) a kind of
// event backs, inside the same transaction as its insert.
func applyProjection(ctx context.Context, tx *sql.Tx, e *db.Event) error {
	if e.Kind != string(KindActionApplied) {
		return nil
	}
	var p actionAppliedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return err
	}
	at := e.At
	return db.InsertActionLog(ctx, tx, &db.ActionLog{
		ID:          p.ID,
		IncidentID:  e.IncidentID,
		Action:      p.Action,
		Params:      p.Params,
		TriggeredBy: p.TriggeredBy,
		Status:      db.ActionStatus(p.Status),
		AppliedAt:   &at,
		Result:      p.Result,
		Error:       p.Error,
	})
}

// Since returns every event for incidentID after afterSeq, in order —
// afterSeq=0 for the full history. Used for the live SSE stream's
// reconnect/replay (afterSeq = the client's Last-Event-ID) and for reads
// that want the whole thing (the transcript page, the JSON export).
func (j *Journal) Since(ctx context.Context, incidentID string, afterSeq int64) ([]*db.Event, error) {
	return j.db.EventsSince(ctx, incidentID, afterSeq)
}

// Subscribe registers ch to receive every event appended for incidentID from
// this point on. The returned func unregisters it. Sends are non-blocking —
// a slow or wedged subscriber must never stall Append (which runs inline
// with the agent's own writes) — so a subscriber that can't keep up misses
// events; it recovers by calling Since with its last-seen seq, the same path
// a fresh reconnect uses. Push for latency, replay for correctness.
func (j *Journal) Subscribe(incidentID string) (<-chan *db.Event, func()) {
	ch := make(chan *db.Event, subscriberBufferSize)
	j.mu.Lock()
	j.subs[incidentID] = append(j.subs[incidentID], ch)
	j.mu.Unlock()

	unsubscribe := func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		subs := j.subs[incidentID]
		for i, c := range subs {
			if c == ch {
				j.subs[incidentID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		if len(j.subs[incidentID]) == 0 {
			delete(j.subs, incidentID)
		}
		close(ch)
	}
	return ch, unsubscribe
}

// SubscribeAll registers ch to receive every event appended for any
// incident — the dashboard index page's live-update signal, where a single
// per-incident Subscribe doesn't apply (a new incident starts with no
// subscriber for it yet, and the index cares about all of them at once).
// Same non-blocking-send/replay-via-Since contract as Subscribe.
func (j *Journal) SubscribeAll() (<-chan *db.Event, func()) {
	ch := make(chan *db.Event, subscriberBufferSize)
	j.mu.Lock()
	j.allSubs = append(j.allSubs, ch)
	j.mu.Unlock()

	unsubscribe := func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		for i, c := range j.allSubs {
			if c == ch {
				j.allSubs = append(j.allSubs[:i], j.allSubs[i+1:]...)
				break
			}
		}
		close(ch)
	}
	return ch, unsubscribe
}

// subscriberBufferSize is generous relative to how bursty a single incident's
// events actually get (a handful per LLM round) — the buffer existing at all
// is just to smooth out a slow single receive loop, not to absorb sustained
// backpressure; a subscriber that falls behind by more than this recovers via
// Since, not a bigger buffer.
const subscriberBufferSize = 32

func (j *Journal) notify(e *db.Event) {
	j.mu.Lock()
	subs := j.subs[e.IncidentID]
	allSubs := j.allSubs
	j.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
		}
	}
	for _, ch := range allSubs {
		select {
		case ch <- e:
		default:
		}
	}
}

// --- incident_created / reporter_added ---

type incidentCreatedPayload struct {
	Source     string `json:"source"`
	ReportedBy string `json:"reported_by"`
	What       string `json:"what"`
	Title      string `json:"title"`
}

// IncidentCreated appends the incident_created event for a newly-created
// incident.
func (j *Journal) IncidentCreated(ctx context.Context, inc *db.Incident) error {
	payload, err := json.Marshal(incidentCreatedPayload{
		Source: inc.Source, ReportedBy: inc.ReportedBy, What: inc.What, Title: inc.Title,
	})
	if err != nil {
		return err
	}
	return j.Append(ctx, &db.Event{
		IncidentID: inc.ID, At: inc.CreatedAt, Kind: string(KindIncidentCreated), Payload: payload,
	})
}

type reporterAddedPayload struct {
	Reporter      string `json:"reporter"`
	Source        string `json:"source"`
	DiscordUserID string `json:"discord_user_id,omitempty"`
}

// ReporterAdded appends the reporter_added event for a report collapsed into
// (or starting) an incident.
func (j *Journal) ReporterAdded(ctx context.Context, incidentID, reporter, source, discordUserID string) error {
	payload, err := json.Marshal(reporterAddedPayload{Reporter: reporter, Source: source, DiscordUserID: discordUserID})
	if err != nil {
		return err
	}
	return j.Append(ctx, &db.Event{IncidentID: incidentID, Kind: string(KindReporterAdded), Payload: payload})
}

// --- action_applied / LastDisruption ---

type actionAppliedPayload struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	Params      any    `json:"params,omitempty"`
	TriggeredBy string `json:"triggered_by"`
	Status      string `json:"status"`
	Result      string `json:"result,omitempty"`
	Error       string `json:"error,omitempty"`
	// Disruptive marks a service-wide action (restart, scan, cleanup, sweep —
	// see isDisruptiveAction in internal/agent/tools.go, the source of truth
	// this is tagged from) rather than an item-scoped one. LastDisruption
	// scans for the most recent event with this set, replacing the old
	// last_disruption single-row table.
	Disruptive bool `json:"disruptive"`
}

// LogAction appends an action_applied event and writes its actions_log
// projection atomically — the shared replacement for the old
// Dispatcher.logAction (agent-triggered, always status=applied by the time it
// logs) and Service.logEscalation (owner-triggered, may log status=failed
// with a Result/Error already known). a.AppliedAt, if nil, defaults to now;
// a.ID, if empty, is generated here (not left to the actions_log projection
// to generate its own — the event and its projection must share one ID so an
// export or a future cross-reference can tie them together).
func (j *Journal) LogAction(ctx context.Context, a *db.ActionLog, disruptive bool) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	at := time.Now()
	if a.AppliedAt != nil {
		at = *a.AppliedAt
	}
	payload, err := json.Marshal(actionAppliedPayload{
		ID: a.ID, Action: a.Action, Params: a.Params, TriggeredBy: a.TriggeredBy,
		Status: string(a.Status), Result: a.Result, Error: a.Error, Disruptive: disruptive,
	})
	if err != nil {
		return err
	}
	return j.Append(ctx, &db.Event{IncidentID: a.IncidentID, At: at, Kind: string(KindActionApplied), Payload: payload})
}

// Disruption is the most recent service-wide disruptive action found in the
// log — the replacement for the old last_disruption table.
type Disruption struct {
	Action     string
	IncidentID string
	At         time.Time
}

// lastDisruptionScanLimit bounds LastDisruption's scan over the most recent
// action_applied events. Not every action is disruptive, so this can't be a
// LIMIT 1 SQL query without db knowing what "disruptive" means (a domain
// concept that belongs here, not in storage) — 200 is generous enough that,
// given this app's actual traffic (a handful of incidents a day), any
// disruption still within it is also comfortably within
// Agent.disruptionNote's own staleness window, and any disruption old enough
// to fall outside it is stale enough that missing it changes nothing.
const lastDisruptionScanLimit = 200

// LastDisruption returns the most recent action tagged disruptive by LogAction,
// or db.ErrNotFound if none has been recorded (within the scan window above).
func (j *Journal) LastDisruption(ctx context.Context) (*Disruption, error) {
	events, err := j.db.EventsByKind(ctx, string(KindActionApplied), lastDisruptionScanLimit)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		var p actionAppliedPayload
		if unmarshalErr := json.Unmarshal(e.Payload, &p); unmarshalErr != nil || !p.Disruptive {
			continue
		}
		return &Disruption{Action: p.Action, IncidentID: e.IncidentID, At: e.At}, nil
	}
	return nil, db.ErrNotFound
}

// --- run_started / llm_round / Conversation ---

type runStartedPayload struct {
	Seed []openai.ChatCompletionMessage `json:"seed"`
}

// RunStarted appends the seed messages a diagnostic run begins from — either
// the fresh [system, user] pair or a resumed BuildSummarySeed result.
// Conversation folds forward from the most recent one of these.
func (j *Journal) RunStarted(ctx context.Context, incidentID string, seed []openai.ChatCompletionMessage) error {
	payload, err := json.Marshal(runStartedPayload{Seed: seed})
	if err != nil {
		return err
	}
	return j.Append(ctx, &db.Event{IncidentID: incidentID, Kind: string(KindRunStarted), Payload: payload})
}

type llmRoundPayload struct {
	Round            int                            `json:"round"`
	AssistantMessage openai.ChatCompletionMessage   `json:"assistant_message"`
	ToolMessages     []openai.ChatCompletionMessage `json:"tool_messages,omitempty"`
}

// LLMRound appends one round's assistant message plus every tool result it
// produced. Called once per round after tool calls are fully processed, so —
// unlike the old per-round SaveConversation, which ran before tool results
// were appended and was never called again after complete_diagnosis returned
// — the final round's tool results are never lost.
func (j *Journal) LLMRound(
	ctx context.Context, incidentID string, round int,
	assistant openai.ChatCompletionMessage, toolMessages []openai.ChatCompletionMessage,
) error {
	payload, err := json.Marshal(llmRoundPayload{Round: round, AssistantMessage: assistant, ToolMessages: toolMessages})
	if err != nil {
		return err
	}
	return j.Append(ctx, &db.Event{IncidentID: incidentID, Kind: string(KindLLMRound), Payload: payload})
}

type conversationImportedPayload struct {
	Messages []openai.ChatCompletionMessage `json:"messages"`
}

// Conversation reconstructs the most recent run's conversation: the seed from
// the latest run_started event, followed by every llm_round event's messages
// after it, in order. A legacy incident with only a backfilled
// conversation_imported event (no run_started/llm_round events, since those
// postdate this package) returns that blob's messages verbatim instead.
// Returns db.ErrNotFound if nothing has been recorded — mirroring the old
// LoadConversation's contract, since this has the same one caller
// (Service.buildReinvestigateSeed) expecting it.
func (j *Journal) Conversation(ctx context.Context, incidentID string) ([]openai.ChatCompletionMessage, error) {
	events, err := j.db.EventsSince(ctx, incidentID, 0)
	if err != nil {
		return nil, err
	}
	var out []openai.ChatCompletionMessage
	for _, e := range events {
		switch e.Kind {
		case string(KindRunStarted):
			var p runStartedPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				out = p.Seed // reset: only the latest run's conversation matters
			}
		case string(KindLLMRound):
			var p llmRoundPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				out = append(out, p.AssistantMessage)
				out = append(out, p.ToolMessages...)
			}
		case string(KindConversationImported):
			var p conversationImportedPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				out = p.Messages // also a reset, same "latest wins" semantics
			}
		}
	}
	if len(out) == 0 {
		return nil, db.ErrNotFound
	}
	return out, nil
}

// --- status_changed ---

type statusChangedPayload struct {
	Status string `json:"status"`
}

// StatusChanged appends a status_changed event to notify live subscribers
// that the incident's status (or, loosely, its autonomous-lock flag — see
// Service.Unlock) moved. The transition itself is already durably written by
// the caller's own db.TransitionStatus/UpdateIncidentStatus/
// SetAutonomousLocked call; this only makes that visible on an open SSE
// connection instead of requiring a manual reload. Status is carried for
// completeness in the export/transcript — incidentEvents always re-reads
// current state from the DB rather than trusting this payload for anything
// that matters.
func (j *Journal) StatusChanged(ctx context.Context, incidentID, status string) error {
	payload, err := json.Marshal(statusChangedPayload{Status: status})
	if err != nil {
		return err
	}
	return j.Append(ctx, &db.Event{IncidentID: incidentID, Kind: string(KindStatusChanged), Payload: payload})
}

// --- diagnosis_completed / run_finished ---

// DiagnosisCompleted appends the full complete_diagnosis result as a clean
// terminal marker for the transcript. result is stored as-is (already
// JSON-marshalable — agent.DiagnosticResult) rather than a typed field, since
// journal cannot import internal/agent without an import cycle (agent imports
// journal to emit these events in the first place).
func (j *Journal) DiagnosisCompleted(ctx context.Context, incidentID string, result any) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return j.Append(ctx, &db.Event{IncidentID: incidentID, Kind: string(KindDiagnosisCompleted), Payload: payload})
}

type runFinishedPayload struct {
	Rounds int    `json:"rounds"`
	Error  string `json:"error,omitempty"`
}

// RunFinished marks a run's exit, successful or not, and how many rounds it
// took.
func (j *Journal) RunFinished(ctx context.Context, incidentID string, rounds int, runErr error) error {
	p := runFinishedPayload{Rounds: rounds}
	if runErr != nil {
		p.Error = runErr.Error()
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return j.Append(ctx, &db.Event{IncidentID: incidentID, Kind: string(KindRunFinished), Payload: payload})
}

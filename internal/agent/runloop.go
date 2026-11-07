package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
)

const journalWriteTimeout = 5 * time.Second

var errRunNotExited = errors.New("the run ended without an exit")

type runLoop struct {
	a         *Agent
	inc       *db.Incident
	snap      runSnapshot
	preFix    *FixSignature
	messages  []openai.ChatCompletionMessage
	assistant openai.ChatCompletionMessage
	toolStart int
	seen      map[string]int
	actions   int
	result    *DiagnosticResult
	finding   *DiagnosticResult
	err       error
}

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), journalWriteTimeout)
}

func (a *Agent) runExit(ctx context.Context, inc *db.Incident, seed []openai.ChatCompletionMessage) RunExit {
	l := &runLoop{a: a, inc: inc, snap: runSnapshot{State: RunClaiming}, seen: make(map[string]int)}
	return l.run(ctx, seed)
}

func (l *runLoop) run(ctx context.Context, seed []openai.ChatCompletionMessage) RunExit {
	l.claim(ctx, seed)
	for !RunMeta(l.snap.State).Terminal {
		if l.snap.State == RunInBatch {
			l.batch(ctx)
			continue
		}
		l.reply(ctx)
	}
	return l.exit()
}

func (l *runLoop) reply(ctx context.Context) {
	round := l.snap.Round
	resp, err := l.a.llmCall(ctx, openai.ChatCompletionRequest{
		Model:    l.a.model,
		Messages: l.messages,
		Tools:    toolDefs(),
	})
	if err != nil {
		l.fire(ctx, runEvent{kind: evLLMFailed, err: fmt.Errorf("llm round %d: %w", round, err)})
		return
	}
	msg := resp.Choices[0].Message
	l.messages = append(l.messages, msg)
	l.assistant = msg
	l.toolStart = len(l.messages)
	l.a.logTurn(ctx, l.inc.ID, round, msg)
	if len(msg.ToolCalls) == 0 {
		l.fire(ctx, runEvent{kind: evTextOnly})
		return
	}
	l.fire(ctx, runEvent{kind: evToolCalls})
}

func (l *runLoop) batch(ctx context.Context) {
	ev, result := l.a.processToolCalls(
		ctx, l.inc, l.preFix, l.assistant.ToolCalls, l.seen, &l.actions, &l.messages,
	)
	l.result = result
	l.fire(ctx, ev)
}

func (l *runLoop) fire(ctx context.Context, ev runEvent) {
	ev.cancelled = ctx.Err() != nil
	before := l.snap
	next, effs, err := nextRun(before, ev)
	if err == nil {
		err = checkRun(next)
	}
	if err != nil {
		l.a.log.ErrorContext(ctx, "agent run machine refused a step",
			"incident", l.inc.ID, "state", string(before.State), "event", string(ev.kind), "error", err)
		ev.err = errors.Join(ev.err, err)
		next, effs = failRun(before, ev)
	}
	if ev.err != nil {
		l.err = ev.err
	}
	l.snap = next
	for _, eff := range effs {
		l.effect(ctx, eff, before)
	}
}

func (l *runLoop) finish(ctx context.Context) {
	state := l.snap.State
	meta := RunMeta(state)
	if state == RunSuperseded && l.err == nil {
		l.err = ctx.Err()
	}
	l.finding = nil
	if state == RunConcluded {
		l.finding = l.result
	}
	var finding any
	if meta.WritesFinding {
		l.finding = manualFinding(state, l.err)
		finding = l.finding
	}
	runErr := l.err
	if state == RunExhausted && runErr == nil {
		runErr = errLoopExhausted
	}
	wctx, cancel := detached(ctx)
	defer cancel()
	if l.snap.Started && l.a.journal != nil {
		if err := l.a.journal.RunExit(wctx, l.inc.ID, finding, l.snap.Replies, string(state), runErr); err != nil {
			l.a.log.WarnContext(ctx, "record run_finished", "incident", l.inc.ID, "error", err)
		}
		return
	}
	if meta.WritesFinding {
		if err := l.a.db.SetIncidentFinding(wctx, l.inc.ID, finding, finding); err != nil {
			l.a.log.WarnContext(ctx, "record run exit finding", "incident", l.inc.ID, "error", err)
		}
	}
}

func (l *runLoop) exit() RunExit {
	return RunExit{
		State:        l.snap.State,
		Rounds:       l.snap.Replies,
		Finding:      l.finding,
		Conversation: l.messages,
		Err:          l.err,
	}
}

func manualFinding(s RunState, err error) *DiagnosticResult {
	root := RunMeta(s).Reason
	if err != nil && (s == RunLLMFailed || s == RunBatchFailed || s == RunClaimFailed) {
		root += ": " + err.Error()
	}
	reason := ""
	if s == RunBudgetTripped {
		reason = "agent applied 3 actions without confirming fix"
	}
	if s == RunExhausted {
		reason = "agent did not reach a conclusion within iteration limit"
	}
	return &DiagnosticResult{
		RootCause:        root,
		Confidence:       "low",
		PrimaryAction:    EscalateManualInvestigation,
		PrimaryReason:    reason,
		EscalateAction:   EscalateManualInvestigation,
		RequiresApproval: true,
	}
}

func (e RunExit) legacy() (*DiagnosticResult, []openai.ChatCompletionMessage, error) {
	switch e.State {
	case RunConcluded, RunBudgetTripped, RunStoppedNoTools, RunExhausted:
		return e.Finding, e.Conversation, nil
	case RunLLMFailed, RunBatchFailed, RunClaimFailed, RunNotClaimed:
		return nil, e.Conversation, e.Err
	case RunSuperseded:
		if e.Err == nil {
			return nil, e.Conversation, context.Canceled
		}
		return nil, e.Conversation, e.Err
	case RunClaiming, RunAwaitingReply, RunInBatch:
	}
	return nil, e.Conversation, errRunNotExited
}

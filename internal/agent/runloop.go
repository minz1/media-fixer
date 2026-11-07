package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

const journalWriteTimeout = 5 * time.Second

type runLoop struct {
	a         *Agent
	inc       *db.Incident
	snap      runSnapshot
	preFix    *FixSignature
	messages  []openai.ChatCompletionMessage
	assistant openai.ChatCompletionMessage
	toolStart int
	seen      map[string]int
	gate      Gate
	journal   *db.Event
	result    *DiagnosticResult
	finding   *DiagnosticResult
	err       error
}

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), journalWriteTimeout)
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
	if kind, stop := gateStop(l.gate.Live(ctx)); stop {
		l.fire(ctx, runEvent{kind: kind})
		return
	}
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
		ctx, l.inc, l.preFix, l.assistant.ToolCalls, l.seen, l.gate, &l.messages,
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
	if meta.WritesFinding {
		l.finding = manualFinding(state, l.err)
	}
	runErr := l.err
	if state == RunExhausted && runErr == nil {
		runErr = errLoopExhausted
	}
	if l.snap.Started {
		l.journal = journal.RunFinishedEvent(l.inc.ID, l.snap.Replies, string(state), runErr)
	}
}

func (l *runLoop) exit() RunExit {
	return RunExit{
		State:        l.snap.State,
		Rounds:       l.snap.Replies,
		Finding:      l.finding,
		Conversation: l.messages,
		Err:          l.err,
		Started:      l.snap.Started,
		Journal:      l.journal,
	}
}

func manualFinding(s RunState, err error) *DiagnosticResult {
	root := RunMeta(s).Reason
	if err != nil && (s == RunLLMFailed || s == RunBatchFailed || s == RunClaimFailed) {
		root += ": " + err.Error()
	}
	reason := ""
	if s == RunBudgetTripped {
		reason = "agent tried 3 autonomous actions without confirming a fix"
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

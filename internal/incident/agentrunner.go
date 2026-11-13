package incident

import (
	"context"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
)

// AgentRunner is the subset of *agent.Agent the incident service depends on.
// Extracting it as an interface lets tests substitute a fake that can block a run
// on demand, so the single-flight/cancellation behaviour is exercised by an actual
// test rather than verified by inspection. *agent.Agent satisfies this interface.
type AgentRunner interface {
	// Run executes the diagnostic loop for an incident, optionally seeded with a
	// prior conversation, and returns the result plus the full conversation.
	Run(ctx context.Context, inc *db.Incident, seed []openai.ChatCompletionMessage, gate agent.Gate) agent.RunExit
	// VerifyResolved reports whether a previously-applied fix now looks
	// resolved, by comparing the item's current state (itemID, title — title
	// is used for a best-effort *arr file-existence check) against pre (the
	// state captured before the fix ran; nil disables the "is this a strict
	// improvement" check).
	VerifyResolved(ctx context.Context, itemID, title string, pre *agent.FixSignature) bool
	VerifySubtitle(ctx context.Context, itemID, lang string) bool
	SwapItem(ctx context.Context, target lifecycle.SwapTarget) (itemID string, landed bool, note string)
	// ScanRunning reports whether a Jellyfin library scan is currently in progress.
	ScanRunning(ctx context.Context) bool
	// BuildSummarySeed constructs seed messages for a resumed run from a summary.
	BuildSummarySeed(ctx context.Context, inc *db.Incident, summary string) []openai.ChatCompletionMessage
	// PlanEscalation previews a diagnostic result's recommended escalation
	// without making any changes.
	PlanEscalation(ctx context.Context, result *agent.DiagnosticResult, what string) (any, error)
	OperationFor(subject operation.Subject, result *agent.DiagnosticResult, planJSON []byte) (operation.Spec, error)
	// CheckPendingOutcome polls live state for one pending arr_search_missing
	// outcome — see Service.downloadCheck, the sweeper that drives it.
	CheckPendingOutcome(ctx context.Context, po *db.PendingOutcome) (*agent.PendingOutcomeObservation, error)
}

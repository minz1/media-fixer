package incident

import (
	"context"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
)

// AgentRunner is the subset of *agent.Agent the incident service depends on.
// Extracting it as an interface lets tests substitute a fake that can block a run
// on demand, so the single-flight/cancellation behaviour is exercised by an actual
// test rather than verified by inspection. *agent.Agent satisfies this interface.
type AgentRunner interface {
	// Run executes the diagnostic loop for an incident, optionally seeded with a
	// prior conversation, and returns the result plus the full conversation.
	Run(ctx context.Context, inc *db.Incident, seed []openai.ChatCompletionMessage) (
		*agent.DiagnosticResult, []openai.ChatCompletionMessage, error)
	// VerifyResolved reports whether a previously-applied fix now looks resolved.
	VerifyResolved(ctx context.Context, itemID string) bool
	// ScanRunning reports whether a Jellyfin library scan is currently in progress.
	ScanRunning(ctx context.Context) bool
	// BuildSummarySeed constructs seed messages for a resumed run from a summary.
	BuildSummarySeed(inc *db.Incident, summary string) []openai.ChatCompletionMessage
}

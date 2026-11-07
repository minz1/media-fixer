package lifecycle

type State string

const (
	Open             State = "open"
	Reopened         State = "reopened"
	Investigating    State = "investigating"
	Verifying        State = "verifying"
	AwaitingDownload State = "awaiting_download"
	ManualTestNeeded State = "manual_test_needed"
	ExecutingPlan    State = "executing_plan"
	AgentFixed       State = "agent_fixed"
	Blocked          State = "blocked"
	Resolved         State = "resolved"
)

func States() []State {
	return []State{
		Open, Reopened, Investigating, Verifying, AwaitingDownload,
		ManualTestNeeded, ExecutingPlan, AgentFixed, Blocked, Resolved,
	}
}

type Reason string

const (
	ReasonNone            Reason = ""
	ReasonNeedsOwner      Reason = "needs_owner"
	ReasonNoRelease       Reason = "no_release"
	ReasonDownloadProblem Reason = "download_problem"
	ReasonNotVerified     Reason = "not_verified"
	ReasonBudgetSpent     Reason = "budget_spent"
	ReasonAgentError      Reason = "agent_error"
	ReasonPlanFailed      Reason = "plan_failed"
	ReasonPlanPartial     Reason = "plan_partial"
	ReasonPlanInterrupted Reason = "plan_interrupted"
)

func Reasons() []Reason {
	return []Reason{
		ReasonNeedsOwner, ReasonNoRelease, ReasonDownloadProblem, ReasonNotVerified, ReasonBudgetSpent,
		ReasonAgentError, ReasonPlanFailed, ReasonPlanPartial, ReasonPlanInterrupted,
	}
}

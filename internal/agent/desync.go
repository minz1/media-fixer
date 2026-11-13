package agent

import (
	"encoding/json"
	"regexp"

	openai "github.com/sashabaranov/go-openai"
)

const desyncReason = "Subtitles reported out of sync; resync is the owner's call. "

var desyncPattern = regexp.MustCompile(
	`(?i)(out[ -]of[ -]sync|not in sync|desync|\b(subs?|subtitles?)\b.{0,40}\b(late|early|ahead|behind|delay(ed)?|lag(s|ging)?|timing)\b)`,
)

func IsDesyncReport(text string) bool { return desyncPattern.MatchString(text) }

func lastAlignment(messages []openai.ChatCompletionMessage) *Alignment {
	calls := map[string]bool{}
	var last *Alignment
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = calls[tc.ID] || tc.Function.Name == toolStreamAlignment
		}
		if m.Role != openai.ChatMessageRoleTool || !calls[m.ToolCallID] {
			continue
		}
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(m.Content), &probe) != nil || probe[keyError] != nil {
			continue
		}
		var al Alignment
		if json.Unmarshal([]byte(m.Content), &al) == nil {
			last = &al
		}
	}
	return last
}

func escalateDesync(result *DiagnosticResult, streams *Alignment) {
	result.SubtitleDesync = true
	result.SubtitleStreams = streams
	result.RequiresApproval = true
	result.EscalateAction = EscalateManualInvestigation
	result.EscalateParams = nil
	result.VerifyAfterSeconds = 0
	result.PrimaryReason = desyncReason + result.PrimaryReason
	result.Outcome = DiagnosisOutcome{Kind: DiagnosisManual, Reason: result.PrimaryReason}
}

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
)

// transcriptToolCall pairs one tool call with its result, for rendering a
// single collapsible block instead of two separate raw messages — a tool
// response message only carries a ToolCallID and Content; the function name
// and arguments live on the preceding assistant message's ToolCalls, joined
// here by ID.
type transcriptToolCall struct {
	Name      string
	Arguments string
	Result    string
	ArgsLine  string
	ResultTop string
}

// transcriptTurn is one renderable unit of a diagnostic conversation: the
// system prompt, the initial user report, or one LLM round's assistant
// message plus every tool call it made. Round is 1-based and only set for
// assistant turns — the same numbering agent.Agent's own round loop uses,
// recovered here from message order rather than carried through as data,
// since journal.Conversation already guarantees exactly one assistant
// message per round.
type transcriptTurn struct {
	Role    string
	Round   int
	Content string
	Tools   []transcriptToolCall
	Final   bool
}

// buildTranscriptTurns groups a flat conversation (as journal.Conversation
// reconstructs it: seed messages followed by each round's assistant+tool
// messages) into turns for display. Shared by the incident page's condensed
// view and the full thought-process page — the two differ only in which
// turns the template shows and how much of each it shows, not in how the
// data is built.
func buildTranscriptTurns(messages []openai.ChatCompletionMessage) []transcriptTurn {
	callByID := make(map[string]openai.ToolCall)
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			callByID[tc.ID] = tc
		}
	}

	var turns []transcriptTurn
	round := 0
	for _, m := range messages {
		if m.Role == openai.ChatMessageRoleTool {
			if len(turns) == 0 {
				continue // defensive: a tool message can't precede any assistant turn
			}
			tc := callByID[m.ToolCallID]
			last := &turns[len(turns)-1]
			last.Tools = append(last.Tools, transcriptToolCall{
				Name: tc.Function.Name, Arguments: tc.Function.Arguments, Result: m.Content,
				ArgsLine: summarizeArgs(tc.Function.Arguments), ResultTop: firstLine(m.Content, resultTopMax),
			})
			continue
		}
		if m.Role == openai.ChatMessageRoleAssistant {
			round++
		}
		turns = append(turns, transcriptTurn{Role: m.Role, Round: round, Content: m.Content})
	}
	if n := len(turns); n > 0 && turns[n-1].Role == openai.ChatMessageRoleAssistant && len(turns[n-1].Tools) == 0 {
		turns[n-1].Final = true
	}
	return turns
}

const (
	argsLineMax    = 80
	resultTopMax   = 100
	condensedSteps = 3
	ellipsis       = "…"
)

func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + ellipsis
}

// summarizeArgs renders a tool call's JSON arguments as one "key=value" line;
// anything that is not a JSON object is shown raw.
func summarizeArgs(raw string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return truncate(strings.Join(strings.Fields(raw), " "), argsLineMax)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if str, ok := obj[k].(string); ok {
			parts = append(parts, k+"="+str)
			continue
		}
		b, _ := json.Marshal(obj[k])
		parts = append(parts, k+"="+string(b))
	}
	return truncate(strings.Join(parts, " "), argsLineMax)
}

func firstLine(s string, limit int) string {
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(s)) == nil {
		return truncate(compact.String(), limit)
	}
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return truncate(l, limit)
		}
	}
	return ""
}

// condenseTurns drops the system prompt and, unless full, keeps only the last
// condensedSteps assistant steps (the report goes too once steps are hidden),
// returning how many earlier steps were hidden.
func condenseTurns(turns []transcriptTurn, full bool) ([]transcriptTurn, int) {
	if full {
		return turns, 0
	}
	var out []transcriptTurn
	steps := 0
	for _, t := range turns {
		if t.Role == openai.ChatMessageRoleSystem {
			continue
		}
		if t.Role == openai.ChatMessageRoleAssistant {
			steps++
		}
		out = append(out, t)
	}
	hidden := steps - condensedSteps
	if hidden <= 0 {
		return out, 0
	}
	for i, t := range out {
		if t.Role == openai.ChatMessageRoleAssistant && t.Round == hidden+1 {
			return out[i:], hidden
		}
	}
	return out, 0
}

// incidentLive reports whether the agent may still add steps to the incident.
func incidentLive(s db.IncidentStatus) bool {
	return s == db.StatusOpen || s == db.StatusInvestigating || s == db.StatusReopened
}

// incidentTranscript renders the full, untruncated thought-process page for
// an incident: the system prompt, any resumed-run seed, and every round in
// full — everything the condensed view on the incident page deliberately
// hides to stay readable. Live via the same hx-sse:connect stream as the
// incident page itself.
func (s *Server) incidentTranscript(w http.ResponseWriter, r *http.Request) {
	id := incidentIDParam(r)
	data, err := s.buildIncidentPageData(r.Context(), id, true)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "transcript_page", data)
}

// diagnosticResultOrNil adapts agent.ParseDiagnosticResult's (value, ok) pair
// to a plain nilable pointer, which templates can test with a plain
// {{if .Finding}} instead of needing a second boolean field alongside it.
func diagnosticResultOrNil(finding any) *agent.DiagnosticResult {
	result, ok := agent.ParseDiagnosticResult(finding)
	if !ok {
		return nil
	}
	return result
}

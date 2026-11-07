package operation

import (
	"fmt"
	"strings"
)

func Applied(op Op) bool {
	for _, r := range op.Results {
		if r.State == ResultStarted || r.State == ResultDone || r.State == ResultUnknown {
			return true
		}
	}
	return false
}

func Resumable(op Op) bool { return op.Status == FailedAtStep && Applied(op) }

func Remaining(op Op) []int {
	var out []int
	for j := op.Index; j >= 0 && j < len(op.Results); j++ {
		if op.Results[j].State != ResultDone {
			out = append(out, j)
		}
	}
	return out
}

func Line(op Op) string {
	if len(op.Results) == 0 {
		return ""
	}
	if !Applied(op) {
		return nothingLine(op)
	}
	if op.Kind == KindReplace {
		return fmt.Sprintf("%s: %s.", header(op), replaceBody(op))
	}
	return smallBody(op) + "."
}

func position(op Op) string { return fmt.Sprintf("step %d of %d", op.Index+1, len(op.Results)) }

func nothingLine(op Op) string {
	switch {
	case op.Status == Running:
		return fmt.Sprintf("About to start %s.", position(op))
	case op.Cause == CauseRestart:
		return fmt.Sprintf("Interrupted by a restart before %s; nothing changed.", position(op))
	case op.Cause == CauseStore:
		return fmt.Sprintf("Stopped by a database error before %s; nothing changed.", position(op))
	}
	return fmt.Sprintf("Stopped at %s; nothing changed.", position(op))
}

func header(op Op) string {
	switch {
	case op.Status == Done:
		return "Finished"
	case op.Status == Running:
		return "Running " + position(op)
	case op.Cause == CauseRestart:
		return "Interrupted by a restart at " + position(op)
	case op.Cause == CauseStore:
		return "Stopped by a database error at " + position(op)
	}
	return "Stopped at " + position(op)
}

type tally struct{ total, done, maybe int }

func tallyOf(op Op, kind StepKind) tally {
	var t tally
	for j, st := range op.Steps {
		if st.Kind != kind {
			continue
		}
		t.total++
		switch op.Results[j].State {
		case ResultDone:
			t.done++
		case ResultStarted, ResultUnknown:
			t.maybe++
		case ResultPending, ResultFailed:
		}
	}
	return t
}

func (t tally) part(verb, noun, participle string) string {
	s := fmt.Sprintf("%s %d of %d%s", verb, t.done, t.total, noun)
	if t.maybe > 0 {
		s += fmt.Sprintf(", %d may be %s", t.maybe, participle)
	}
	return s
}

func replaceBody(op Op) string {
	var parts []string
	if t := tallyOf(op, StepBlocklist); t.total > 0 {
		parts = append(parts, t.part("blocklisted", "", "blocklisted"))
	}
	if t := tallyOf(op, StepDeleteFile); t.total > 0 {
		noun := " files"
		if t.total == 1 {
			noun = " file"
		}
		parts = append(parts, t.part("deleted", noun, "deleted"))
	}
	parts = append(parts, sendPart(op.Results[len(op.Results)-1].State, "search"))
	return strings.Join(parts, ", ")
}

func sendPart(s StepState, what string) string {
	switch s {
	case ResultDone:
		return what + " sent"
	case ResultStarted, ResultUnknown:
		return fmt.Sprintf("the %s may have been sent", what)
	case ResultPending, ResultFailed:
	}
	return what + " not sent"
}

func smallBody(op Op) string {
	var first string
	if op.Kind == KindTorrentReadd {
		first = lead(
			op.Results[0].State,
			"Deleted the torrent",
			"The torrent may be deleted",
			"The torrent is not deleted",
		)
		return first + "; " + sendPart(op.Results[1].State, "re-add")
	}
	first = lead(op.Results[0].State, "Removed", "The removal may have landed", "Not removed")
	return first + "; " + sendPart(op.Results[1].State, "search")
}

func lead(s StepState, done, maybe, none string) string {
	switch s {
	case ResultDone:
		return done
	case ResultStarted, ResultUnknown:
		return maybe
	case ResultPending, ResultFailed:
	}
	return none
}

type phrase struct {
	kind  StepKind
	count int
	cmd   string
}

func NextLine(op Op) string {
	if !Resumable(op) {
		return ""
	}
	var ps []phrase
	for _, j := range Remaining(op) {
		st := op.Steps[j]
		if n := len(ps); n > 0 && ps[n-1].kind == st.Kind {
			ps[n-1].count++
			continue
		}
		ps = append(ps, phrase{kind: st.Kind, count: 1, cmd: st.Command})
	}
	words := make([]string, len(ps))
	for i, p := range ps {
		words[i] = p.words()
	}
	return "Resume: " + strings.Join(words, ", ") + "."
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (p phrase) words() string {
	switch p.kind {
	case StepBlocklist:
		return "blocklist " + plural(p.count, "grab", "grabs")
	case StepDeleteFile:
		return "delete " + plural(p.count, "file", "files")
	case StepSearch:
		return "send " + p.cmd
	case StepTorrentDelete:
		return "delete the torrent"
	case StepTorrentAdd:
		return "re-add it"
	case StepQueueRemove:
		return "remove the queue item"
	}
	return string(p.kind)
}

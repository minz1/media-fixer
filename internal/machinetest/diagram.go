package machinetest

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

const mermaidEnd = "[*]"

func stateID[S any](s S) string {
	if fmt.Sprint(s) == "" {
		return mermaidEnd
	}
	return strings.Map(func(r rune) rune {
		if r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, fmt.Sprint(s))
}

func mermaidEdges[S, K comparable, Snap, Ev any](m Machine[S, K, Snap, Ev]) map[string]bool {
	edges := map[string]bool{}
	if id := stateID(m.State(m.Initial)); id != mermaidEnd {
		edges[mermaidEnd+" --> "+id] = true
	}
	for _, s := range m.States {
		if m.Terminal(s) && stateID(s) != mermaidEnd {
			edges[stateID(s)+" --> "+mermaidEnd] = true
		}
		for _, snap := range m.Samples(s) {
			for _, k := range m.Kinds {
				for _, ev := range m.Events(k) {
					if next, err := m.Next(snap, ev); err == nil {
						edges[fmt.Sprintf("%s --> %s: %v", stateID(s), stateID(m.State(next)), k)] = true
					}
				}
			}
		}
	}
	return edges
}

func Mermaid[S, K comparable, Snap, Ev any](m Machine[S, K, Snap, Ev]) string {
	var lines []string
	for e := range mermaidEdges(m) {
		lines = append(lines, e)
	}
	slices.SortFunc(lines, func(a, b string) int {
		if c := strings.Compare(mermaidRank(a), mermaidRank(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return "```mermaid\nstateDiagram-v2\n    " + strings.Join(lines, "\n    ") + "\n```\n"
}

func mermaidRank(edge string) string {
	if strings.HasPrefix(edge, mermaidEnd) {
		return "0"
	}
	return "1"
}

func GoldenMermaid[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev], path string) {
	t.Helper()
	got := Mermaid(m)
	if os.Getenv("MACHINETEST_UPDATE") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Errorf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil || string(want) != got {
		t.Errorf("%s is out of date (err %v); regenerate with MACHINETEST_UPDATE=1 go test ./...", path, err)
	}
}

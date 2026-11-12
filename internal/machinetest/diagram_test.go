package machinetest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/machinetest"
)

const doorDiagram = "```mermaid\nstateDiagram-v2\n    [*] --> closed\n    closed --> gone: remove\n    closed --> locked: lock\n    closed --> open: push\n    gone --> [*]\n    locked --> closed: unlock\n    open --> closed: pull\n```\n"

func TestMermaidTwoStateMachine(t *testing.T) {
	t.Parallel()
	if got := machinetest.Mermaid(goodDoor()); got != doorDiagram {
		t.Errorf("diagram:\n%s\nwant:\n%s", got, doorDiagram)
	}
}

func TestMermaidDeterministic(t *testing.T) {
	t.Parallel()
	first := machinetest.Mermaid(goodDoor())
	if first != machinetest.Mermaid(goodDoor()) {
		t.Error("two calls gave different diagrams")
	}
}

func TestMermaidOmitsRefusals(t *testing.T) {
	t.Parallel()
	got := machinetest.Mermaid(goodDoor())
	for _, bad := range []string{"open --> locked", "locked --> open", "closed --> closed"} {
		if strings.Contains(got, bad) {
			t.Errorf("refused pair drawn: %s", bad)
		}
	}
}

func TestGoldenMermaid_DiffNamesRegenerate(t *testing.T) {
	t.Setenv("MACHINETEST_UPDATE", "")
	path := filepath.Join(t.TempDir(), "door.md")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	machinetest.GoldenMermaid(r, goodDoor(), path)
	if !r.has("MACHINETEST_UPDATE=1") {
		t.Errorf("errors %v do not name the regenerate command", r.errs)
	}
}

func TestGoldenMermaid_UpdateWrites(t *testing.T) {
	t.Setenv("MACHINETEST_UPDATE", "1")
	path := filepath.Join(t.TempDir(), "door.md")
	r := &recorder{}
	machinetest.GoldenMermaid(r, goodDoor(), path)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != doorDiagram || len(r.errs) != 0 {
		t.Errorf("read %q, err %v, errs %v", got, err, r.errs)
	}
}

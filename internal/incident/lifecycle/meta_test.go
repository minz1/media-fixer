package lifecycle_test

import (
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func TestMeta_EveryStateHasMeta(t *testing.T) {
	t.Parallel()
	if n := len(lifecycle.States()); n != 10 {
		t.Fatalf("States() has %d states, want 10", n)
	}
	for _, s := range lifecycle.States() {
		m := lifecycle.Meta(s)
		if m.Label == "" || m.Line == "" || m.Tone == "" || m.Driver == "" {
			t.Errorf("state %s has incomplete meta %+v", s, m)
		}
		if m.Active && m.Settled {
			t.Errorf("state %s is both active and settled", s)
		}
		if m.Terminal && !m.Settled {
			t.Errorf("state %s is terminal but not settled", s)
		}
		if m.Live && !m.Active {
			t.Errorf("state %s is live but not active", s)
		}
	}
	for _, r := range lifecycle.Reasons() {
		if r == "" {
			t.Error("empty reason in Reasons()")
		}
	}
}

func TestStatesWhere_ActiveIncludesExecutingPlan(t *testing.T) {
	t.Parallel()
	active := lifecycle.StatesWhere(func(m lifecycle.StateMeta) bool { return m.Active })
	want := []lifecycle.State{
		lifecycle.Open, lifecycle.Reopened, lifecycle.Investigating, lifecycle.Verifying,
		lifecycle.AwaitingDownload, lifecycle.ExecutingPlan,
	}
	if !slices.Equal(active, want) {
		t.Fatalf("active states = %v, want %v", active, want)
	}
	unresolved := lifecycle.StatesWhere(func(m lifecycle.StateMeta) bool { return !m.Terminal })
	if slices.Contains(unresolved, lifecycle.Resolved) || len(unresolved) != 9 {
		t.Fatalf("non-terminal states = %v", unresolved)
	}
}

func TestMeta_DriverExitsCoverEveryState(t *testing.T) {
	t.Parallel()
	for _, s := range lifecycle.States() {
		m := lifecycle.Meta(s)
		switch {
		case m.Driver == lifecycle.DriverNone && m.Active:
			t.Errorf("active state %s has no driver", s)
		case m.Driver != lifecycle.DriverNone && !m.Active:
			t.Errorf("state %s has driver %s but is not active", s, m.Driver)
		}
		if m.Progress && m.Driver == lifecycle.DriverNone {
			t.Errorf("progress state %s has no driver", s)
		}
	}
}

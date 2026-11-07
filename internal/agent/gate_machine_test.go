package agent_test

import (
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
)

func TestRunMachine_PausedAndStaleRows(t *testing.T) {
	t.Parallel()
	waiting := agent.RunSnapshot{State: agent.RunAwaitingReply, Started: true, Round: 2, Replies: 2}
	fin := []agent.RunEffect{agent.EffFinish}
	apf := []agent.RunEffect{agent.EffAnswerPending, agent.EffRecordRound, agent.EffFinish}
	cases := []rowCase{
		{waiting, agent.EvPaused, false, ended(agent.RunPaused, 2, 2), fin},
		{waiting, agent.EvStale, false, ended(agent.RunSuperseded, 2, 2), fin},
		{batch(2), agent.EvPaused, false, ended(agent.RunPaused, 2, 3), apf},
		{batch(2), agent.EvStale, false, ended(agent.RunSuperseded, 2, 3), apf},
	}
	for _, c := range cases {
		next, effs, err := agent.NextRun(c.from, agent.NewRunEvent(c.kind, c.cancelled))
		if err != nil || next != c.to || !slices.Equal(effs, c.effects) {
			t.Errorf("%v × %s = %+v %v %v, want %+v %v", c.from.State, c.kind, next, effs, err, c.to, c.effects)
		}
	}
	claiming := agent.RunSnapshot{State: agent.RunClaiming}
	for _, k := range []agent.RunEventKind{agent.EvPaused, agent.EvStale} {
		if _, _, err := agent.NextRun(claiming, agent.NewRunEvent(k, false)); err == nil {
			t.Errorf("claiming accepted %s", k)
		}
		if got, _ := agent.FailRun(claiming, agent.NewRunEvent(k, false)); k == agent.EvStale &&
			got.State != agent.RunSuperseded {
			t.Errorf("failState(claiming, stale) = %s", got.State)
		}
	}
	for _, from := range []agent.RunSnapshot{waiting, batch(2)} {
		if got, _ := agent.FailRun(from, agent.NewRunEvent(agent.EvPaused, false)); got.State != agent.RunPaused {
			t.Errorf("failState(%s, paused) = %s", from.State, got.State)
		}
		if got, _ := agent.FailRun(from, agent.NewRunEvent(agent.EvStale, false)); got.State != agent.RunSuperseded {
			t.Errorf("failState(%s, stale) = %s", from.State, got.State)
		}
	}
	if agent.CheckRun(agent.RunSnapshot{State: agent.RunPaused}) == nil {
		t.Error("R9: a paused run that never started passed Check")
	}
	meta := agent.RunMeta(agent.RunPaused)
	if !meta.Terminal || meta.WritesFinding || meta.NotRun != "autonomous actions paused" {
		t.Errorf("paused meta %+v", meta)
	}
}

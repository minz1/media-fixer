package babysit_test

import (
	"context"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func TestS4_Copies_StuckBoundary(t *testing.T) {
	t.Parallel()
	after := sweep.DefaultSettings().StuckAfter
	for _, streak := range []int{after - 1, after} {
		e := newEnv(t)
		e.addSeries(7, "Seven")
		e.target(7, "Seven", streak, 2)
		want := db.SweepTarget{Streak: streak}.Stuck(after)

		s, err := e.svc.Show(context.Background(), 7)
		if err != nil {
			t.Fatal(err)
		}
		if s.Stuck != want {
			t.Errorf("streak %d: Show.Stuck = %v, want %v", streak, s.Stuck, want)
		}
		l, err := e.svc.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		stuck := 0
		for _, r := range l.Rows {
			stuck += r.StuckSeasons
		}
		if (stuck == 1) != want || stuck > 1 {
			t.Errorf("streak %d: List StuckSeasons = %d, want stuck %v", streak, stuck, want)
		}
		targets, err := e.db.ListSweepTargets(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := len(sweep.StuckTargets(targets, after)) == 1; got != want {
			t.Errorf("streak %d: digest list stuck = %v, want %v", streak, got, want)
		}
	}
}

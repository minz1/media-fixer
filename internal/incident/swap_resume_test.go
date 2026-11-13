package incident_test

import (
	"context"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

const (
	swapNewPath = "/data/downloads/sonarr/Show.S01E06/Show.S01E06.mkv"
	swapNewSize = int64(900)
)

type swapArr struct {
	*resumeArr
}

func (a *swapArr) GetEpisodes(context.Context, int, int) ([]client.Episode, error) {
	a.note("read:episodes")
	return []client.Episode{{ID: 6, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 6, EpisodeFileID: 42}}, nil
}

func (a *swapArr) ManualImport(context.Context, []client.ImportCandidate) (int, error) {
	a.note("import")
	return 9, nil
}

type swapJelly struct{ resumeArr }

func (j *swapJelly) MediaUpdated(context.Context, []string) error {
	j.note("notify")
	return nil
}

func swapSpecForTest(t *testing.T, id string) (operation.Spec, []byte) {
	t.Helper()
	plan := &client.SwapPlan{
		Title: "Show S01E06", SeriesID: 7, Season: 1, Episode: 6, EpisodeID: 6,
		Steps: []operation.Step{
			{
				Kind: operation.StepImport, App: "sonarr", Path: swapNewPath, Size: swapNewSize, FileID: 41,
				MediaID: 7, EpisodeIDs: []int{6}, Season: 1, Episode: 6,
			},
			{
				Kind: operation.StepJellyfinNotify, App: "jellyfin", Path: "/media/tv/Show/S01E06.old.mkv",
				Name: "jf-series", Season: 1, Episode: 6,
			},
		},
	}
	source := []byte(`{"swap":true}`)
	spec, err := opexec.SwapSpec(operation.Subject{Kind: operation.SubjectIncident, ID: id}, plan, source)
	if err != nil {
		t.Fatal(err)
	}
	return spec, source
}

func TestD6_PlanLostMidImport(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{}, nil)
	inc := fx.awaiting(t, "swap_release", map[string]any{})
	spec, source := swapSpecForTest(t, inc.ID)
	lifecycletest.StorePlan(t, fx.db, inc.ID, source)
	cur := fx.svc.CurrentVersion(context.Background(), inc.ID)
	ev := lifecycle.Event{
		Kind:       lifecycle.OwnerApprove,
		Version:    cur.N,
		HasVersion: cur.Set,
		PlanFP:     card.Fingerprint(source),
		Spec:       &spec,
	}
	a, err := fx.db.ApplyIncident(
		context.Background(),
		inc.ID,
		lifecycletest.Decide(ev, lifecycletest.Limits(), time.Now()),
	)
	if err != nil || a.Snap.Op == nil {
		t.Fatalf("approve: op=%v err=%v", a.Snap.Op, err)
	}
	shutdownNow(t, fx.svc)
	if _, err = fx.db.ApplyOperation(context.Background(), a.Snap.Op.ID, func(op operation.Op) (operation.Op, error) {
		return operation.Next(op, operation.Event{Kind: operation.StepStarted, Index: 0, At: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	startupInterrupt(t, fx.db)

	boot := reboot(t, fx.db)
	jf := &swapJelly{}
	boot.svc.SetOpPorts(opexec.Ports{
		Arr:      func(string) opexec.Arr { return &swapArr{boot.arr} },
		Jellyfin: jf,
	})
	boot.arr.files = map[int]client.EpisodeFile{42: {ID: 42, Path: swapNewPath, Size: swapNewSize}}
	boot.svc.ReconcileForTest(context.Background())

	s := snapWithOp(t, fx.db, inc.ID)
	if s.Reason != lifecycle.ReasonPlanInterrupted || !lifecycle.Offers(s, time.Now(), lifecycle.ActResume) {
		t.Fatalf("incident = %s/%s, want plan_interrupted offering Resume", s.Status, s.Reason)
	}
	if boot.arr.count("import") != 0 {
		t.Fatal("the reboot posted an import")
	}

	err = boot.svc.ResumeOperation(
		context.Background(),
		inc.ID,
		incident.Version{N: s.Version, Set: true},
		s.Op.ID,
		s.Op.Version,
	)

	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if boot.arr.count("import") != 0 {
		t.Errorf("Resume posted a second import: %v", boot.arr.calls)
	}
	if jf.count("notify") != 1 {
		t.Errorf("Jellyfin was told %d times, want 1", jf.count("notify"))
	}
	op := incidentOp(t, fx, inc.ID)
	if op.Status != operation.Done || op.Results[0].State != operation.ResultDone {
		t.Errorf("op = %s %v, want done with the import done by the probe", op.Status, states(op))
	}
}

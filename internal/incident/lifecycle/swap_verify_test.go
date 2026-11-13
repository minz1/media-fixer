package lifecycle_test

import (
	"encoding/json"
	"testing"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
)

func swapDoneOp(t *testing.T) operation.Op {
	t.Helper()
	spec := operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectIncident, ID: "inc"},
		Kind:    operation.KindSwapRelease,
		Title:   "Show S01E06",
		Source:  json.RawMessage(`{"swap":true}`),
		Steps: []operation.Step{
			{
				Kind: operation.StepImport, App: "sonarr", Path: "/data/downloads/sonarr/R/e.mkv", Size: 900,
				FileID: 41, MediaID: 7, EpisodeIDs: []int{66}, Season: 1, Episode: 6,
			},
			{
				Kind: operation.StepJellyfinNotify, App: "jellyfin", Path: "/media/tv/Show/old.mkv", Name: "jf-series",
				Season: 1, Episode: 6,
			},
		},
	}
	op := mustNext(operation.Op{}, operation.Event{Kind: operation.Created, Spec: &spec})
	for _, e := range []operation.Event{
		begin(0), finish(0, operation.ResultDone), begin(1), finish(1, operation.ResultDone),
	} {
		op = mustNext(op, e)
	}
	op.ID = opID
	return op
}

func TestD8_SwapVerifySpec(t *testing.T) {
	t.Parallel()
	f := lifecycle.Finding{
		EscalateAction: lifecycle.EscalateSwapRelease,
		JSON:           json.RawMessage(`{"escalate_action":"swap_release","verify_subtitle_language":"en"}`),
	}
	from := with(execOp(swapDoneOp(t)), finding(f), func(s *lifecycle.Snapshot) { s.ItemID = "jf-old" })
	n := decide(t, from, executedEv(opID)).Next
	want := &lifecycle.SwapTarget{
		SeriesItemID: "jf-series", Season: 1, Episode: 6, SonarrSeriesID: 7, EpisodeID: 66, OldFileID: 41,
		CandidateSize: 900,
	}
	sp := n.Verify.Spec
	if sp.Kind != lifecycle.VerifySubtitles || sp.Language != "en" || sp.SubtitleItemID != "jf-old" ||
		sp.Swap == nil || *sp.Swap != *want {
		t.Fatalf("spec %+v swap %+v", sp, sp.Swap)
	}
	if b, err := json.Marshal(sp); err != nil || !json.Valid(b) || !containsKey(b, "swap") {
		t.Fatalf("json %s, err %v: the swap key is missing", b, err)
	}
}

func containsKey(b []byte, key string) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m[key] != nil
}

func TestD8_NewItemIDLandsInSnapshot(t *testing.T) {
	t.Parallel()
	from := with(snap(lifecycle.Verifying), func(s *lifecycle.Snapshot) { s.ItemID = "jf-old" })
	d := decide(t, from, tev{ev: lifecycle.Event{Kind: lifecycle.VerifyObserved, Verify: lifecycle.VerifyFixed,
		NewItemID: "jf-new"}})
	if d.Next.ItemID != "jf-new" {
		t.Fatalf("ItemID = %q", d.Next.ItemID)
	}
	d = decide(t, from, tev{ev: lifecycle.Event{Kind: lifecycle.VerifyObserved, Verify: lifecycle.VerifyFixed}})
	if d.Next.ItemID != "jf-old" {
		t.Fatalf("ItemID without a new id = %q", d.Next.ItemID)
	}
}

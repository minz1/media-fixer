package opexec_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/opexec"
)

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestReplaceResultFrom_MatchesOldShape(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"complete": "", "second delete fails": "/episodefile/22", "blocklist fails": "/history/failed/12",
	}
	for name, failOn := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan := plans(false)["season"]
			oldRec := newRecorder(t, failOn)
			oldRes, _ := client.NewArr(oldRec.srv.URL, "key").ExecuteReplace(context.Background(), plan)
			spec, err := opexec.ReplaceSpec(incident(), plan, nil)
			if err != nil {
				t.Fatalf("ReplaceSpec: %v", err)
			}
			got := execOnRecorder(t, newRecorder(t, failOn), mustOp(t, spec))
			if want, have := asJSON(t, oldRes), asJSON(t, opexec.ReplaceResultFrom(got)); want != have {
				t.Fatalf("result\nold %s\nnew %s", want, have)
			}
		})
	}
}

func TestReplaceResultFrom_EmptyPlanShape(t *testing.T) {
	t.Parallel()
	plan := plans(true)["movie"]
	plan.Files = nil
	oldRec := newRecorder(t, "")
	oldRes, _ := client.NewArr(oldRec.srv.URL, "key").ExecuteReplace(context.Background(), plan)
	spec, err := opexec.ReplaceSpec(incident(), plan, nil)
	if err != nil {
		t.Fatalf("ReplaceSpec: %v", err)
	}
	got := execOnRecorder(t, newRecorder(t, ""), mustOp(t, spec))
	if want, have := asJSON(t, oldRes), asJSON(t, opexec.ReplaceResultFrom(got)); want != have {
		t.Fatalf("result\nold %s\nnew %s", want, have)
	}
}

func TestReaddResultFrom_MatchesOldShape(t *testing.T) {
	t.Parallel()
	plan := &client.ReaddPlan{Name: "Show", InfoHash: "abc", Category: "tv", Magnet: "magnet:?xt=abc"}
	spec, err := opexec.ReaddSpec(incident(), plan, nil)
	if err != nil {
		t.Fatalf("ReaddSpec: %v", err)
	}
	full := newRig(mustOp(t, spec))
	got := full.x.Run(context.Background(), full.store.op)
	want := `{"deleted":true,"readded":true,"name":"Show","magnet":"magnet:?xt=abc"}`
	if have := asJSON(t, opexec.ReaddResultFrom(got)); have != want {
		t.Fatalf("full = %s", have)
	}
	part := newRig(mustOp(t, spec))
	part.decy.fail["tadd"] = status(500)
	got = part.x.Run(context.Background(), part.store.op)
	want = `{"deleted":true,"readded":false,"name":"Show","magnet":"magnet:?xt=abc"}`
	if have := asJSON(t, opexec.ReaddResultFrom(got)); have != want {
		t.Fatalf("partial = %s", have)
	}
}

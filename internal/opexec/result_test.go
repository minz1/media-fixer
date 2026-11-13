package opexec_test

import (
	"encoding/json"
	"testing"

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
	cases := []struct{ name, failOn, want string }{
		{
			"complete", "",
			`{"deleted_files":[{"id":21,"path":"/tv/a.mkv","size":10},{"id":22,"path":"/tv/b.mkv","size":20}],` +
				`"blocklisted_grabs":[11,12],"search_triggered":"SeasonSearch"}`,
		},
		{
			"second delete fails", "/episodefile/22",
			`{"deleted_files":[{"id":21,"path":"/tv/a.mkv","size":10}],"blocklisted_grabs":[11,12],` +
				`"search_triggered":""}`,
		},
		{
			"blocklist fails", "/history/failed/12",
			`{"deleted_files":null,"blocklisted_grabs":[11],"search_triggered":""}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec, err := opexec.ReplaceSpec(incident(), plans(false)["season"], nil)
			if err != nil {
				t.Fatalf("ReplaceSpec: %v", err)
			}
			got := execOnRecorder(t, newRecorder(t, tc.failOn), mustOp(t, spec))
			if have := asJSON(t, opexec.ReplaceResultFrom(got)); have != tc.want {
				t.Fatalf("result\nwant %s\nhave %s", tc.want, have)
			}
		})
	}
}

func TestReplaceResultFrom_EmptyPlanShape(t *testing.T) {
	t.Parallel()
	plan := plans(true)["movie"]
	plan.Files = nil
	spec, err := opexec.ReplaceSpec(incident(), plan, nil)
	if err != nil {
		t.Fatalf("ReplaceSpec: %v", err)
	}
	got := execOnRecorder(t, newRecorder(t, ""), mustOp(t, spec))
	want := `{"deleted_files":null,"blocklisted_grabs":null,"search_triggered":"MoviesSearch"}`
	if have := asJSON(t, opexec.ReplaceResultFrom(got)); have != want {
		t.Fatalf("result\nwant %s\nhave %s", want, have)
	}
}

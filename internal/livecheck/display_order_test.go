package livecheck_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/livecheck"
)

func allStatuses() []livecheck.Status {
	return []livecheck.Status{
		livecheck.StatusOK,
		livecheck.StatusDegraded,
		livecheck.StatusFail,
		livecheck.StatusMissing,
		livecheck.StatusSkipped,
		livecheck.StatusUnconfigured,
	}
}

func TestD10_8_OneDisplayOrder(t *testing.T) {
	t.Parallel()

	order := livecheck.StatusDisplayOrderForTest()
	seen := make(map[livecheck.Status]int)
	for i, s := range order {
		seen[s]++
		if got := livecheck.StatusRankForTest(s); got != i {
			t.Errorf("rank of %s = %d, want %d", s, got, i)
		}
	}
	var results []livecheck.Result
	var want []string
	for _, s := range allStatuses() {
		if seen[s] != 1 {
			t.Errorf("status %s appears %d times in the display order", s, seen[s])
		}
		results = append(results, livecheck.Result{Status: s})
	}
	for _, s := range order {
		want = append(want, fmt.Sprintf("1 %s", s))
	}
	if got := livecheck.SummaryLineForTest(results); got != strings.Join(want, " · ") {
		t.Errorf("summary = %q, want %q", got, strings.Join(want, " · "))
	}
	for _, s := range allStatuses() {
		report := &livecheck.Report{Results: []livecheck.Result{{Status: s}}}
		if got := livecheck.HasFailures(report); got != livecheck.FailingForTest(s) {
			t.Errorf("HasFailures for %s = %v, failing = %v", s, got, livecheck.FailingForTest(s))
		}
		wantFail := s == livecheck.StatusFail || s == livecheck.StatusMissing
		if livecheck.FailingForTest(s) != wantFail {
			t.Errorf("failing(%s) = %v, want %v", s, !wantFail, wantFail)
		}
	}
}

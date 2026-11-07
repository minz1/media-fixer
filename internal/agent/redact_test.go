package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
)

const (
	secretToken = "SEKRIT123"
	secretBody  = "bad request for http://sonarr.lan/api/v3/command?apikey=SEKRIT123 (X-Api-Key: SEKRIT123)"
)

func secretFailing(s *callServer, method, path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == method && r.URL.Path == path {
			s.note(r.Method + " " + r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(secretBody))
			return
		}
		s.ServeHTTP(w, r)
	})
}

func newSecretServer(t *testing.T, method, path string) string {
	t.Helper()
	srv := httptest.NewServer(secretFailing(&callServer{}, method, path))
	t.Cleanup(srv.Close)
	return srv.URL
}

func eventPayloads(t *testing.T, database *db.DB, id, kind string) []string {
	t.Helper()
	events, err := database.EventsSince(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, string(e.Payload))
		}
	}
	return out
}

func assertRedacted(t *testing.T, label string, texts []string) {
	t.Helper()
	if len(texts) == 0 {
		t.Errorf("%s: nothing stored", label)
		return
	}
	joined := strings.Join(texts, "\n")
	if strings.Contains(joined, secretToken) {
		t.Errorf("%s holds the secret: %s", label, joined)
	}
	if !strings.Contains(joined, "REDACTED") {
		t.Errorf("%s has no REDACTED marker: %s", label, joined)
	}
}

func TestRulingA2_StoredErrorTextRedacted(t *testing.T) {
	t.Parallel()
	url := newSecretServer(t, http.MethodPost, "/api/v3/command")
	ag, database, inc := newRig(t, url,
		script("arr_search_missing", `{"media_type":"movie","title":"Arrival"}`), rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	acts := listActions(t, database, inc.ID)
	if len(acts) != 1 || acts[0].Status != db.ActionFailed {
		t.Fatalf("actions = %+v, want one failed row", acts)
	}
	assertRedacted(t, "actions_log error", []string{acts[0].Error})
	assertRedacted(t, "action_applied payload", eventPayloads(t, database, inc.ID, "action_applied"))
	assertRedacted(t, "llm_round", eventPayloads(t, database, inc.ID, "llm_round"))
}

func TestReviewP3_CapReasonRedacted(t *testing.T) {
	t.Parallel()
	url := newSecretServer(t, http.MethodPost, "/api/v3/command")
	claim := `{"root_cause":"rc","confidence":"medium","primary_action":"arr_search_missing",` +
		`"requires_approval":false}`
	ag, database, inc := newRig(t, url, []string{
		toolCallResponse("arr_search_missing", `{"media_type":"movie","title":"Arrival"}`),
		toolCallResponse("complete_diagnosis", claim),
		toolCallResponse("complete_diagnosis", claim),
		toolCallResponse("complete_diagnosis", claim),
	}, rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.Finding == nil || exit.Finding.Outcome.Kind != agent.DiagnosisManual {
		t.Fatalf("exit %s %+v, want the capped manual finding", exit.State, exit.Finding)
	}
	assertRedacted(t, "cap reason", []string{exit.Finding.Outcome.Reason, exit.Finding.PrimaryReason})
	assertRedacted(t, "repeat replies", toolReplies(exit.Conversation)[1:3])
	assertRedacted(t, "diagnosis_completed", eventPayloads(t, database, inc.ID, "diagnosis_completed"))
}

func TestRulingA2_RefusalTextRedacted(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, nil, rigOpts{})
	reason := "rejected by http://radarr.lan/api/v3/movie?token=" + secretToken
	reply := ag.RefuseForTest(context.Background(), inc, "radarr_rescan", reason, newRecGate(database, inc))
	acts := listActions(t, database, inc.ID)
	if len(acts) != 1 || acts[0].Status != db.ActionRefused {
		t.Fatalf("actions = %+v, want one refused row", acts)
	}
	assertRedacted(t, "refused row error", []string{acts[0].Error})
	assertRedacted(t, "refusal reply", []string{reply})
}

func TestRulingA2_ReadToolErrorRedacted(t *testing.T) {
	t.Parallel()
	url := newSecretServer(t, http.MethodGet, "/api/v3/movie")
	ag, database, inc := newRig(t, url,
		script("arr_grab_history", `{"media_type":"movie","title":"Arrival"}`), rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	assertRedacted(t, "read reply", toolReplies(exit.Conversation)[:1])
	assertRedacted(t, "llm_round", eventPayloads(t, database, inc.ID, "llm_round"))
}

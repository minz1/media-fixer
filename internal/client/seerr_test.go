package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

func TestSeerrCommentAndResolve(t *testing.T) {
	t.Parallel()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Api-Key") != "key" {
			t.Errorf("%s %s with key %q", r.Method, r.URL.Path, r.Header.Get("X-Api-Key"))
		}
		var body struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, r.URL.Path+" "+body.Message)
	}))
	defer srv.Close()

	c := client.NewSeerr(srv.URL+"/", "key")
	if err := c.Comment(context.Background(), "42", "fixed"); err != nil {
		t.Fatal(err)
	}
	if err := c.Resolve(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/api/v1/issue/42/comment fixed", "/api/v1/issue/42/resolved "}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

func TestSeerrErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if err := client.NewSeerr(srv.URL, "key").Resolve(context.Background(), "42"); err == nil {
		t.Error("expected an error on 403")
	}
}

func TestSeerrRejectsNonNumericIssueID(t *testing.T) {
	t.Parallel()
	if err := client.NewSeerr("http://127.0.0.1:1", "key").Resolve(context.Background(), "../settings"); err == nil {
		t.Error("expected an error for a non-numeric issue id")
	}
}

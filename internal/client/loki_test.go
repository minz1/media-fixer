package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
)

const lokiTestLimit = 100
const lokiSmallLimit = 10

func TestLoki_QueryRange(t *testing.T) {
	t.Parallel()
	ts := time.Now().Add(-5 * time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("query") == "" {
			t.Error("expected query param")
		}
		if q.Get("limit") != strconv.Itoa(lokiTestLimit) {
			t.Errorf("limit: %q", q.Get("limit"))
		}

		// Loki returns nanosecond timestamps as JSON strings.
		resp := map[string]any{
			"data": map[string]any{
				"result": []any{
					map[string]any{
						"values": []any{
							[]string{strconv.FormatInt(ts.UnixNano(), 10), "error: EIO on /mnt/fuse/movie.mkv"},
							[]string{strconv.FormatInt(ts.Add(time.Second).UnixNano(), 10), "retry attempt 1"},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c, _ := client.NewLoki(srv.URL, "", "")
	from := ts.Add(-time.Minute)
	to := ts.Add(time.Minute)

	result, err := c.QueryRange(context.Background(), `{unit=~"jellyfin|decypharr"}`, from, to, lokiTestLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Lines) != 2 {
		t.Fatalf("lines: got %d want 2", len(result.Lines))
	}
	if result.Lines[0].Message != "error: EIO on /mnt/fuse/movie.mkv" {
		t.Errorf("line 0: %q", result.Lines[0].Message)
	}
}

func TestLoki_EmptyResult(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"result": []any{}},
		})
	}))
	defer srv.Close()

	c, _ := client.NewLoki(srv.URL, "", "")
	result, err := c.QueryRange(
		context.Background(),
		`{unit="jellyfin"}`,
		time.Now().Add(-time.Minute),
		time.Now(),
		lokiSmallLimit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Lines) != 0 {
		t.Errorf("expected 0 lines, got %d", len(result.Lines))
	}
}

func TestLoki_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c, _ := client.NewLoki(srv.URL, "", "")
	_, err := c.QueryRange(context.Background(), `{unit="x"}`, time.Now().Add(-time.Minute), time.Now(), lokiSmallLimit)
	if err == nil {
		t.Fatal("expected error on 400")
	}
}

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

func serveBody(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestQ6a_FileStateDecode(t *testing.T) {
	t.Parallel()
	sonarr, err := client.NewArr(serveFile(t, "testdata/sonarr-queue-2026-10-29.json").URL, "key").
		QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	radarr, err := client.NewArr(serveFile(t, "testdata/radarr-queue-2026-10-29.json").URL, "key").
		QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	movies, err := client.NewArr(serveBody(t, `{"totalRecords":4,"records":[
		{"id":1,"movieId":5,"movie":{"hasFile":true}},
		{"id":2,"movieId":6,"movie":{"hasFile":false}},
		{"id":3,"movieId":7,"movie":{}},
		{"id":4,"episodeId":9}]}`).URL, "key").QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name string
		got  client.FileState
		want client.FileState
	}{
		{"episode true", entryByID(t, sonarr, 1464435435).File, client.FilePresent},
		{"episode false", entryByID(t, sonarr, 1886296760).File, client.FileMissing},
		{"unmapped episode", entryByID(t, sonarr, 1304969599).File, client.FileUnknown},
		{"unmapped radarr", radarr[0].File, client.FileUnknown},
		{"movie true", entryByID(t, movies, 1).File, client.FilePresent},
		{"movie false", entryByID(t, movies, 2).File, client.FileMissing},
		{"movie absent", entryByID(t, movies, 3).File, client.FileUnknown},
		{"episode absent", entryByID(t, movies, 4).File, client.FileUnknown},
	}
	for _, w := range want {
		if w.got != w.want {
			t.Errorf("%s: File = %d, want %d", w.name, w.got, w.want)
		}
	}
}

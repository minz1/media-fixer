package mediaagent_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/mediaagent"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

func TestB1_ParseProbe(t *testing.T) {
	t.Parallel()
	stdout, err := os.ReadFile("testdata/overgeared-e06.json")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile("testdata/overgeared-e06.stderr")
	if err != nil {
		t.Fatal(err)
	}
	res, err := mediaagent.ParseProbe(stdout, stderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Streams) != 9 {
		t.Fatalf("streams = %d, want 9 (attachment dropped)", len(res.Streams))
	}
	if s := res.Streams[3]; s.CodecName != "ass" || s.Language != "por" || s.Title != "Portuguese (Brazil) [Forced]" ||
		!s.Forced ||
		s.Default {
		t.Errorf("stream 3 = %+v", s)
	}
	if s := res.Streams[0]; !s.Default || s.CodecType != "video" {
		t.Errorf("stream 0 = %+v", s)
	}
	for _, i := range []int{5, 7} {
		if s := res.Streams[i]; s.CodecName != "" || s.CodecType != "subtitle" || s.Index != i {
			t.Errorf("stream %d = %+v", i, s)
		}
	}
	if !slices.Equal(res.UnknownCodecIDs, []string{"S_TEXT/WEBVTT"}) {
		t.Errorf("unknown ids = %v", res.UnknownCodecIDs)
	}
}

type probeOps struct {
	allowAllOps

	got string
}

func (p *probeOps) ProbeStreams(_ context.Context, path string) (*mediaagentapi.ProbeResult, error) {
	p.got = path
	return &mediaagentapi.ProbeResult{Streams: []mediaagentapi.ProbeStream{
		{Index: 6, CodecType: "subtitle", CodecName: "ass", Language: "eng", Default: true},
	}}, nil
}

func TestB1_ProbeRoute(t *testing.T) {
	t.Parallel()
	ops := &probeOps{}
	h := mediaagent.NewHandler(ops, "sekrit", slog.New(slog.DiscardHandler))
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sekrit")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := do(`{"path":"/mnt/x.mkv"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got mediaagentapi.ProbeResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if ops.got != "/mnt/x.mkv" || len(got.Streams) != 1 || got.Streams[0].Language != "eng" || !got.Streams[0].Default {
		t.Errorf("got %+v, path %q", got, ops.got)
	}
	if bad := do(`{}`); bad.Code != http.StatusBadRequest {
		t.Errorf("missing path: status = %d, want 400", bad.Code)
	}
}

func TestB1_ProbeRunsSandboxedAsUser(t *testing.T) {
	t.Parallel()
	argv := mediaagent.ProbeArgv("/nix/store/x/bin/ffprobe", "jellyfin", "/mnt/x.mkv")
	if argv[0] != "systemd-run" {
		t.Fatalf("argv[0] = %q, want systemd-run", argv[0])
	}
	for _, want := range []string{
		"--wait", "--pipe", "--collect",
		"User=jellyfin", "NoNewPrivileges=yes", "ProtectSystem=strict", "ProtectHome=yes",
		"PrivateTmp=yes", "PrivateNetwork=yes", "PrivateDevices=yes", "CapabilityBoundingSet=", "RuntimeMaxSec=30",
	} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv lacks %q: %v", want, argv)
		}
	}
	sep := slices.Index(argv, "--")
	if sep < 0 {
		t.Fatalf("no -- before the command: %v", argv)
	}
	if got, want := argv[sep+1:], []string{
		"/nix/store/x/bin/ffprobe", "-v", "info", "-show_streams", "-of", "json", "/mnt/x.mkv",
	}; !slices.Equal(got, want) {
		t.Errorf("command = %v, want %v", got, want)
	}
}

func TestB1_ProbeFailureKeepsStderrTail(t *testing.T) {
	bin := t.TempDir()
	for name, body := range map[string]string{
		"ffprobe":     "#!/bin/sh\nexit 0\n",
		"systemd-run": "#!/bin/sh\necho 'noise' >&2\necho 'x.mkv: Invalid data found when processing input' >&2\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	file := filepath.Join(root, "x.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := mediaagent.NewRealOps([]string{root}).ProbeStreams(t.Context(), file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Error, "x.mkv: Invalid data found when processing input") {
		t.Errorf("error = %q", res.Error)
	}
}

func TestB1_ProbeRefusesOutsideMounts(t *testing.T) {
	t.Parallel()
	ops := mediaagent.NewRealOps([]string{t.TempDir()})
	_, err := ops.ProbeStreams(t.Context(), "/etc/passwd")
	if err == nil || !strings.Contains(err.Error(), "outside allowed mount roots") {
		t.Fatalf("err = %v, want mount refusal", err)
	}
}

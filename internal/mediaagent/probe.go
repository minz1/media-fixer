package mediaagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

const (
	probeTimeout   = 30 * time.Second
	probeStderrCap = 256 * 1024
)

var unknownCodecRE = regexp.MustCompile(`Unknown/unsupported AVCodecID (\S+?)\.?\s*$`)

type tailBuffer struct{ buf []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - probeStderrCap; over > 0 {
		t.buf = slices.Delete(t.buf, 0, over)
	}
	return len(p), nil
}

type ffprobeOut struct {
	Streams []struct {
		Index       int               `json:"index"`
		CodecType   string            `json:"codec_type"`
		CodecName   string            `json:"codec_name"`
		Tags        map[string]string `json:"tags"`
		Disposition struct {
			Default int `json:"default"`
			Forced  int `json:"forced"`
		} `json:"disposition"`
	} `json:"streams"`
}

func parseProbe(stdout, stderr []byte) (*mediaagentapi.ProbeResult, error) {
	var out ffprobeOut
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("ffprobe json: %w", err)
	}
	res := &mediaagentapi.ProbeResult{Streams: []mediaagentapi.ProbeStream{}}
	for _, s := range out.Streams {
		if s.CodecType == "attachment" {
			continue
		}
		res.Streams = append(res.Streams, mediaagentapi.ProbeStream{
			Index:     s.Index,
			CodecType: s.CodecType,
			CodecName: s.CodecName,
			Language:  s.Tags["language"],
			Title:     s.Tags["title"],
			Forced:    s.Disposition.Forced == 1,
			Default:   s.Disposition.Default == 1,
		})
	}
	for line := range strings.SplitSeq(string(stderr), "\n") {
		m := unknownCodecRE.FindStringSubmatch(line)
		if m != nil && !slices.Contains(res.UnknownCodecIDs, m[1]) {
			res.UnknownCodecIDs = append(res.UnknownCodecIDs, m[1])
		}
	}
	return res, nil
}

func (o *RealOps) ProbeStreams(ctx context.Context, path string) (*mediaagentapi.ProbeResult, error) {
	path, mountErr := o.resolveUnderMounts(path)
	if mountErr != nil {
		return nil, mountErr
	}
	info, err := os.Stat(path)
	if err != nil {
		return &mediaagentapi.ProbeResult{Error: err.Error(), NotFound: os.IsNotExist(err)}, nil
	}
	if !info.Mode().IsRegular() {
		return &mediaagentapi.ProbeResult{Error: "path is not a regular file"}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var stdout bytes.Buffer
	var stderr tailBuffer
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "debug", "-show_streams", "-of", "json", path)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		if errors.Is(runErr, exec.ErrNotFound) {
			return nil, errors.New("ffprobe not on media-agent's PATH")
		}
		lines := strings.Split(strings.TrimSpace(string(stderr.buf)), "\n")
		return &mediaagentapi.ProbeResult{Error: fmt.Sprintf("ffprobe: %v: %s", runErr, lines[len(lines)-1])}, nil
	}
	return parseProbe(stdout.Bytes(), stderr.buf)
}

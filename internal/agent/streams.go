package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

const (
	streamTypeSubtitle = "Subtitle"
	langEnglish        = "eng"
	cueScanLines       = 50
	cueSampleLines     = 3
	minDetectLines     = 3
	minVerifyCues      = 3
	itemEpisode        = "Episode"
	itemMovie          = "Movie"
	itemSeries         = "Series"
)

// shortcut: a fixed table of the languages seen here, add a row when an incident needs one
func langCodes(s string) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "en", "eng", "english":
		return "en", langEnglish, true
	case "ja", "jpn", "japanese":
		return "ja", "jpn", true
	case "es", "spa":
		return "es", "spa", true
	case "pt", "por":
		return "pt", "por", true
	case "fr", "fre", "fra":
		return "fr", "fre", true
	case "de", "ger", "deu":
		return "de", "ger", true
	case "it", "ita":
		return "it", "ita", true
	case "ar", "ara":
		return "ar", "ara", true
	case "zh", "chi", "zho":
		return "zh", "chi", true
	}
	return "", "", false
}

type AlignedStream struct {
	JellyfinIndex     int    `json:"jellyfin_index"`
	Language          string `json:"language"`
	Title             string `json:"title,omitempty"`
	FileIndex         int    `json:"file_index"`
	FfmpegMapsTo      int    `json:"ffmpeg_maps_to"`
	FfmpegMapsToCodec string `json:"ffmpeg_maps_to_codec"`
	Drift             bool   `json:"drift"`
}

type Alignment struct {
	Streams               []AlignedStream `json:"streams"`
	IndexDrift            bool            `json:"index_drift"`
	UnknownCodecTracks    []int           `json:"unknown_codec_tracks"`
	UnknownCodecIDs       []string        `json:"unknown_codec_ids,omitempty"`
	WantedLanguagePresent bool            `json:"wanted_language_present"`
	Path                  string          `json:"path"`
}

func alignStreams(file []mediaagentapi.ProbeStream, jf []client.MediaStream, wantISO2 string) Alignment {
	_, want, _ := langCodes(wantISO2)
	is := func(lang string) bool {
		_, iso2, ok := langCodes(lang)
		return ok && iso2 == want
	}
	a := Alignment{Streams: []AlignedStream{}, UnknownCodecTracks: []int{}}
	codecs := map[int]string{}
	var known []mediaagentapi.ProbeStream
	for _, s := range file {
		codecs[s.Index] = s.CodecName
		if s.CodecType != "subtitle" {
			continue
		}
		if s.CodecName == "" {
			a.UnknownCodecTracks = append(a.UnknownCodecTracks, s.Index)
			continue
		}
		known = append(known, s)
		a.WantedLanguagePresent = a.WantedLanguagePresent || is(s.Language)
	}
	pos, k := 0, 0
	for _, s := range jf {
		if s.IsExternal {
			a.WantedLanguagePresent = a.WantedLanguagePresent || (s.Type == streamTypeSubtitle && is(s.Language))
			continue
		}
		n := pos
		pos++
		if s.Type != streamTypeSubtitle {
			continue
		}
		row := AlignedStream{
			JellyfinIndex: s.Index, Language: s.Language, Title: s.Title, FileIndex: -1, FfmpegMapsTo: n,
		}
		if k < len(known) {
			row.FileIndex = known[k].Index
		}
		k++
		row.FfmpegMapsToCodec = codecs[n]
		if row.FfmpegMapsToCodec == "" {
			row.FfmpegMapsToCodec = "unknown"
		}
		row.Drift = row.FileIndex != row.FfmpegMapsTo
		a.IndexDrift = a.IndexDrift || row.Drift
		a.Streams = append(a.Streams, row)
	}
	return a
}

const wordTrimSet = ".,!?;:\"'()[]-"

var (
	cueTagRe   = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)
	cueIndexRe = regexp.MustCompile(`^\d+$`)
)

func cueLines(srt string, n int) []string {
	var out []string
	for line := range strings.SplitSeq(srt, "\n") {
		line = strings.TrimSpace(cueTagRe.ReplaceAllString(line, ""))
		if line == "" || cueIndexRe.MatchString(line) || strings.Contains(line, "-->") {
			continue
		}
		out = append(out, line)
		if len(out) == n {
			break
		}
	}
	return out
}

func isStopword(w string) bool {
	switch w {
	case "the", "and", "to", "of", "in", "that", "is", "it", "you", "i", "for", "was", "on", "with", "he",
		"this", "are", "be", "not", "have", "but", "we", "they", "what", "at", "as", "do", "my", "his", "from":
		return true
	}
	return false
}

// shortcut: English only, add languages when an incident needs one
func detectLanguage(text string) string {
	lines := cueLines(text, cueScanLines)
	if len(lines) < minDetectLines {
		return ""
	}
	words, hits := 0, 0
	for _, l := range lines {
		for w := range strings.FieldsSeq(strings.ToLower(l)) {
			words++
			if isStopword(strings.Trim(w, wordTrimSet)) {
				hits++
			}
		}
	}
	if words > 0 && hits*4 >= words {
		return langEnglish
	}
	return ""
}

type ExtractTest struct {
	Status           int      `json:"status"`
	Bytes            int      `json:"bytes"`
	TimedOut         bool     `json:"timed_out,omitempty"`
	Cues             int      `json:"cues"`
	FirstLines       []string `json:"first_lines"`
	DetectedLanguage string   `json:"detected_language,omitempty"`
	StreamLanguage   string   `json:"stream_language"`
}

// shortcut: 20 GB keeps an extraction under the 90 s cap; raise it if Jellyfin extracts faster or the cap changes
const subtitleExtractMaxBytes = 20 << 30

var ErrSubtitleFileTooLarge = errors.New(
	"subtitle extraction refused: the file is over 20 GB, so extracting an embedded track would exceed the 90 s cap")

func (d *Dispatcher) extractTest(ctx context.Context, itemID string, index int) (*ExtractTest, error) {
	info, err := d.Jellyfin.PlaybackInfo(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if len(info.MediaSources) == 0 {
		return nil, fmt.Errorf("item %s has no media source", itemID)
	}
	src := info.MediaSources[0]
	var stream *client.MediaStream
	for i := range src.MediaStreams {
		if s := &src.MediaStreams[i]; s.Type == streamTypeSubtitle && s.Index == index {
			stream = s
		}
	}
	if stream == nil {
		return nil, fmt.Errorf("item %s has no subtitle stream %d", itemID, index)
	}
	return d.extractStream(ctx, itemID, src, stream)
}

func (d *Dispatcher) extractStream(
	ctx context.Context, itemID string, src client.MediaSource, stream *client.MediaStream,
) (*ExtractTest, error) {
	if !stream.IsExternal && src.Size > subtitleExtractMaxBytes {
		return nil, ErrSubtitleFileTooLarge
	}
	fetch, err := d.Jellyfin.SubtitleText(ctx, itemID, src.ID, stream.Index)
	if err != nil {
		return nil, err
	}
	out := &ExtractTest{
		Status: fetch.Status, Bytes: fetch.Bytes, TimedOut: fetch.TimedOut,
		Cues: strings.Count(fetch.Text, "-->"), FirstLines: cueLines(fetch.Text, cueSampleLines),
		DetectedLanguage: detectLanguage(fetch.Text), StreamLanguage: stream.Language,
	}
	if out.FirstLines == nil {
		out.FirstLines = []string{}
	}
	return out, nil
}

func subtitleReadToolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name: toolStreamAlignment,
			Def: &openai.FunctionDefinition{
				Name: toolStreamAlignment,
				Description: "Compare Jellyfin's subtitle streams with the file's real streams (in code). " +
					"index_drift true means Jellyfin extracts the wrong track: its stream table dropped tracks " +
					"ffmpeg can't read. Use it on every subtitle incident once the episode item is known.",
				Parameters: jsonSchema(map[string]any{
					paramItemID:   param("string", "Jellyfin Episode or Movie item ID"),
					paramLanguage: param("string", "Wanted subtitle language (en, eng or english)"),
				}, []string{paramItemID, paramLanguage}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readStreamAlignment,
		},
		{
			Name: toolSubtitleExtractTest,
			Def: &openai.FunctionDefinition{
				Name: toolSubtitleExtractTest,
				Description: "Fetch one subtitle track through Jellyfin as SRT (the same extraction a player " +
					"triggers; slow on huge files, 90 s cap) and report status, size, cue count, first lines and " +
					"detected language. Use the jellyfin_index from stream_alignment or jellyfin_playback_info.",
				Parameters: jsonSchema(map[string]any{
					paramItemID:    param("string", "Jellyfin Episode or Movie item ID"),
					paramStreamIdx: param("integer", "Jellyfin stream index of the subtitle track"),
				}, []string{paramItemID, paramStreamIdx}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readSubtitleExtractTest,
		},
	}
}

const verifySubtitleMaxStreams = 2

func (a *Agent) VerifySubtitle(ctx context.Context, itemID, lang string) bool {
	_, want, ok := langCodes(lang)
	if !ok || a.disp == nil || a.disp.Jellyfin == nil {
		return false
	}
	info, err := a.disp.Jellyfin.PlaybackInfo(ctx, itemID)
	if err != nil || len(info.MediaSources) == 0 {
		return false
	}
	src := info.MediaSources[0]
	var tagged []*client.MediaStream
	for i := range src.MediaStreams {
		s := &src.MediaStreams[i]
		if _, iso3, known := langCodes(s.Language); s.Type == streamTypeSubtitle && known && iso3 == want {
			tagged = append(tagged, s)
		}
	}
	slices.SortStableFunc(tagged, func(x, y *client.MediaStream) int {
		return cmp.Compare(boolInt(y.IsExternal), boolInt(x.IsExternal))
	})
	tried := 0
	for _, s := range tagged {
		if tried == verifySubtitleMaxStreams {
			break
		}
		res, extractErr := a.disp.extractStream(ctx, itemID, src, s)
		if errors.Is(extractErr, ErrSubtitleFileTooLarge) {
			continue
		}
		tried++
		if extractErr == nil && res.Status == http.StatusOK && res.Cues >= minVerifyCues &&
			(want != langEnglish || res.DetectedLanguage == langEnglish) {
			return true
		}
	}
	return false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func alignedItems(messages []openai.ChatCompletionMessage) []string {
	items := map[string]string{}
	var out []string
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name != toolStreamAlignment {
				continue
			}
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if id, _ := args[paramItemID].(string); id != "" {
				items[tc.ID] = id
			}
		}
		if m.Role != openai.ChatMessageRoleTool || items[m.ToolCallID] == "" {
			continue
		}
		var reply map[string]any
		if json.Unmarshal([]byte(m.Content), &reply) == nil && reply[keyError] == nil {
			out = append(out, items[m.ToolCallID])
		}
	}
	return out
}

func (a *Agent) resolveSubtitleItem(ctx context.Context, inc *db.Incident, verifyItem string, aligned []string) string {
	if a.disp == nil || a.disp.Jellyfin == nil {
		return ""
	}
	if inc.JellyfinItemID != "" {
		info, err := a.disp.Jellyfin.ItemInfo(ctx, inc.JellyfinItemID)
		if err != nil {
			return ""
		}
		if info.Type == itemEpisode || info.Type == itemMovie {
			return inc.JellyfinItemID
		}
		if info.Type != itemSeries {
			return ""
		}
	}
	if verifyItem == "" || !slices.Contains(aligned, verifyItem) {
		return ""
	}
	info, err := a.disp.Jellyfin.ItemInfo(ctx, verifyItem)
	if err != nil {
		return ""
	}
	switch {
	case info.Type == itemEpisode && (inc.JellyfinItemID == "" || info.SeriesID == inc.JellyfinItemID):
		return verifyItem
	case info.Type == itemMovie && inc.JellyfinItemID == "":
		return verifyItem
	}
	return ""
}

func SubtitleLanguage(s string) string {
	if strings.TrimSpace(s) == "" {
		return langEnglish
	}
	if _, iso3, ok := langCodes(s); ok {
		return iso3
	}
	return s
}

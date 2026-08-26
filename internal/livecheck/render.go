package livecheck

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// RenderJSON writes the report as indented JSON.
func RenderJSON(w io.Writer, report *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// tabwriterFlags/padding mirror the go standard library's typical column
// alignment for a compact but readable fixed-width table.
const (
	tabwriterMinWidth = 0
	tabwriterTabWidth = 0
	tabwriterPadding  = 2
)

// RenderText writes the human-readable table shown by the CLI.
func RenderText(w io.Writer, report *Report) {
	fmt.Fprintf(w, "media-fixer live check — %s  (write=%v disruptive=%v)\n\n",
		report.StartedAt.Format("2006-01-02T15:04:05Z07:00"), report.Options.AllowWrite, report.Options.AllowDisruptive)

	renderFixtures(w, report.Fixtures)
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, tabwriterMinWidth, tabwriterTabWidth, tabwriterPadding, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tRISK\tSTATUS\tLATENCY\tDETAIL")
	for _, r := range report.Results {
		fmt.Fprintln(tw, resultRow(r))
	}
	_ = tw.Flush()

	if len(report.Env) > 0 {
		fmt.Fprintln(w, "\nENVIRONMENT")
		etw := tabwriter.NewWriter(w, tabwriterMinWidth, tabwriterTabWidth, tabwriterPadding, ' ', 0)
		for _, r := range report.Env {
			fmt.Fprintln(etw, resultRow(r))
		}
		_ = etw.Flush()
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, summaryLine(report.Results))
}

func renderFixtures(w io.Writer, fx Fixtures) {
	parts := []string{}
	if fx.JellyfinItemID != "" {
		parts = append(parts, fmt.Sprintf("jellyfin=%s:%s", fx.JellyfinItemType, fx.JellyfinItemID))
	}
	if fx.SeriesTitle != "" {
		parts = append(parts, fmt.Sprintf("sonarr=%q", fx.SeriesTitle))
	}
	if fx.MovieTitle != "" {
		parts = append(parts, fmt.Sprintf("radarr=%q", fx.MovieTitle))
	}
	if fx.TorrentName != "" {
		parts = append(parts, fmt.Sprintf("torrent=%q", fx.TorrentName))
	}
	if fx.SamplePath != "" {
		parts = append(parts, "file="+fx.SamplePath)
	}
	if len(parts) > 0 {
		fmt.Fprintln(w, "fixtures  "+strings.Join(parts, " "))
	}
	for _, note := range fx.Notes {
		fmt.Fprintln(w, "  ! "+note)
	}
}

func resultRow(r Result) string {
	latency := "-"
	if r.LatencyMS > 0 {
		latency = fmt.Sprintf("%dms", r.LatencyMS)
	}
	detail := r.Detail
	if r.Error != "" {
		detail = r.Error
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s", r.Tool, r.Risk, r.Status, latency, detail)
}

func summaryLine(results []Result) string {
	counts := map[Status]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	order := []Status{StatusOK, StatusDegraded, StatusFail, StatusMissing, StatusUnconfigured, StatusSkipped}
	parts := make([]string, 0, len(order))
	for _, s := range order {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	return strings.Join(parts, " · ")
}

// HasFailures reports whether the report contains any fail or missing
// result — the CLI's exit-code signal.
func HasFailures(report *Report) bool {
	for _, r := range report.Results {
		if r.Status == StatusFail || r.Status == StatusMissing {
			return true
		}
	}
	return false
}

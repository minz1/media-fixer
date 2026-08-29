// Command media-fixer-check drives the real agent dispatcher against the
// real configured services and reports, per tool, whether it actually
// works — for local dev iteration and CI, without waiting for an incident
// to surface a broken tool.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/minz1/mediafixer/internal/clientset"
	"github.com/minz1/mediafixer/internal/config"
	"github.com/minz1/mediafixer/internal/livecheck"
)

func main() {
	os.Exit(run())
}

const exitFail = 1

func run() int {
	cfgPath := flag.String("config", "/etc/media-fixer/config.toml", "path to TOML config file")
	allowWrite := flag.Bool(
		"write", false, "also run safe, idempotent write actions (rescans, cache cleanup, repair sweeps)",
	)
	allowDisruptive := flag.Bool("disruptive", false, "also run disruptive actions (service restarts, library scans)")
	asJSON := flag.Bool("json", false, "emit the report as JSON instead of a text table")
	only := flag.String("only", "", "comma-separated list of tool names to check (default: all)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return exitFail
	}

	clients, err := clientset.Build(cfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build clients: %v\n", err)
		return exitFail
	}
	disp := clients.Dispatcher(nil, nil)

	opts := livecheck.Options{
		AllowWrite:       *allowWrite,
		AllowDisruptive:  *allowDisruptive,
		Only:             splitCommaList(*only),
		FixtureOverrides: fixturesFromConfig(cfg),
	}

	report := livecheck.New(disp, opts).Run(context.Background())

	if *asJSON {
		if jsonErr := livecheck.RenderJSON(os.Stdout, report); jsonErr != nil {
			fmt.Fprintf(os.Stderr, "render json: %v\n", jsonErr)
			return exitFail
		}
	} else {
		livecheck.RenderText(os.Stdout, report)
	}

	if livecheck.HasFailures(report) {
		return exitFail
	}
	return 0
}

func fixturesFromConfig(cfg *config.Config) livecheck.Fixtures {
	return livecheck.Fixtures{
		JellyfinItemID: cfg.SelfTest.JellyfinItemID,
		SeriesTitle:    cfg.SelfTest.SeriesTitle,
		MovieTitle:     cfg.SelfTest.MovieTitle,
		TorrentName:    cfg.SelfTest.TorrentName,
		SamplePath:     cfg.SelfTest.SamplePath,
	}
}

func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

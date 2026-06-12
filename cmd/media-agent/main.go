package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/minz1/mediafixer/internal/mediaagent"
)

type mountsFlag []string

func (f *mountsFlag) String() string  { return "" }
func (f *mountsFlag) Set(v string) error { *f = append(*f, v); return nil }

func main() {
	addr   := flag.String("addr", ":9191", "listen address")
	apiKey := flag.String("api-key", "", "bearer token clients must present")
	var mounts mountsFlag
	flag.Var(&mounts, "disk-mount", "mount point to include in GET /disk (repeatable; defaults to built-in list if omitted)")
	flag.Parse()

	if *apiKey == "" {
		if v := os.Getenv("MEDIA_AGENT_API_KEY"); v != "" {
			*apiKey = v
		} else {
			slog.Error("--api-key or MEDIA_AGENT_API_KEY is required")
			os.Exit(1)
		}
	}

	if len(mounts) > 0 {
		mediaagent.DiskMounts = []string(mounts)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	h := mediaagent.NewHandler(&mediaagent.RealOps{}, *apiKey, log)

	log.Info("media-agent listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, h); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

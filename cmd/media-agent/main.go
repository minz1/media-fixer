package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/minz1/mediafixer/internal/mediaagent"
)

func main() {
	addr   := flag.String("addr", ":9191", "listen address")
	apiKey := flag.String("api-key", "", "bearer token clients must present")
	flag.Parse()

	if *apiKey == "" {
		if v := os.Getenv("MEDIA_AGENT_API_KEY"); v != "" {
			*apiKey = v
		} else {
			slog.Error("--api-key or MEDIA_AGENT_API_KEY is required")
			os.Exit(1)
		}
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	h := mediaagent.NewHandler(&mediaagent.RealOps{}, *apiKey, log)

	log.Info("media-agent listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, h); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

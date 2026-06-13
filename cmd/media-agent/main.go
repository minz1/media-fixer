package main

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/minz1/mediafixer/internal/mediaagent"
)

const agentReadHeaderTimeout = 10 * time.Second

type mountsFlag []string

func (f *mountsFlag) String() string     { return "" }
func (f *mountsFlag) Set(v string) error { *f = append(*f, v); return nil }

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":9191", "listen address")
	apiKey := flag.String("api-key", "", "bearer token clients must present")
	var mounts mountsFlag
	flag.Var(
		&mounts,
		"disk-mount",
		"mount point to include in GET /disk (repeatable; defaults to built-in list if omitted)",
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if *apiKey == "" {
		if v := os.Getenv("MEDIA_AGENT_API_KEY"); v != "" {
			*apiKey = v
		} else {
			log.Error("--api-key or MEDIA_AGENT_API_KEY is required")
			return errors.New("api key required")
		}
	}

	ops := mediaagent.NewRealOps([]string(mounts))
	h := mediaagent.NewHandler(ops, *apiKey, log)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           h,
		ReadHeaderTimeout: agentReadHeaderTimeout,
	}

	log.Info("media-agent listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "error", err)
		return err
	}
	return nil
}

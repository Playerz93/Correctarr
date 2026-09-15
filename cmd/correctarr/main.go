// Correctarr checks that every file Sonarr and Radarr own is really on disk,
// readable, and present in Plex, and can trigger fixes.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/playerz93/correctarr/internal/db"
	"github.com/playerz93/correctarr/internal/sweep"
	"github.com/playerz93/correctarr/internal/web"
)

var version = "dev"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", envOr("CORRECTARR_LISTEN", ":8585"), "address to serve the GUI on")
	dataDir := flag.String("data", envOr("CORRECTARR_DATA", "/config"), "directory for the SQLite database")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		logger.Fatalf("data dir: %v", err)
	}
	store, err := db.Open(filepath.Join(*dataDir, "correctarr.db"))
	if err != nil {
		logger.Fatalf("open database: %v", err)
	}
	defer store.Close()
	if err := store.MarkStaleRuns(); err != nil {
		logger.Printf("mark stale runs: %v", err)
	}
	seedFromEnv(store, logger)

	svc := sweep.New(store, logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.RunScheduler(ctx)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           web.New(store, svc, logger, version),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Printf("correctarr %s listening on %s (data in %s)", version, *listen, *dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("http: %v", err)
		}
	}()
	<-ctx.Done()
	logger.Printf("shutting down")
	svc.Cancel()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

// seedFromEnv fills empty Plex settings from the environment on first start
// so an Unraid template can pre-populate them. GUI values always win.
func seedFromEnv(store *db.DB, logger *log.Logger) {
	s, err := store.GetSettings()
	if err != nil {
		return
	}
	changed := false
	if s.PlexURL == "" && os.Getenv("PLEX_URL") != "" {
		s.PlexURL = os.Getenv("PLEX_URL")
		changed = true
	}
	if s.PlexToken == "" && os.Getenv("PLEX_TOKEN") != "" {
		s.PlexToken = os.Getenv("PLEX_TOKEN")
		changed = true
	}
	if changed {
		if err := store.SaveSettings(s); err != nil {
			logger.Printf("seed settings: %v", err)
		} else {
			logger.Printf("seeded Plex settings from environment")
		}
	}
}

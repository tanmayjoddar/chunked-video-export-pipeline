package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/example/chunked-export/internal/api"
	"github.com/example/chunked-export/internal/config"
	"github.com/example/chunked-export/internal/jobs"
	"github.com/example/chunked-export/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: server <api|worker|migrate>")
		os.Exit(2)
	}
	mode := os.Args[1]
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		// api+worker boot together; a transient DDL race is possible — retry once.
		time.Sleep(2 * time.Second)
		if err := store.Migrate(ctx, pool); err != nil {
			slog.Error("migrate app", "err", err)
			os.Exit(1)
		}
	}
	// River migrations (river tables). Docs: pkg.go.dev/github.com/riverqueue/river/rivermigrate
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		slog.Error("river migrator", "err", err)
		os.Exit(1)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		slog.Error("river migrate", "err", err)
		os.Exit(1)
	}
	if mode == "migrate" {
		slog.Info("migrations done")
		return
	}

	workers := river.NewWorkers()
	deps := &jobs.Deps{Config: cfg, Store: store.New(pool)}
	river.AddWorker(workers, &jobs.ProxyWorker{Deps: deps})
	river.AddWorker(workers, &jobs.PlanWorker{Deps: deps})
	river.AddWorker(workers, &jobs.ChunkWorker{Deps: deps})
	river.AddWorker(workers, &jobs.StitchWorker{Deps: deps})

	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.RiverMaxWorkers},
		},
		Workers: workers,
		// Floor for jobs without a per-worker Timeout (all of ours define one).
		// Must stay < RescueStuckJobsAfter (river validates this).
		JobTimeout:           30 * time.Second,
		RescueStuckJobsAfter: time.Duration(cfg.RescueStuckAfterS) * time.Second,
	})
	if err != nil {
		slog.Error("river client", "err", err)
		os.Exit(1)
	}
	deps.Client = riverClient

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch mode {
	case "api":
		srv, err := api.New(cfg, store.New(pool), riverClient)
		if err != nil {
			slog.Error("api init", "err", err)
			os.Exit(1)
		}
		// api process also runs river client so tusd hook can enqueue without a worker present (no workers started here though).
		httpSrv := &http.Server{
			Addr:              ":" + cfg.Port,
			Handler:           srv.Mux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      10 * time.Minute, // SSE
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			slog.Info("api listen", "port", cfg.Port)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("listen", "err", err)
				os.Exit(1)
			}
		}()
		<-sigCtx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
		slog.Info("api shutdown")
	case "worker":
		if err := riverClient.Start(sigCtx); err != nil {
			slog.Error("river start", "err", err)
			os.Exit(1)
		}
		slog.Info("worker started")
		// stuck-chunk watchdog: release heartbeats older than rescue window so another worker picks them up
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-sigCtx.Done():
					return
				case <-t.C:
					n, err := store.New(pool).ReleaseStuckChunks(context.Background(), cfg.RescueStuckAfterS)
					if err == nil && n > 0 {
						slog.Info("released stuck chunks", "n", n)
					}
				}
			}
		}()
		<-sigCtx.Done()
		if err := riverClient.Stop(context.Background()); err != nil {
			slog.Error("river stop", "err", err)
		}
		slog.Info("worker shutdown")
	default:
		fmt.Fprintln(os.Stderr, "unknown mode: api|worker|migrate")
		os.Exit(2)
	}
}

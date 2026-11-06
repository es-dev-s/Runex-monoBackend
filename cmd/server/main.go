package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"deploy_platform/internal/api"
	"deploy_platform/internal/cache"
	"deploy_platform/internal/config"
	"deploy_platform/internal/db"
	"deploy_platform/internal/deploy"
	"deploy_platform/internal/detect"
	"deploy_platform/internal/logs"
	"deploy_platform/internal/runtime"
	"deploy_platform/internal/store"
	"deploy_platform/internal/usage"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()

	// Misconfiguration must be loud. In production a wrong value aborts the
	// boot instead of degrading onto a different database or a dead GitHub App,
	// because both failures are invisible until a user's deploy breaks.
	if problems := cfg.Validate(); len(problems) > 0 {
		for _, problem := range problems {
			if cfg.Production() {
				log.Error("config", "err", problem.Error())
			} else {
				log.Warn("config", "err", problem.Error())
			}
		}
		if cfg.Production() {
			log.Error("refusing to start with an invalid production configuration")
			os.Exit(1)
		}
	}

	if err := api.WorkDir(cfg); err != nil {
		log.Error("workdir", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil && cfg.Production() {
		// Falling back here would quietly serve an empty control plane and
		// orphan every existing deployment.
		log.Error("postgres unreachable; refusing to fall back in production", "err", err)
		os.Exit(1)
	}
	if err != nil && cfg.DatabaseURL != config.IsolatedDatabaseURL {
		log.Warn("configured postgres unreachable; using isolated control-plane database", "err", err)
		pool, err = db.Connect(ctx, config.IsolatedDatabaseURL)
	}
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	st := store.New(pool)
	// The cache is non-authoritative (GitHub metadata only), so degrading to
	// the in-process map is acceptable even in production.
	ch, err := cache.Open(ctx, cfg.RedisURL)
	if err != nil && cfg.RedisURL != config.IsolatedRedisURL {
		log.Warn("configured redis unreachable; using isolated control-plane cache", "err", err)
		ch, err = cache.Open(ctx, config.IsolatedRedisURL)
	}
	if err != nil {
		log.Error("redis", "err", err)
		os.Exit(1)
	}
	defer ch.Close()

	hub := logs.NewHub()
	docker := runtime.New()
	engine := deploy.New(cfg, st, docker, detect.New(cfg.DetectorURL), hub)
	sampler := usage.New(docker, st)
	sampler.SetOverLimit(engine.StopOverLimit)
	go func() {
		recoverCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		engine.Recover(recoverCtx)
		cancel()
		engine.Watch(ctx)
	}()
	go sampler.Run(ctx)
	apiSrv := api.New(cfg, st, engine, hub, ch, sampler)
	go apiSrv.WatchDomains(ctx)
	go apiSrv.WatchTrials(ctx)
	server := api.NewHTTPServer(cfg, apiSrv.Handler())
	log.Info("data stores", "postgres", "ok", "cache", ch.Backend())

	errCh := make(chan error, 1)
	go func() {
		log.Info("platform listening", "addr", cfg.Addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	case err := <-errCh:
		if err != nil {
			log.Error("server exited", "err", err)
			os.Exit(1)
		}
	}
}

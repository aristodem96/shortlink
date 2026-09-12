// Command api runs the shortlink HTTP service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"shortlink/internal/analytics"
	"shortlink/internal/config"
	"shortlink/internal/observability"
	"shortlink/internal/service"
	"shortlink/internal/shortid"
	"shortlink/internal/storage/postgres"
	"shortlink/internal/storage/rediscache"
	"shortlink/internal/transport/httpapi"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// main does nothing but translate a failure into an exit code, so that
	// every deferred cleanup in run actually runs -- os.Exit skips defers.
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	log, err := observability.NewLogger(cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}
	slog.SetDefault(log)

	// The root context is cancelled by SIGINT or SIGTERM, which is what starts
	// the graceful shutdown sequence below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting shortlink", slog.String("version", version), slog.String("addr", cfg.HTTP.Addr))

	// --- infrastructure ------------------------------------------------------

	startupCtx, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStartup()

	if err := postgres.Migrate(startupCtx, cfg.Postgres.DSN, log); err != nil {
		return err
	}

	pool, err := postgres.NewPool(startupCtx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Warn("could not close redis client", slog.Any("error", err))
		}
	}()

	if err := rdb.Ping(startupCtx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}

	// --- application ---------------------------------------------------------

	linkRepo := postgres.NewLinkRepository(pool)
	keyRepo := postgres.NewAPIKeyRepository(pool)
	clickRepo := postgres.NewClickRepository(pool)

	collector := analytics.NewCollector(clickRepo, analytics.CollectorConfig{
		BufferSize:    cfg.Clicks.BufferSize,
		Workers:       cfg.Clicks.Workers,
		BatchSize:     cfg.Clicks.BatchSize,
		FlushInterval: cfg.Clicks.FlushInterval,
	}, log)
	collector.Start()

	codes, err := shortid.NewGenerator(cfg.CodeLength)
	if err != nil {
		return err
	}

	links := service.NewLinkService(
		linkRepo,
		rediscache.NewLinkCache(rdb),
		collector,
		codes,
		analytics.NewIPHasher(cfg.IPHashSalt),
		service.LinkServiceConfig{
			CacheTTL:         cfg.Cache.TTL,
			NegativeCacheTTL: cfg.Cache.NegativeTTL,
		},
		log,
	)

	router := httpapi.NewRouter(httpapi.Deps{
		Links:   links,
		Auth:    service.NewAuthenticator(keyRepo),
		Limiter: rediscache.NewLimiter(rdb, cfg.Limiter.RPS, cfg.Limiter.Burst, log),
		ReadinessChecks: []httpapi.ReadinessCheck{
			{Name: "postgres", Probe: pool.Ping},
			{Name: "redis", Probe: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
		},
		Logger:            log,
		BaseURL:           cfg.BaseURL,
		Version:           version,
		TrustProxyHeaders: cfg.TrustProxyHeaders,
	})

	server := &http.Server{
		Addr:         cfg.HTTP.Addr,
		Handler:      router,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  cfg.HTTP.IdleTimeout,
		// Without this the request line and headers are covered only by
		// ReadTimeout, which a slowloris client can exploit on a keep-alive
		// connection.
		ReadHeaderTimeout: 3 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	// --- run and shut down ---------------------------------------------------

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		// Bind first, then announce. ListenAndServe would have this the other
		// way round: the log would claim the server is listening a moment
		// before the bind fails, which reads as a contradiction in the logs of
		// the one situation where they matter most.
		//
		// ListenConfig rather than net.Listen so the bind honours ctx: a
		// shutdown signal during startup aborts it instead of being noticed
		// only once the server is already up.
		var lc net.ListenConfig
		listener, err := lc.Listen(ctx, "tcp", server.Addr)
		if err != nil {
			return fmt.Errorf("http server: listen on %s: %w", server.Addr, err)
		}

		// The resolved address, so a port of :0 reports the one actually taken.
		log.Info("http server listening", slog.String("addr", listener.Addr().String()))

		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

	group.Go(func() error {
		<-groupCtx.Done()
		log.Info("shutdown signal received, draining")

		// A fresh context: the one that was just cancelled cannot be used to
		// bound the work that the cancellation started.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.HTTP.ShutdownTimeout)
		defer cancel()

		// Order matters: stop accepting first, so no new clicks are produced,
		// then drain the ones already buffered.
		var shutdownErr error
		if err := server.Shutdown(shutdownCtx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("drain http server: %w", err))
		}
		if err := collector.Close(shutdownCtx); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
		return shutdownErr
	})

	if err := group.Wait(); err != nil {
		return err
	}

	log.Info("shutdown complete")
	return nil
}

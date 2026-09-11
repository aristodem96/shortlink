// Command bot runs the Telegram bot that sells API access, together with the
// payment webhook and the reconciler that backs it up.
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

	// The runtime image is distroless and ships no time zone database, so
	// time.LoadLocation("Europe/Kyiv") would fail there -- and only there, never
	// on a developer's machine. Embedding it costs about 450 KB.
	_ "time/tzdata"

	"golang.org/x/sync/errgroup"

	"github.com/aristodem96/shortlink/internal/billing"
	"github.com/aristodem96/shortlink/internal/bot"
	"github.com/aristodem96/shortlink/internal/config"
	"github.com/aristodem96/shortlink/internal/cryptopay"
	"github.com/aristodem96/shortlink/internal/observability"
	"github.com/aristodem96/shortlink/internal/storage/postgres"
	"github.com/aristodem96/shortlink/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadBot()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	log, err := observability.NewLogger(cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("load time zone %q: %w", cfg.Timezone, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startupCtx, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStartup()

	// The API applies migrations too; goose holds an advisory lock, so the two
	// processes starting together cannot race each other.
	if err := postgres.Migrate(startupCtx, cfg.Postgres.DSN, log); err != nil {
		return err
	}

	pool, err := postgres.NewPool(startupCtx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	tg := telegram.New(cfg.TelegramToken)
	pay := cryptopay.New(cfg.CryptoPayURL, cfg.CryptoPayToken)

	svc := billing.New(
		postgres.NewBillingRepository(pool),
		pay,
		bot.NewNotifier(tg, loc),
		billing.Config{
			PriceUSD:       cfg.PriceUSD,
			AccessDays:     cfg.AccessDays,
			MaxKeys:        cfg.MaxKeys,
			AcceptedAssets: cfg.AcceptedAssets,
			InvoiceTTL:     cfg.InvoiceTTL,
		},
		log,
	)

	b := bot.New(tg, tg, svc, cfg.APIBaseURL, loc, log)

	// The menu is a convenience; a bot that cannot set it still works.
	if err := tg.SetCommands(startupCtx, bot.Commands); err != nil {
		log.Warn("could not register the command menu", slog.Any("error", err))
	}

	mux := http.NewServeMux()
	mux.Handle("/webhooks/cryptopay", svc.WebhookHandler(cfg.CryptoPayToken))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
	}

	// Said loudly, because it is the difference between test coins and money.
	log.Info("starting bot",
		slog.String("network", cfg.Network),
		slog.String("price_usd", cfg.PriceUSD),
		slog.Int("access_days", cfg.AccessDays),
		slog.Int("max_keys", cfg.MaxKeys))

	group, gctx := errgroup.WithContext(ctx)

	group.Go(func() error { return b.Run(gctx) })

	group.Go(func() error {
		ticker := time.NewTicker(cfg.ReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-ticker.C:
				if err := svc.Reconcile(gctx); err != nil && gctx.Err() == nil {
					log.Warn("reconciliation failed", slog.Any("error", err))
				}
			}
		}
	})

	group.Go(func() error {
		var lc net.ListenConfig
		listener, err := lc.Listen(gctx, "tcp", server.Addr)
		if err != nil {
			return fmt.Errorf("webhook server: listen on %s: %w", server.Addr, err)
		}
		log.Info("webhook server listening", slog.String("addr", listener.Addr().String()))
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("webhook server: %w", err)
		}
		return nil
	})

	group.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	})

	if err := group.Wait(); err != nil {
		return err
	}
	log.Info("bot stopped")
	return nil
}

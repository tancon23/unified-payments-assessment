// Command worker finishes what requests and webhooks left open: it retries
// provider calls that got no answer, captures approved PayPal orders, and
// asks providers about checkouts and refunds whose webhook never came.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/tancon23/unified-payments-assessment/internal/app"
	"github.com/tancon23/unified-payments-assessment/internal/postgres"
)

func main() {
	log := app.Logger()
	if err := run(log); err != nil {
		log.Error("worker stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := app.LoadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := app.OpenDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool); err != nil {
		return err
	}
	providers, err := cfg.Providers(log)
	if err != nil {
		return err
	}

	log.Info("worker started", "interval", cfg.WorkerInterval, "providers", providers.Names())
	// RunWorker returns once the current pass ends after a shutdown signal,
	// before the pool closes.
	cfg.Service(postgres.New(pool), providers, log).RunWorker(ctx, cfg.WorkerInterval)
	log.Info("worker stopped")
	return nil
}

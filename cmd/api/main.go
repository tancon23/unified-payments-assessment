// Command api runs the payments gateway HTTP API.
//
//	api           apply migrations, then serve the API
//	api migrate   apply migrations and exit
//	api seed      create or update the acme and globex tenants and print new API keys
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/tanweerdev/unified-payments-assessment/internal/app"
	"github.com/tanweerdev/unified-payments-assessment/internal/httpapi"
	"github.com/tanweerdev/unified-payments-assessment/internal/postgres"
)

func main() {
	log := app.Logger()
	if err := run(log, os.Args[1:]); err != nil {
		log.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, args []string) error {
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
	store := postgres.New(pool)

	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "":
		return serve(ctx, log, cfg, store)
	case "migrate":
		log.Info("migrations applied")
		return nil
	case "seed":
		return seed(ctx, store)
	default:
		return fmt.Errorf("unknown command %q: want migrate or seed", command)
	}
}

func serve(ctx context.Context, log *slog.Logger, cfg app.Config, store *postgres.Store) error {
	providers, err := cfg.Providers(log)
	if err != nil {
		return err
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewRouter(httpapi.Deps{
			Service:  cfg.Service(store, providers, log),
			Tenants:  store,
			Metrics:  httpapi.NewMetrics(reg),
			Gatherer: reg,
			Log:      log,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("api listening", "addr", cfg.HTTPAddr, "providers", providers.Names())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// seed creates the two example tenants, or gives them new keys, and prints
// the keys. Only their SHA-256 hashes are stored.
func seed(ctx context.Context, store *postgres.Store) error {
	fmt.Println("tenant  api key")
	for _, tenant := range []string{"acme", "globex"} {
		b := make([]byte, 24)
		rand.Read(b)
		key := tenant + "_" + hex.EncodeToString(b)
		if err := store.UpsertTenant(ctx, tenant, tenant, key); err != nil {
			return fmt.Errorf("seeding %s: %w", tenant, err)
		}
		fmt.Printf("%-7s %s\n", tenant, key)
	}
	return nil
}

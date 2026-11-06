// Package app holds the configuration and wiring shared by the api and
// worker commands.
package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
	"github.com/tancon23/unified-payments-assessment/internal/payment"
	"github.com/tancon23/unified-payments-assessment/internal/provider/paypal"
	"github.com/tancon23/unified-payments-assessment/internal/provider/stripe"
)

// Config is read from environment variables.
type Config struct {
	DatabaseURL    string
	HTTPAddr       string
	WorkerInterval time.Duration
	// FirstCheck overrides how long the worker waits before asking a
	// provider about a checkout or refund whose webhook has not arrived.
	FirstCheck time.Duration
	Stripe     stripe.Config
	PayPal     paypal.Config
}

func LoadConfig() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		HTTPAddr:    cmp.Or(os.Getenv("HTTP_ADDR"), ":8080"),
		Stripe: stripe.Config{
			APIBase:       cmp.Or(os.Getenv("STRIPE_API_BASE"), "https://api.stripe.com"),
			SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
			WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		},
		PayPal: paypal.Config{
			APIBase:      cmp.Or(os.Getenv("PAYPAL_API_BASE"), "https://api-m.sandbox.paypal.com"),
			ClientID:     os.Getenv("PAYPAL_CLIENT_ID"),
			ClientSecret: os.Getenv("PAYPAL_CLIENT_SECRET"),
			WebhookID:    os.Getenv("PAYPAL_WEBHOOK_ID"),
			PlanID:       os.Getenv("PAYPAL_PLAN_ID"),
		},
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	var err error
	if cfg.WorkerInterval, err = durationEnv("WORKER_INTERVAL", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WorkerInterval == 0 {
		return Config{}, errors.New("WORKER_INTERVAL must be positive")
	}
	if cfg.FirstCheck, err = durationEnv("WORKER_FIRST_CHECK", 0); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return d, nil
}

// Providers registers every provider that has credentials. A new provider
// is one more adapter package and one more block here.
func (c Config) Providers(log *slog.Logger) (*gateway.Registry, error) {
	client := &http.Client{} // each call is bounded by the service's CallTimeout
	providers := gateway.NewRegistry()

	if c.Stripe.SecretKey != "" {
		if c.Stripe.WebhookSecret == "" {
			return nil, errors.New("STRIPE_WEBHOOK_SECRET is required with STRIPE_SECRET_KEY")
		}
		s := c.Stripe
		s.HTTPClient = client
		providers.Register(stripe.Name, stripe.New(s))
	}

	if c.PayPal.ClientID != "" {
		if c.PayPal.ClientSecret == "" {
			return nil, errors.New("PAYPAL_CLIENT_SECRET is required with PAYPAL_CLIENT_ID")
		}
		if c.PayPal.WebhookID == "" {
			log.Warn("PAYPAL_WEBHOOK_ID is not set: PayPal webhooks are rejected and the worker discovers outcomes instead")
		}
		if c.PayPal.PlanID == "" {
			log.Warn("PAYPAL_PLAN_ID is not set: PayPal subscriptions are refused")
		}
		p := c.PayPal
		p.HTTPClient = client
		providers.Register(paypal.Name, paypal.New(p))
	}

	if len(providers.Names()) == 0 {
		return nil, errors.New("no payment provider configured: set STRIPE_SECRET_KEY and/or PAYPAL_CLIENT_ID")
	}
	return providers, nil
}

// Service builds the payment service.
func (c Config) Service(store payment.Store, providers *gateway.Registry, log *slog.Logger) *payment.Service {
	cfg := payment.DefaultConfig()
	if c.FirstCheck > 0 {
		cfg.FirstCheck = c.FirstCheck
	}
	return payment.NewService(store, providers, cfg, log)
}

// OpenDB connects to PostgreSQL and checks the connection.
func OpenDB(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	return pool, nil
}

// Logger writes JSON logs to standard output.
func Logger() *slog.Logger { return slog.New(slog.NewJSONHandler(os.Stdout, nil)) }

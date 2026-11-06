// Command sandbox serves a local stand-in for the Stripe and PayPal sandbox
// APIs. See package sandbox for what it covers.
package main

import (
	"cmp"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/sandbox"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	addr := cmp.Or(os.Getenv("HTTP_ADDR"), ":8090")
	srv := sandbox.New(sandbox.Config{
		PublicURL:           cmp.Or(os.Getenv("PUBLIC_URL"), "http://localhost:8090"),
		StripeWebhookURL:    cmp.Or(os.Getenv("STRIPE_WEBHOOK_URL"), "http://localhost:8080/v1/webhooks/stripe"),
		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		PayPalWebhookURL:    cmp.Or(os.Getenv("PAYPAL_WEBHOOK_URL"), "http://localhost:8080/v1/webhooks/paypal"),
		PayPalWebhookID:     os.Getenv("PAYPAL_WEBHOOK_ID"),
		PayPalPlanID:        os.Getenv("PAYPAL_PLAN_ID"),
		Log:                 log,
	})
	log.Info("sandbox listening", "addr", addr)
	server := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		log.Error("sandbox stopped", "err", err)
		os.Exit(1)
	}
}

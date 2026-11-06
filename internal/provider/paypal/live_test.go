package paypal_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/paypal"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/providertest"
)

// The Live tests call the PayPal sandbox with PAYPAL_CLIENT_ID and
// PAYPAL_CLIENT_SECRET. They are skipped without them; `make test-live`
// loads them from .env.

const liveAPI = "https://api-m.sandbox.paypal.com"

func liveProvider(t *testing.T) *paypal.Provider {
	t.Helper()
	id, secret := os.Getenv("PAYPAL_CLIENT_ID"), os.Getenv("PAYPAL_CLIENT_SECRET")
	if id == "" || secret == "" {
		t.Skip("PAYPAL_CLIENT_ID and PAYPAL_CLIENT_SECRET are not set")
	}
	return paypal.New(paypal.Config{
		APIBase: liveAPI, ClientID: id, ClientSecret: secret,
		WebhookID: os.Getenv("PAYPAL_WEBHOOK_ID"), PlanID: os.Getenv("PAYPAL_PLAN_ID"),
	})
}

func TestLiveOrder(t *testing.T) {
	ctx := context.Background()
	p := liveProvider(t)
	params := providertest.Params(gateway.ModePayment)

	co, err := p.CreateCheckout(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if co.Ref == "" || !strings.Contains(co.URL, "sandbox.paypal.com") {
		t.Fatalf("checkout = %+v", co)
	}
	t.Logf("open %s and approve with a sandbox personal account to see the flow", co.URL)

	again, err := p.CreateCheckout(ctx, params)
	if err != nil || again.Ref != co.Ref {
		t.Fatalf("same request id returned %+v, %v; want %s", again, err, co.Ref)
	}

	st, err := p.GetCheckout(ctx, co.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != gateway.CheckoutOpen || st.Amount != params.Amount || st.Currency != params.Currency {
		t.Fatalf("state = %+v", st)
	}

	_, err = p.Capture(ctx, gateway.CaptureParams{IdempotencyKey: "capture-" + params.PaymentID, CheckoutRef: co.Ref})
	if !gateway.IsRejected(err) {
		t.Fatalf("capture before approval: want RejectedError, got %v", err)
	}
	t.Logf("capture before approval rejected as expected: %v", err)
}

func TestLiveSubscription(t *testing.T) {
	ctx := context.Background()
	p := liveProvider(t)
	if os.Getenv("PAYPAL_PLAN_ID") == "" {
		t.Skip("PAYPAL_PLAN_ID is not set")
	}
	plan, err := p.Plan(ctx)
	if err != nil {
		t.Fatalf("reading the configured plan: %v", err)
	}
	t.Logf("plan %s charges %d %s every %s", plan.ID, plan.Amount, plan.Currency, plan.Interval)

	params := providertest.Params(gateway.ModeSubscription)
	params.Amount, params.Currency, params.Interval = plan.Amount, plan.Currency, plan.Interval
	co, err := p.CreateCheckout(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(co.Ref, "I-") || !strings.Contains(co.URL, "sandbox.paypal.com") {
		t.Fatalf("checkout = %+v", co)
	}
	st, err := p.GetCheckout(ctx, co.Ref)
	if err != nil || st.Status != gateway.CheckoutOpen {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

func TestLiveWebhookVerificationRejectsForgery(t *testing.T) {
	p := liveProvider(t)
	if os.Getenv("PAYPAL_WEBHOOK_ID") == "" {
		t.Skip("PAYPAL_WEBHOOK_ID is not set")
	}
	header := http.Header{
		"Paypal-Auth-Algo":         {"SHA256withRSA"},
		"Paypal-Cert-Url":          {"https://api.sandbox.paypal.com/v1/notifications/certs/CERT-360caa42-fca2a594-a5cafa77"},
		"Paypal-Transmission-Id":   {"69cd13f0-d67a-11e5-baa3-778b53f4ae55"},
		"Paypal-Transmission-Sig":  {"bm90IGEgcmVhbCBzaWduYXR1cmU="},
		"Paypal-Transmission-Time": {"2026-01-01T00:00:00Z"},
	}
	body := []byte(`{"id":"WH-FORGED","event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":"X"}}`)

	_, err := p.VerifyWebhook(context.Background(), header, body)
	if err == nil || errors.Is(err, gateway.ErrTransient) {
		t.Fatalf("forged webhook: want a definitive rejection, got %v", err)
	}
	t.Logf("forged webhook rejected: %v", err)
}

func TestLiveRejectsUnknownCredentials(t *testing.T) {
	liveProvider(t)
	p := paypal.New(paypal.Config{APIBase: liveAPI, ClientID: "not-a-client", ClientSecret: "not-a-secret"})
	if _, err := p.GetCheckout(context.Background(), "MISSING"); !gateway.IsRejected(err) {
		t.Fatalf("want RejectedError, got %v", err)
	}
}

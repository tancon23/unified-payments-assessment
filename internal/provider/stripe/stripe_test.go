package stripe_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/providertest"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/stripe"
	"github.com/tanweerdev/unified-payments-assessment/internal/sandbox"
)

func TestContract(t *testing.T) {
	sb := providertest.NewSandbox(t, sandbox.Config{StripeWebhookSecret: "whsec_test"})
	providertest.Run(t, providertest.Harness{
		Provider: stripe.New(stripe.Config{APIBase: sb.URL, SecretKey: "sk_test_123", WebhookSecret: "whsec_test"}),
		Pay:      sb.Pay,
	})
}

// recorder answers every request with body and keeps the last one it saw.
type recorder struct {
	header http.Header
	form   url.Values
}

func newRecorded(t *testing.T, status int, body string) (*stripe.Provider, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		rec.header, rec.form = r.Header.Clone(), r.PostForm
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return stripe.New(stripe.Config{APIBase: srv.URL, SecretKey: "sk_test_123", WebhookSecret: "whsec_test"}), rec
}

func TestCheckoutRequest(t *testing.T) {
	ctx := context.Background()
	p, rec := newRecorded(t, http.StatusOK, `{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1"}`)

	params := providertest.Params(gateway.ModePayment)
	if _, err := p.CreateCheckout(ctx, params); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"mode":                 "payment",
		"client_reference_id":  params.PaymentID,
		"metadata[payment_id]": params.PaymentID,
		"payment_intent_data[metadata][payment_id]":     params.PaymentID,
		"line_items[0][quantity]":                       "1",
		"line_items[0][price_data][currency]":           "usd",
		"line_items[0][price_data][unit_amount]":        "2500",
		"line_items[0][price_data][product_data][name]": params.Description,
		"success_url": params.SuccessURL,
		"cancel_url":  params.CancelURL,
	}
	for key, value := range want {
		if got := rec.form.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if rec.form.Has("line_items[0][price_data][recurring][interval]") {
		t.Error("a one-off checkout sent a recurring interval")
	}
	for header, value := range map[string]string{
		"Authorization":   "Bearer sk_test_123",
		"Idempotency-Key": params.IdempotencyKey,
		"Stripe-Version":  "2024-06-20",
	} {
		if got := rec.header.Get(header); got != value {
			t.Errorf("header %s = %q, want %q", header, got, value)
		}
	}

	sub := providertest.Params(gateway.ModeSubscription)
	if _, err := p.CreateCheckout(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if got := rec.form.Get("line_items[0][price_data][recurring][interval]"); got != "month" {
		t.Errorf("recurring interval = %q, want month", got)
	}
	if got := rec.form.Get("subscription_data[metadata][payment_id]"); got != sub.PaymentID {
		t.Errorf("subscription_data[metadata][payment_id] = %q, want %q", got, sub.PaymentID)
	}
	if rec.form.Has("payment_intent_data[metadata][payment_id]") {
		t.Error("a subscription checkout sent payment intent data")
	}
}

func TestErrorClassification(t *testing.T) {
	ctx := context.Background()
	params := providertest.Params(gateway.ModePayment)

	// 409: an earlier request with the same idempotency key is still running.
	for _, status := range []int{408, 409, 429, 500, 503} {
		p, _ := newRecorded(t, status, `{"error":{"type":"api_error"}}`)
		if _, err := p.CreateCheckout(ctx, params); !errors.Is(err, gateway.ErrTransient) {
			t.Errorf("HTTP %d: want ErrTransient, got %v", status, err)
		}
	}

	p, _ := newRecorded(t, http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"parameter_missing","message":"Missing required param: currency."}}`)
	_, err := p.CreateCheckout(ctx, params)
	var rejected *gateway.RejectedError
	if !errors.As(err, &rejected) || rejected.Code != "parameter_missing" || rejected.StatusCode != http.StatusBadRequest {
		t.Errorf("HTTP 400: want RejectedError parameter_missing, got %v", err)
	}

	p, _ = newRecorded(t, http.StatusUnauthorized, `{"error":{"type":"invalid_request_error","code":"api_key_invalid"}}`)
	if _, err := p.CreateCheckout(ctx, params); !gateway.IsRejected(err) {
		t.Errorf("HTTP 401: want RejectedError, got %v", err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	p = stripe.New(stripe.Config{APIBase: closed.URL, SecretKey: "sk_test_123"})
	if _, err := p.CreateCheckout(ctx, params); !errors.Is(err, gateway.ErrTransient) {
		t.Errorf("unreachable: want ErrTransient, got %v", err)
	}
}

// Package providertest is the contract every gateway.Provider meets. Each
// adapter package runs Run against its provider, the placeholder Paddle
// adapter included.
package providertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

// Delivery is a webhook as a provider sent it.
type Delivery struct {
	Header http.Header
	Body   []byte
}

// Harness describes the provider under test.
type Harness struct {
	Provider gateway.Provider
	// Placeholder marks an adapter that is not implemented yet. It meets the
	// contract by refusing every call definitively and rejecting every
	// webhook, so nothing can be half-processed through it.
	Placeholder bool
	// Pay plays the customer paying a checkout and returns the webhook the
	// provider sent about it.
	Pay func(t *testing.T, checkoutRef string) Delivery
}

// Amount and currency used for every checkout; subscriptions charge it
// monthly.
const (
	Amount   = 2500
	Currency = "usd"
)

// Params returns parameters for a new checkout with its own payment id.
func Params(mode gateway.Mode) gateway.CheckoutParams {
	b := make([]byte, 8)
	rand.Read(b)
	id := "pay_" + hex.EncodeToString(b)
	p := gateway.CheckoutParams{
		IdempotencyKey: "checkout-" + id,
		PaymentID:      id,
		Mode:           mode,
		Amount:         Amount,
		Currency:       Currency,
		Description:    "Contract test",
		SuccessURL:     "https://shop.test/ok?order=1&step=done",
		CancelURL:      "https://shop.test/cancel",
	}
	if mode == gateway.ModeSubscription {
		p.Interval = gateway.IntervalMonth
	}
	return p
}

// Run checks h.Provider against the contract.
func Run(t *testing.T, h Harness) {
	if h.Placeholder {
		runPlaceholder(t, h.Provider)
		return
	}
	ctx := context.Background()
	p := h.Provider

	t.Run("opens a checkout", func(t *testing.T) {
		co := open(t, p, Params(gateway.ModePayment))
		if co.Ref == "" || co.URL == "" {
			t.Fatalf("checkout = %+v, want a ref and a URL", co)
		}
	})

	t.Run("a repeated idempotency key returns the first checkout", func(t *testing.T) {
		params := Params(gateway.ModePayment)
		first, again := open(t, p, params), open(t, p, params)
		if again.Ref != first.Ref {
			t.Fatalf("same key opened %s and %s", first.Ref, again.Ref)
		}
		if other := open(t, p, Params(gateway.ModePayment)); other.Ref == first.Ref {
			t.Fatal("a new key returned the first checkout")
		}
	})

	t.Run("reports an open checkout with its amount", func(t *testing.T) {
		co := open(t, p, Params(gateway.ModePayment))
		st, err := p.GetCheckout(ctx, co.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != gateway.CheckoutOpen || st.Amount != Amount || st.Currency != Currency {
			t.Fatalf("state = %+v", st)
		}
	})

	t.Run("never reports an unknown checkout as open", func(t *testing.T) {
		st, err := p.GetCheckout(ctx, "does-not-exist")
		if err == nil && st.Status != gateway.CheckoutExpired {
			t.Fatalf("state = %+v, want expired or a rejection", st)
		}
		if err != nil && !gateway.IsRejected(err) {
			t.Fatalf("want a definitive rejection, got %v", err)
		}
	})

	t.Run("opens a subscription checkout", func(t *testing.T) {
		co := open(t, p, Params(gateway.ModeSubscription))
		st, err := p.GetCheckout(ctx, co.Ref)
		if err != nil || st.Status != gateway.CheckoutOpen {
			t.Fatalf("state = %+v, %v", st, err)
		}
	})

	t.Run("a payment webhook verifies and settles the checkout", func(t *testing.T) {
		paid(t, h)
	})

	t.Run("rejects tampered and unsigned webhooks", func(t *testing.T) {
		params := Params(gateway.ModePayment)
		co := open(t, p, params)
		d := h.Pay(t, co.Ref)
		// Point the event at another payment, as a forger would.
		other := Params(gateway.ModePayment).PaymentID
		tampered := bytes.ReplaceAll(d.Body, []byte(params.PaymentID), []byte(other))
		if bytes.Equal(tampered, d.Body) {
			t.Fatal("the webhook does not carry the payment id")
		}
		if _, err := p.VerifyWebhook(ctx, d.Header, tampered); !errors.Is(err, gateway.ErrInvalidSignature) {
			t.Errorf("tampered body: want ErrInvalidSignature, got %v", err)
		}
		if _, err := p.VerifyWebhook(ctx, http.Header{}, d.Body); !errors.Is(err, gateway.ErrInvalidSignature) {
			t.Errorf("no signature headers: want ErrInvalidSignature, got %v", err)
		}
	})

	t.Run("refunds part of a payment, once per key", func(t *testing.T) {
		paymentRef := paid(t, h)
		params := gateway.RefundParams{IdempotencyKey: "refund-re_1", RefundID: "re_1", PaymentRef: paymentRef, Amount: 1000, Currency: Currency}
		r, err := p.Refund(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if r.Ref == "" || (r.Status != gateway.RefundSucceeded && r.Status != gateway.RefundPending) {
			t.Fatalf("refund = %+v", r)
		}
		if again, err := p.Refund(ctx, params); err != nil || again.Ref != r.Ref {
			t.Fatalf("same key refunded again: %+v, %v", again, err)
		}
		if got, err := p.GetRefund(ctx, r.Ref); err != nil || got.Ref != r.Ref {
			t.Fatalf("GetRefund = %+v, %v", got, err)
		}

		over := gateway.RefundParams{IdempotencyKey: "refund-re_2", RefundID: "re_2", PaymentRef: paymentRef, Amount: Amount, Currency: Currency}
		if _, err := p.Refund(ctx, over); !gateway.IsRejected(err) {
			t.Fatalf("refunding more than remains: want a rejection, got %v", err)
		}
	})
}

func open(t *testing.T, p gateway.Provider, params gateway.CheckoutParams) gateway.Checkout {
	t.Helper()
	co, err := p.CreateCheckout(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	return co
}

// paid opens a checkout, has the customer pay it, verifies the webhook,
// captures when the provider needs it, and returns the payment ref.
func paid(t *testing.T, h Harness) string {
	t.Helper()
	ctx := context.Background()
	params := Params(gateway.ModePayment)
	co := open(t, h.Provider, params)

	d := h.Pay(t, co.Ref)
	ev, err := h.Provider.VerifyWebhook(ctx, d.Header, d.Body)
	if err != nil {
		t.Fatalf("genuine webhook rejected: %v", err)
	}
	if ev.PaymentID != params.PaymentID || ev.Amount != Amount || ev.Currency != Currency {
		t.Fatalf("event = %+v, want payment %s for %d %s", ev, params.PaymentID, Amount, Currency)
	}

	switch ev.Kind {
	case gateway.EventCheckoutPaid:
	case gateway.EventCheckoutAuthorized:
		capturer, ok := h.Provider.(gateway.Capturer)
		if !ok {
			t.Fatal("provider reported an authorized checkout but cannot capture it")
		}
		st, err := capturer.Capture(ctx, gateway.CaptureParams{IdempotencyKey: "capture-" + params.PaymentID, CheckoutRef: co.Ref})
		if err != nil || st.Status != gateway.CheckoutPaid {
			t.Fatalf("capture = %+v, %v", st, err)
		}
	default:
		t.Fatalf("event kind = %q, want paid or authorized", ev.Kind)
	}

	st, err := h.Provider.GetCheckout(ctx, co.Ref)
	if err != nil || st.Status != gateway.CheckoutPaid || st.PaymentRef == "" {
		t.Fatalf("state after payment = %+v, %v", st, err)
	}
	return st.PaymentRef
}

func runPlaceholder(t *testing.T, p gateway.Provider) {
	ctx := context.Background()
	calls := map[string]func() error{
		"CreateCheckout": func() error { _, err := p.CreateCheckout(ctx, Params(gateway.ModePayment)); return err },
		"GetCheckout":    func() error { _, err := p.GetCheckout(ctx, "ref"); return err },
		"Refund":         func() error { _, err := p.Refund(ctx, gateway.RefundParams{}); return err },
		"GetRefund":      func() error { _, err := p.GetRefund(ctx, "ref"); return err },
	}
	for name, call := range calls {
		if err := call(); !gateway.IsRejected(err) {
			t.Errorf("%s: want a definitive rejection, got %v", name, err)
		}
	}
	if _, err := p.VerifyWebhook(ctx, http.Header{}, []byte(`{}`)); !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("VerifyWebhook: want ErrInvalidSignature, got %v", err)
	}
}

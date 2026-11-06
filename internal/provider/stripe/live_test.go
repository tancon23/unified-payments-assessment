package stripe_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
	"github.com/tancon23/unified-payments-assessment/internal/provider/providertest"
	"github.com/tancon23/unified-payments-assessment/internal/provider/stripe"
)

// The Live tests call Stripe's test mode with STRIPE_SECRET_KEY. They are
// skipped without it; `make test-live` loads it from .env.

const liveAPI = "https://api.stripe.com"

func liveKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("STRIPE_SECRET_KEY")
	if key == "" {
		t.Skip("STRIPE_SECRET_KEY is not set")
	}
	if !strings.HasPrefix(key, "sk_test_") {
		t.Fatal("STRIPE_SECRET_KEY is not a test-mode key; refusing to run")
	}
	return key
}

func uniqueID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func TestLiveCheckout(t *testing.T) {
	ctx := context.Background()
	p := stripe.New(stripe.Config{APIBase: liveAPI, SecretKey: liveKey(t)})
	params := providertest.Params(gateway.ModePayment)

	co, err := p.CreateCheckout(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(co.Ref, "cs_test_") || !strings.HasPrefix(co.URL, "https://checkout.stripe.com/") {
		t.Fatalf("checkout = %+v", co)
	}
	t.Logf("open %s and pay with card 4242 4242 4242 4242 to see the flow", co.URL)

	again, err := p.CreateCheckout(ctx, params)
	if err != nil || again.Ref != co.Ref {
		t.Fatalf("same idempotency key returned %+v, %v; want %s", again, err, co.Ref)
	}

	st, err := p.GetCheckout(ctx, co.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != gateway.CheckoutOpen || st.Amount != params.Amount || st.Currency != params.Currency {
		t.Fatalf("state = %+v", st)
	}
}

func TestLiveSubscriptionCheckout(t *testing.T) {
	p := stripe.New(stripe.Config{APIBase: liveAPI, SecretKey: liveKey(t)})
	co, err := p.CreateCheckout(context.Background(), providertest.Params(gateway.ModeSubscription))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(co.Ref, "cs_test_") {
		t.Fatalf("checkout = %+v", co)
	}
}

// paidIntent pays a PaymentIntent with Stripe's test card. A Checkout
// Session can only be paid in a browser, so refunds are tested on this.
func paidIntent(t *testing.T, key string, amount string) string {
	t.Helper()
	form := url.Values{
		"amount":                             {amount},
		"currency":                           {"usd"},
		"payment_method":                     {"pm_card_visa"},
		"confirm":                            {"true"},
		"automatic_payment_methods[enabled]": {"true"},
		"automatic_payment_methods[allow_redirects]": {"never"},
	}
	req, _ := http.NewRequest(http.MethodPost, liveAPI+"/v1/payment_intents", strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pi struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pi); err != nil || pi.Status != "succeeded" {
		t.Fatalf("paying test intent: HTTP %d, %+v, %v", resp.StatusCode, pi, err)
	}
	return pi.ID
}

func TestLiveRefund(t *testing.T) {
	ctx := context.Background()
	key := liveKey(t)
	p := stripe.New(stripe.Config{APIBase: liveAPI, SecretKey: key})
	intent := paidIntent(t, key, "2500")

	refundID := uniqueID("re_")
	r, err := p.Refund(ctx, gateway.RefundParams{IdempotencyKey: "refund-" + refundID, RefundID: refundID, PaymentRef: intent, Amount: 1000, Currency: "usd"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != gateway.RefundSucceeded || !strings.HasPrefix(r.Ref, "re_") {
		t.Fatalf("refund = %+v", r)
	}
	if got, err := p.GetRefund(ctx, r.Ref); err != nil || got != r {
		t.Fatalf("GetRefund = %+v, %v; want %+v", got, err, r)
	}

	over := uniqueID("re_")
	_, err = p.Refund(ctx, gateway.RefundParams{IdempotencyKey: "refund-" + over, RefundID: over, PaymentRef: intent, Amount: 2000, Currency: "usd"})
	if !gateway.IsRejected(err) {
		t.Fatalf("over-refund: want RejectedError, got %v", err)
	}
	t.Logf("over-refund rejected as expected: %v", err)
}

func TestLiveRejectsUnknownKey(t *testing.T) {
	liveKey(t)
	p := stripe.New(stripe.Config{APIBase: liveAPI, SecretKey: "sk_test_not_a_real_key"})
	if _, err := p.GetCheckout(context.Background(), "cs_test_missing"); !gateway.IsRejected(err) {
		t.Fatalf("want RejectedError, got %v", err)
	}
}

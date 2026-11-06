package stripe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

const secret = "whsec_test"

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	now := time.Now()
	valid := SignatureHeader(secret, body, now)
	ts := fmt.Sprint(now.Unix())

	cases := []struct {
		name   string
		header string
		body   []byte
		ok     bool
	}{
		{"valid", valid, body, true},
		{"rolled secret: second v1 matches", "t=" + ts + ",v1=" + sign("old", ts, body) + ",v1=" + sign(secret, ts, body), body, true},
		{"wrong secret", SignatureHeader("whsec_other", body, now), body, false},
		{"tampered body", valid, []byte(`{"id":"evt_1","type":"checkout.session.expired"}`), false},
		{"stale timestamp", SignatureHeader(secret, body, now.Add(-10*time.Minute)), body, false},
		{"future timestamp", SignatureHeader(secret, body, now.Add(10*time.Minute)), body, false},
		{"missing header", "", body, false},
		{"no v1", "t=" + ts, body, false},
		{"v0 only", "t=" + ts + ",v0=" + sign(secret, ts, body), body, false},
		{"garbage", "nonsense", body, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := verifySignature(c.header, c.body, secret, now)
			if c.ok && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !c.ok && !errors.Is(err, gateway.ErrInvalidSignature) {
				t.Fatalf("want ErrInvalidSignature, got %v", err)
			}
		})
	}
}

func sessionEvent(typ, status, paymentStatus string) []byte {
	return fmt.Appendf(nil, `{"id":"evt_1","type":%q,"data":{"object":{
		"id":"cs_test_1","mode":"payment","status":%q,"payment_status":%q,
		"payment_intent":"pi_1","amount_total":2500,"currency":"usd","client_reference_id":"pay_1"}}}`,
		typ, status, paymentStatus)
}

func TestParseCheckoutEvents(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want gateway.EventKind
	}{
		{"completed and paid", sessionEvent("checkout.session.completed", "complete", "paid"), gateway.EventCheckoutPaid},
		{"completed, payment still processing", sessionEvent("checkout.session.completed", "complete", "unpaid"), gateway.EventIgnored},
		{"delayed payment succeeded", sessionEvent("checkout.session.async_payment_succeeded", "complete", "paid"), gateway.EventCheckoutPaid},
		{"delayed payment failed", sessionEvent("checkout.session.async_payment_failed", "complete", "unpaid"), gateway.EventCheckoutFailed},
		{"expired", sessionEvent("checkout.session.expired", "expired", "unpaid"), gateway.EventCheckoutExpired},
		{"untracked type", sessionEvent("checkout.session.created", "open", "unpaid"), gateway.EventIgnored},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, err := parseEvent(c.body)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != c.want {
				t.Fatalf("kind = %q, want %q", ev.Kind, c.want)
			}
			if c.want != gateway.EventIgnored {
				want := gateway.Event{ID: "evt_1", Type: ev.Type, Kind: c.want, PaymentID: "pay_1", CheckoutRef: "cs_test_1", PaymentRef: "pi_1", Amount: 2500, Currency: "usd"}
				if ev != want {
					t.Fatalf("event = %+v, want %+v", ev, want)
				}
			}
		})
	}
}

func TestParseRefundEvents(t *testing.T) {
	refundEvent := func(status string) []byte {
		return fmt.Appendf(nil, `{"id":"evt_2","type":"refund.updated","data":{"object":{
			"id":"re_1","status":%q,"amount":1000,"currency":"usd","metadata":{"refund_id":"re_gw"}}}}`, status)
	}
	for status, want := range map[string]gateway.EventKind{
		"succeeded": gateway.EventRefundSucceeded,
		"failed":    gateway.EventRefundFailed,
		"canceled":  gateway.EventRefundFailed,
		"pending":   gateway.EventIgnored,
	} {
		ev, err := parseEvent(refundEvent(status))
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind != want {
			t.Errorf("status %s: kind = %q, want %q", status, ev.Kind, want)
		}
		if want != gateway.EventIgnored && (ev.RefundID != "re_gw" || ev.RefundRef != "re_1" || ev.Amount != 1000) {
			t.Errorf("status %s: event = %+v", status, ev)
		}
	}
}

func TestParseRejectsEventWithoutID(t *testing.T) {
	if _, err := parseEvent([]byte(`{"type":"checkout.session.completed"}`)); err == nil {
		t.Fatal("want error for event without id")
	}
}

func TestParseSubscriptionCheckoutUsesTheSubscriptionAsPaymentRef(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed","data":{"object":{
		"id":"cs_test_1","mode":"subscription","status":"complete","payment_status":"paid",
		"subscription":"sub_1","amount_total":2500,"currency":"usd","client_reference_id":"pay_1"}}}`)
	ev, err := parseEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != gateway.EventCheckoutPaid || ev.PaymentRef != "sub_1" {
		t.Fatalf("event = %+v, want paid with payment ref sub_1", ev)
	}
}

func TestDocumentedSamplesParse(t *testing.T) {
	cases := map[string]gateway.Event{
		"checkout.session.completed.json": {
			ID: "evt_1QXk2bL8u5zR0aS3mN4oP7qR", Type: "checkout.session.completed", Kind: gateway.EventCheckoutPaid,
			PaymentID: "pay_3f2a9c1e7b6d4a8e9f0c1b2a", CheckoutRef: "cs_test_a1B2c3D4e5F6g7H8i9J0kLmNoPqRsTuVwXyZ",
			PaymentRef: "pi_3QXk2aL8u5zR0aS31bC2dE3f", Amount: 2500, Currency: "usd",
		},
		"checkout.session.expired.json": {
			ID: "evt_1QXm7dL8u5zR0aS3pQ8rS1tU", Type: "checkout.session.expired", Kind: gateway.EventCheckoutExpired,
			PaymentID: "pay_8e7d6c5b4a3f2e1d0c9b8a7f", CheckoutRef: "cs_test_z9Y8x7W6v5U4t3S2r1QpOnMlKjIhGfEdCbA",
			Amount: 2500, Currency: "usd",
		},
		"refund.updated.json": {
			ID: "evt_1QXn9fL8u5zR0aS3vW2xY4zA", Type: "refund.updated", Kind: gateway.EventRefundSucceeded,
			RefundID: "re_5c4b3a2f1e0d9c8b7a6f5e4d", RefundRef: "re_3QXk2aL8u5zR0aS30aB1cD2e", Amount: 1000, Currency: "usd",
		},
	}
	for file, want := range cases {
		body, err := os.ReadFile("../../../docs/webhooks/stripe/" + file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseEvent(body)
		if err != nil || got != want {
			t.Errorf("%s: event = %+v, %v; want %+v", file, got, err, want)
		}
	}
}

func TestEmptySigningSecretRejectsEverything(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	p := New(Config{SecretKey: "sk_test_123"})
	header := http.Header{"Stripe-Signature": {SignatureHeader("", body, time.Now())}}
	if _, err := p.VerifyWebhook(context.Background(), header, body); !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Fatalf("a delivery signed with an empty secret was accepted: %v", err)
	}
}

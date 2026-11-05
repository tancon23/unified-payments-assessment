package paypal

import (
	"os"
	"testing"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
)

func TestDocumentedSamplesParse(t *testing.T) {
	cases := map[string]gateway.Event{
		"CHECKOUT.ORDER.APPROVED.json": {
			ID: "WH-7Y7254563A4550640-11V2185806837105M", Type: "CHECKOUT.ORDER.APPROVED", Kind: gateway.EventCheckoutAuthorized,
			PaymentID: "pay_3f2a9c1e7b6d4a8e9f0c1b2a", CheckoutRef: "5O190127TN364715T", Amount: 2500, Currency: "usd",
		},
		"PAYMENT.CAPTURE.COMPLETED.json": {
			ID: "WH-58D329510W468432D-8HN650336L201105X", Type: "PAYMENT.CAPTURE.COMPLETED", Kind: gateway.EventCheckoutPaid,
			PaymentID: "pay_3f2a9c1e7b6d4a8e9f0c1b2a", CheckoutRef: "5O190127TN364715T", PaymentRef: "3C679366HH908993F",
			Amount: 2500, Currency: "usd",
		},
		"PAYMENT.CAPTURE.DENIED.json": {
			ID: "WH-4SW78779LY2325805-07E03580SX1414828", Type: "PAYMENT.CAPTURE.DENIED", Kind: gateway.EventCheckoutFailed,
			PaymentID: "pay_8e7d6c5b4a3f2e1d0c9b8a7f", CheckoutRef: "8MC585209K746392H", PaymentRef: "7NW873794T343360M",
			Amount: 2500, Currency: "usd",
		},
		"PAYMENT.CAPTURE.REFUNDED.json": {
			ID: "WH-1GE84257G0350133W-6RW800890C634293G", Type: "PAYMENT.CAPTURE.REFUNDED", Kind: gateway.EventRefundSucceeded,
			RefundID: "re_5c4b3a2f1e0d9c8b7a6f5e4d", RefundRef: "1JU08902781691411", Amount: 1000, Currency: "usd",
		},
		"BILLING.SUBSCRIPTION.ACTIVATED.json": {
			ID: "WH-77687562XN25889J8-8Y6T55435R66168T6", Type: "BILLING.SUBSCRIPTION.ACTIVATED", Kind: gateway.EventCheckoutPaid,
			PaymentID: "pay_6a5b4c3d2e1f0a9b8c7d6e5f", CheckoutRef: "I-BW452GLLEP1G", PaymentRef: "I-BW452GLLEP1G",
		},
	}
	for file, want := range cases {
		body, err := os.ReadFile("../../../docs/webhooks/paypal/" + file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseEvent(body)
		if err != nil || got != want {
			t.Errorf("%s: event = %+v, %v; want %+v", file, got, err, want)
		}
	}
}

func TestUntrackedEventIsIgnored(t *testing.T) {
	ev, err := parseEvent([]byte(`{"id":"WH-1","event_type":"CUSTOMER.DISPUTE.CREATED","resource":{}}`))
	if err != nil || ev.Kind != gateway.EventIgnored {
		t.Fatalf("event = %+v, %v; want ignored", ev, err)
	}
}

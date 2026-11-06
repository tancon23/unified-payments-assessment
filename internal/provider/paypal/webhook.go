package paypal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

// signatureHeaders are the transmission headers PayPal signs each delivery
// with, keyed by their field in the verify-webhook-signature request.
var signatureHeaders = map[string]string{
	"auth_algo":         "Paypal-Auth-Algo",
	"cert_url":          "Paypal-Cert-Url",
	"transmission_id":   "Paypal-Transmission-Id",
	"transmission_sig":  "Paypal-Transmission-Sig",
	"transmission_time": "Paypal-Transmission-Time",
}

// VerifyWebhook asks PayPal's verify-webhook-signature API whether the
// delivery is genuine. The signature covers the transmission headers, the
// webhook id and a CRC of the body, so neither can be altered or replayed
// to another endpoint.
func (p *Provider) VerifyWebhook(ctx context.Context, h http.Header, body []byte) (gateway.Event, error) {
	if p.cfg.WebhookID == "" {
		return gateway.Event{}, fmt.Errorf("%w: no webhook id configured", gateway.ErrInvalidSignature)
	}
	if !json.Valid(body) {
		return gateway.Event{}, fmt.Errorf("%w: body is not JSON", gateway.ErrInvalidSignature)
	}
	fields := map[string]string{"webhook_id": p.cfg.WebhookID}
	for field, header := range signatureHeaders {
		v := h.Get(header)
		if v == "" {
			return gateway.Event{}, fmt.Errorf("%w: missing %s header", gateway.ErrInvalidSignature, header)
		}
		fields[field] = v
	}

	var res struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := p.call(ctx, http.MethodPost, "/v1/notifications/verify-webhook-signature", "", verifyRequest(fields, body), &res); err != nil {
		return gateway.Event{}, fmt.Errorf("verifying webhook signature: %w", err)
	}
	if res.VerificationStatus != "SUCCESS" {
		return gateway.Event{}, fmt.Errorf("%w: PayPal reported %s", gateway.ErrInvalidSignature, res.VerificationStatus)
	}
	return parseEvent(body)
}

// verifyRequest builds the verification request with the event spliced in
// byte for byte. PayPal requires the event exactly as it was received, and
// re-encoding it would change its whitespace.
func verifyRequest(fields map[string]string, event []byte) []byte {
	head, _ := json.Marshal(fields) // a map of strings always encodes
	out := make([]byte, 0, len(head)+len(event)+len(`,"webhook_event":}`))
	out = append(out, head[:len(head)-1]...)
	out = append(out, `,"webhook_event":`...)
	out = append(out, event...)
	return append(out, '}')
}

type event struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	Resource  json.RawMessage `json:"resource"`
}

func parseEvent(body []byte) (gateway.Event, error) {
	var e event
	if err := json.Unmarshal(body, &e); err != nil {
		return gateway.Event{}, fmt.Errorf("decoding event: %w", err)
	}
	if e.ID == "" {
		return gateway.Event{}, fmt.Errorf("event has no id")
	}
	ev := gateway.Event{ID: e.ID, Type: e.EventType}

	var err error
	switch e.EventType {
	case "CHECKOUT.ORDER.APPROVED":
		var o order
		if err := decodeResource(e, &o); err != nil {
			return gateway.Event{}, err
		}
		ev.Kind = gateway.EventCheckoutAuthorized
		ev.CheckoutRef = o.ID
		if len(o.PurchaseUnits) > 0 {
			ev.PaymentID = o.PurchaseUnits[0].CustomID
			ev.Amount, ev.Currency, err = o.PurchaseUnits[0].Amount.minor()
		}

	case "PAYMENT.CAPTURE.COMPLETED", "PAYMENT.CAPTURE.DENIED", "PAYMENT.CAPTURE.DECLINED":
		var c capture
		if err := decodeResource(e, &c); err != nil {
			return gateway.Event{}, err
		}
		ev.Kind = gateway.EventCheckoutFailed
		if e.EventType == "PAYMENT.CAPTURE.COMPLETED" {
			ev.Kind = gateway.EventCheckoutPaid
		}
		ev.PaymentID = c.CustomID
		ev.CheckoutRef = c.SupplementaryData.RelatedIDs.OrderID
		ev.PaymentRef = c.ID
		ev.Amount, ev.Currency, err = c.Amount.minor()

	case "BILLING.SUBSCRIPTION.ACTIVATED", "BILLING.SUBSCRIPTION.CANCELLED", "BILLING.SUBSCRIPTION.EXPIRED":
		var s subscription
		if err := decodeResource(e, &s); err != nil {
			return gateway.Event{}, err
		}
		ev.Kind = gateway.EventCheckoutExpired
		if e.EventType == "BILLING.SUBSCRIPTION.ACTIVATED" {
			ev.Kind = gateway.EventCheckoutPaid
		}
		ev.PaymentID = s.CustomID
		ev.CheckoutRef = s.ID
		ev.PaymentRef = s.ID

	case "PAYMENT.CAPTURE.REFUNDED":
		var r refund
		if err := decodeResource(e, &r); err != nil {
			return gateway.Event{}, err
		}
		ev.Kind = gateway.EventRefundSucceeded
		ev.RefundID = r.CustomID
		ev.RefundRef = r.ID
		ev.Amount, ev.Currency, err = r.Amount.minor()
	}
	if err != nil {
		return gateway.Event{}, fmt.Errorf("decoding %s amount: %w", e.EventType, err)
	}
	return ev, nil
}

func decodeResource(e event, into any) error {
	if err := json.Unmarshal(e.Resource, into); err != nil {
		return fmt.Errorf("decoding %s resource: %w", e.EventType, err)
	}
	return nil
}

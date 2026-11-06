package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

// signatureTolerance bounds how old a signed delivery may be, which stops a
// captured request from being replayed later.
const signatureTolerance = 5 * time.Minute

func (p *Provider) VerifyWebhook(_ context.Context, h http.Header, body []byte) (gateway.Event, error) {
	if p.cfg.WebhookSecret == "" {
		// An empty secret would let anyone compute a valid signature.
		return gateway.Event{}, fmt.Errorf("%w: no webhook signing secret configured", gateway.ErrInvalidSignature)
	}
	if err := verifySignature(h.Get("Stripe-Signature"), body, p.cfg.WebhookSecret, time.Now()); err != nil {
		return gateway.Event{}, err
	}
	return parseEvent(body)
}

// verifySignature checks a "t=<unix>,v1=<hex>[,v1=<hex>...]" header, where
// each v1 is an HMAC-SHA256 of "<unix>.<body>". Several v1 values appear
// while a signing secret is being rolled; any one may match.
func verifySignature(header string, body []byte, secret string, now time.Time) error {
	var ts string
	var sigs []string
	for part := range strings.SplitSeq(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return fmt.Errorf("%w: malformed Stripe-Signature header", gateway.ErrInvalidSignature)
	}
	if now.Sub(time.Unix(unix, 0)).Abs() > signatureTolerance {
		return fmt.Errorf("%w: timestamp outside the %s tolerance", gateway.ErrInvalidSignature, signatureTolerance)
	}
	expected := []byte(sign(secret, ts, body))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), expected) {
			return nil
		}
	}
	return fmt.Errorf("%w: no matching v1 signature", gateway.ErrInvalidSignature)
}

func sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader builds the Stripe-Signature header Stripe would send with
// body. The local sandbox and the tests use it to produce genuine deliveries.
func SignatureHeader(secret string, body []byte, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	return "t=" + ts + ",v1=" + sign(secret, ts, body)
}

type event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

var checkoutEvents = map[string]gateway.EventKind{
	"checkout.session.completed":               gateway.EventCheckoutPaid,
	"checkout.session.async_payment_succeeded": gateway.EventCheckoutPaid,
	"checkout.session.async_payment_failed":    gateway.EventCheckoutFailed,
	"checkout.session.expired":                 gateway.EventCheckoutExpired,
}

var refundEvents = map[string]bool{
	"refund.created":        true,
	"refund.updated":        true,
	"refund.failed":         true,
	"charge.refund.updated": true,
}

func parseEvent(body []byte) (gateway.Event, error) {
	var e event
	if err := json.Unmarshal(body, &e); err != nil {
		return gateway.Event{}, fmt.Errorf("decoding event: %w", err)
	}
	if e.ID == "" {
		return gateway.Event{}, fmt.Errorf("event has no id")
	}
	ev := gateway.Event{ID: e.ID, Type: e.Type}

	if kind, ok := checkoutEvents[e.Type]; ok {
		var s session
		if err := json.Unmarshal(e.Data.Object, &s); err != nil {
			return gateway.Event{}, fmt.Errorf("decoding %s object: %w", e.Type, err)
		}
		st := s.state()
		// A session completed with a delayed payment method is not paid yet;
		// checkout.session.async_payment_succeeded follows.
		if e.Type == "checkout.session.completed" && st.Status != gateway.CheckoutPaid {
			return ev, nil
		}
		ev.Kind = kind
		ev.PaymentID = s.ClientReferenceID
		ev.CheckoutRef = s.ID
		ev.PaymentRef = st.PaymentRef
		ev.Amount, ev.Currency = s.AmountTotal, s.Currency
		return ev, nil
	}

	if refundEvents[e.Type] {
		var r refund
		if err := json.Unmarshal(e.Data.Object, &r); err != nil {
			return gateway.Event{}, fmt.Errorf("decoding %s object: %w", e.Type, err)
		}
		switch r.toGateway().Status {
		case gateway.RefundSucceeded:
			ev.Kind = gateway.EventRefundSucceeded
		case gateway.RefundFailed:
			ev.Kind = gateway.EventRefundFailed
		default:
			return ev, nil
		}
		ev.RefundID = r.Metadata["refund_id"]
		ev.RefundRef = r.ID
		ev.Amount, ev.Currency = r.Amount, r.Currency
	}
	return ev, nil
}

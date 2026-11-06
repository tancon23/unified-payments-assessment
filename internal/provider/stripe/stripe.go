// Package stripe implements gateway.Provider on Stripe Checkout.
package stripe

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
	"github.com/tancon23/unified-payments-assessment/internal/provider/providerhttp"
)

// Name identifies Stripe in API requests, webhook URLs and errors.
const Name = "stripe"

// apiVersion pins the response shapes this adapter decodes.
const apiVersion = "2024-06-20"

type Config struct {
	APIBase       string // https://api.stripe.com
	SecretKey     string
	WebhookSecret string // whsec_... of the webhook endpoint
	HTTPClient    *http.Client
}

type Provider struct {
	cfg    Config
	client providerhttp.Client
}

var _ gateway.Provider = (*Provider)(nil)

func New(cfg Config) *Provider {
	return &Provider{
		cfg: cfg,
		client: providerhttp.Client{
			Provider:   Name,
			HTTP:       cmp.Or(cfg.HTTPClient, http.DefaultClient),
			ParseError: parseError,
		},
	}
}

func (p *Provider) CreateCheckout(ctx context.Context, in gateway.CheckoutParams) (gateway.Checkout, error) {
	form := url.Values{
		"mode":                                          {string(in.Mode)},
		"client_reference_id":                           {in.PaymentID},
		"success_url":                                   {in.SuccessURL},
		"cancel_url":                                    {in.CancelURL},
		"metadata[payment_id]":                          {in.PaymentID},
		"line_items[0][quantity]":                       {"1"},
		"line_items[0][price_data][currency]":           {in.Currency},
		"line_items[0][price_data][unit_amount]":        {strconv.FormatInt(in.Amount, 10)},
		"line_items[0][price_data][product_data][name]": {in.Description},
	}
	if in.Mode == gateway.ModeSubscription {
		form.Set("line_items[0][price_data][recurring][interval]", string(in.Interval))
		form.Set("subscription_data[metadata][payment_id]", in.PaymentID)
	} else {
		form.Set("payment_intent_data[metadata][payment_id]", in.PaymentID)
	}

	var s session
	if err := p.call(ctx, http.MethodPost, "/v1/checkout/sessions", in.IdempotencyKey, form, &s); err != nil {
		return gateway.Checkout{}, err
	}
	return gateway.Checkout{Ref: s.ID, URL: s.URL}, nil
}

func (p *Provider) GetCheckout(ctx context.Context, ref string) (gateway.CheckoutState, error) {
	var s session
	if err := p.call(ctx, http.MethodGet, "/v1/checkout/sessions/"+url.PathEscape(ref), "", nil, &s); err != nil {
		return gateway.CheckoutState{}, err
	}
	return s.state(), nil
}

func (p *Provider) Refund(ctx context.Context, in gateway.RefundParams) (gateway.Refund, error) {
	form := url.Values{
		"payment_intent":      {in.PaymentRef},
		"amount":              {strconv.FormatInt(in.Amount, 10)},
		"metadata[refund_id]": {in.RefundID},
	}
	var r refund
	if err := p.call(ctx, http.MethodPost, "/v1/refunds", in.IdempotencyKey, form, &r); err != nil {
		return gateway.Refund{}, err
	}
	return r.toGateway(), nil
}

func (p *Provider) GetRefund(ctx context.Context, ref string) (gateway.Refund, error) {
	var r refund
	if err := p.call(ctx, http.MethodGet, "/v1/refunds/"+url.PathEscape(ref), "", nil, &r); err != nil {
		return gateway.Refund{}, err
	}
	return r.toGateway(), nil
}

func (p *Provider) call(ctx context.Context, method, path, idempotencyKey string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.APIBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.SecretKey)
	req.Header.Set("Stripe-Version", apiVersion)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return p.client.Do(req, out)
}

func parseError(body []byte) (code, message string) {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e) // an unreadable body leaves both fields empty
	return cmp.Or(e.Error.Code, e.Error.Type), e.Error.Message
}

// session is the subset of a Checkout Session the adapter reads.
type session struct {
	ID                string `json:"id"`
	URL               string `json:"url"`
	Mode              string `json:"mode"`
	Status            string `json:"status"`         // open, complete, expired
	PaymentStatus     string `json:"payment_status"` // paid, unpaid, no_payment_required
	PaymentIntent     string `json:"payment_intent"`
	Subscription      string `json:"subscription"`
	AmountTotal       int64  `json:"amount_total"`
	Currency          string `json:"currency"`
	ClientReferenceID string `json:"client_reference_id"`
}

func (s session) state() gateway.CheckoutState {
	st := gateway.CheckoutState{Amount: s.AmountTotal, Currency: s.Currency, PaymentRef: s.PaymentIntent}
	if s.Mode == string(gateway.ModeSubscription) {
		st.PaymentRef = s.Subscription
	}
	switch {
	case s.Status == "expired":
		st.Status = gateway.CheckoutExpired
	case s.Status == "complete" && s.PaymentStatus != "unpaid":
		st.Status = gateway.CheckoutPaid
	default:
		// Includes completed sessions whose delayed payment method (such as a
		// bank debit) has not settled yet.
		st.Status = gateway.CheckoutOpen
	}
	return st
}

type refund struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"` // pending, requires_action, succeeded, failed, canceled
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Metadata map[string]string `json:"metadata"`
}

func (r refund) toGateway() gateway.Refund {
	out := gateway.Refund{Ref: r.ID, Status: gateway.RefundPending}
	switch r.Status {
	case "succeeded":
		out.Status = gateway.RefundSucceeded
	case "failed", "canceled":
		out.Status = gateway.RefundFailed
	}
	return out
}

// Package paypal implements gateway.Provider on PayPal Orders v2 for one-off
// payments and Subscriptions v1 for recurring ones.
package paypal

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
	"github.com/tancon23/unified-payments-assessment/internal/money"
	"github.com/tancon23/unified-payments-assessment/internal/provider/providerhttp"
)

// Name identifies PayPal in API requests, webhook URLs and errors.
const Name = "paypal"

type Config struct {
	APIBase      string // https://api-m.sandbox.paypal.com or https://api-m.paypal.com
	ClientID     string
	ClientSecret string
	WebhookID    string // id of the webhook registration; part of every signature
	// PlanID is the billing plan every subscription uses. It is created once
	// in the PayPal dashboard; this adapter does not manage plans.
	PlanID     string
	HTTPClient *http.Client
}

type Provider struct {
	cfg    Config
	client providerhttp.Client

	// tokenSlot guards the cached token. It is a channel rather than a mutex
	// so that a caller waiting on a slow token fetch gives up when its own
	// deadline passes.
	tokenSlot   chan struct{}
	token       string
	tokenExpiry time.Time

	planMu sync.Mutex
	plan   *Plan
}

var (
	_ gateway.Provider = (*Provider)(nil)
	_ gateway.Capturer = (*Provider)(nil)
)

func New(cfg Config) *Provider {
	return &Provider{
		cfg: cfg,
		client: providerhttp.Client{
			Provider:   Name,
			HTTP:       cmp.Or(cfg.HTTPClient, http.DefaultClient),
			ParseError: parseError,
		},
		tokenSlot: make(chan struct{}, 1),
	}
}

func (p *Provider) CreateCheckout(ctx context.Context, in gateway.CheckoutParams) (gateway.Checkout, error) {
	if in.Mode == gateway.ModeSubscription {
		return p.createSubscription(ctx, in)
	}
	amount, err := toAmount(in.Amount, in.Currency)
	if err != nil {
		return gateway.Checkout{}, err
	}
	req := map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{{
			"custom_id":   in.PaymentID,
			"description": in.Description,
			"amount":      amount,
		}},
		"payment_source": map[string]any{"paypal": map[string]any{"experience_context": map[string]any{
			"return_url":  in.SuccessURL,
			"cancel_url":  in.CancelURL,
			"user_action": "PAY_NOW",
		}}},
	}
	var o order
	if err := p.call(ctx, http.MethodPost, "/v2/checkout/orders", in.IdempotencyKey, req, &o); err != nil {
		return gateway.Checkout{}, err
	}
	return gateway.Checkout{Ref: o.ID, URL: linkHref(o.Links, "payer-action")}, nil
}

// createSubscription subscribes the customer to the configured plan. The
// request's price must match the plan, so the gateway never records a price
// other than the one PayPal will charge.
func (p *Provider) createSubscription(ctx context.Context, in gateway.CheckoutParams) (gateway.Checkout, error) {
	plan, err := p.Plan(ctx)
	if err != nil {
		return gateway.Checkout{}, err
	}
	if plan.Amount != in.Amount || plan.Currency != in.Currency || plan.Interval != in.Interval {
		return gateway.Checkout{}, &gateway.RejectedError{
			Provider:   Name,
			StatusCode: http.StatusUnprocessableEntity,
			Code:       "PLAN_MISMATCH",
			Message: fmt.Sprintf("PayPal subscriptions use plan %s: amount %d %s every %s",
				plan.ID, plan.Amount, plan.Currency, plan.Interval),
		}
	}

	req := map[string]any{
		"plan_id":   plan.ID,
		"custom_id": in.PaymentID,
		"application_context": map[string]any{
			"return_url":  in.SuccessURL,
			"cancel_url":  in.CancelURL,
			"user_action": "SUBSCRIBE_NOW",
		},
	}
	var s subscription
	if err := p.call(ctx, http.MethodPost, "/v1/billing/subscriptions", in.IdempotencyKey, req, &s); err != nil {
		return gateway.Checkout{}, err
	}
	return gateway.Checkout{Ref: s.ID, URL: linkHref(s.Links, "approve")}, nil
}

// Plan is the billing plan subscriptions use.
type Plan struct {
	ID       string
	Amount   int64 // minor units
	Currency string
	Interval gateway.Interval
}

// Plan returns the configured billing plan, read from PayPal once and then
// kept.
func (p *Provider) Plan(ctx context.Context) (Plan, error) {
	if p.cfg.PlanID == "" {
		return Plan{}, &gateway.RejectedError{Provider: Name, StatusCode: http.StatusUnprocessableEntity,
			Code: "PLAN_NOT_CONFIGURED", Message: "set PAYPAL_PLAN_ID to the billing plan subscriptions use"}
	}
	p.planMu.Lock()
	cached := p.plan
	p.planMu.Unlock()
	if cached != nil {
		return *cached, nil
	}

	var raw struct {
		ID            string `json:"id"`
		BillingCycles []struct {
			TenureType string `json:"tenure_type"`
			Frequency  struct {
				IntervalUnit  string `json:"interval_unit"`
				IntervalCount int    `json:"interval_count"`
			} `json:"frequency"`
			PricingScheme struct {
				FixedPrice amount `json:"fixed_price"`
			} `json:"pricing_scheme"`
		} `json:"billing_cycles"`
	}
	if err := p.call(ctx, http.MethodGet, "/v1/billing/plans/"+url.PathEscape(p.cfg.PlanID), "", nil, &raw); err != nil {
		return Plan{}, fmt.Errorf("reading plan %s: %w", p.cfg.PlanID, err)
	}
	for _, cycle := range raw.BillingCycles {
		if cycle.TenureType != "REGULAR" || cycle.Frequency.IntervalCount != 1 {
			continue
		}
		amt, cur, err := cycle.PricingScheme.FixedPrice.minor()
		if err != nil {
			return Plan{}, fmt.Errorf("reading plan %s: %w", p.cfg.PlanID, err)
		}
		plan := Plan{ID: raw.ID, Amount: amt, Currency: cur, Interval: gateway.Interval(strings.ToLower(cycle.Frequency.IntervalUnit))}
		p.planMu.Lock()
		p.plan = &plan
		p.planMu.Unlock()
		return plan, nil
	}
	return Plan{}, &gateway.RejectedError{Provider: Name, StatusCode: http.StatusUnprocessableEntity, Code: "PLAN_UNSUPPORTED",
		Message: fmt.Sprintf("plan %s needs a regular billing cycle charged every one day, week, month or year", p.cfg.PlanID)}
}

func (p *Provider) GetCheckout(ctx context.Context, ref string) (gateway.CheckoutState, error) {
	if isSubscription(ref) {
		var s subscription
		if err := p.call(ctx, http.MethodGet, "/v1/billing/subscriptions/"+url.PathEscape(ref), "", nil, &s); err != nil {
			return gateway.CheckoutState{}, err
		}
		return s.state(), nil
	}
	var o order
	err := p.call(ctx, http.MethodGet, "/v2/checkout/orders/"+url.PathEscape(ref), "", nil, &o)
	if isNotFound(err) {
		// PayPal stops returning orders the buyer never approved.
		return gateway.CheckoutState{Status: gateway.CheckoutExpired}, nil
	}
	if err != nil {
		return gateway.CheckoutState{}, err
	}
	return o.state()
}

func (p *Provider) Capture(ctx context.Context, in gateway.CaptureParams) (gateway.CheckoutState, error) {
	var o order
	err := p.call(ctx, http.MethodPost, "/v2/checkout/orders/"+url.PathEscape(in.CheckoutRef)+"/capture", in.IdempotencyKey, map[string]any{}, &o)
	var rejected *gateway.RejectedError
	if errors.As(err, &rejected) && rejected.Code == "ORDER_ALREADY_CAPTURED" {
		return p.GetCheckout(ctx, in.CheckoutRef)
	}
	if err != nil {
		return gateway.CheckoutState{}, err
	}
	return o.state()
}

func (p *Provider) Refund(ctx context.Context, in gateway.RefundParams) (gateway.Refund, error) {
	amount, err := toAmount(in.Amount, in.Currency)
	if err != nil {
		return gateway.Refund{}, err
	}
	req := map[string]any{"amount": amount, "custom_id": in.RefundID}
	var r refund
	if err := p.call(ctx, http.MethodPost, "/v2/payments/captures/"+url.PathEscape(in.PaymentRef)+"/refund", in.IdempotencyKey, req, &r); err != nil {
		return gateway.Refund{}, err
	}
	return r.toGateway(), nil
}

func (p *Provider) GetRefund(ctx context.Context, ref string) (gateway.Refund, error) {
	var r refund
	if err := p.call(ctx, http.MethodGet, "/v2/payments/refunds/"+url.PathEscape(ref), "", nil, &r); err != nil {
		return gateway.Refund{}, err
	}
	return r.toGateway(), nil
}

// call sends an authenticated JSON request. A []byte body is sent as is;
// anything else is encoded as JSON. requestID becomes the PayPal-Request-Id
// header, PayPal's idempotency key.
func (p *Provider) call(ctx context.Context, method, path, requestID string, body, out any) error {
	token, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.APIBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=representation")
	if requestID != "" {
		req.Header.Set("PayPal-Request-Id", requestID)
	}

	err = p.client.Do(req, out)
	var rejected *gateway.RejectedError
	if errors.As(err, &rejected) && rejected.StatusCode == http.StatusUnauthorized {
		// The cached token was revoked or expired early. Drop it so the
		// next attempt fetches a fresh one.
		p.dropToken(ctx, token)
		return fmt.Errorf("%w: access token rejected", gateway.ErrTransient)
	}
	return err
}

// accessToken returns a cached OAuth token, fetching a new one a minute
// before the current one expires.
func (p *Provider) accessToken(ctx context.Context) (string, error) {
	if err := p.lockToken(ctx); err != nil {
		return "", err
	}
	defer p.unlockToken()
	if p.token != "" && time.Now().Before(p.tokenExpiry) {
		return p.token, nil
	}

	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.APIBase+"/v1/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(p.cfg.ClientID, p.cfg.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := p.client.Do(req, &t); err != nil {
		return "", fmt.Errorf("fetching access token: %w", err)
	}
	p.token = t.AccessToken
	p.tokenExpiry = time.Now().Add(time.Duration(t.ExpiresIn)*time.Second - time.Minute)
	return p.token, nil
}

func (p *Provider) lockToken(ctx context.Context) error {
	select {
	case p.tokenSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: waiting for an access token: %v", gateway.ErrTransient, ctx.Err())
	}
}

func (p *Provider) unlockToken() { <-p.tokenSlot }

func (p *Provider) dropToken(ctx context.Context, token string) {
	if p.lockToken(ctx) != nil {
		return
	}
	defer p.unlockToken()
	if p.token == token {
		p.token = ""
	}
}

func parseError(body []byte) (code, message string) {
	var e struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Details []struct {
			Issue       string `json:"issue"`
			Description string `json:"description"`
		} `json:"details"`
		OAuthError       string `json:"error"`
		OAuthDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &e) // an unreadable body leaves every field empty
	if len(e.Details) > 0 {
		return e.Details[0].Issue, cmp.Or(e.Details[0].Description, e.Message)
	}
	return cmp.Or(e.Name, e.OAuthError), cmp.Or(e.Message, e.OAuthDescription)
}

func isNotFound(err error) bool {
	var rejected *gateway.RejectedError
	return errors.As(err, &rejected) && rejected.StatusCode == http.StatusNotFound
}

// isSubscription tells subscription ids, which PayPal prefixes with "I-",
// apart from order ids.
func isSubscription(ref string) bool { return strings.HasPrefix(ref, "I-") }

// wholeUnitsOnly lists currencies PayPal accepts only in whole units, although
// ISO 4217 gives them a minor unit.
var wholeUnitsOnly = map[string]bool{"huf": true, "twd": true}

func toAmount(amount int64, currency string) (map[string]string, error) {
	cur := strings.ToLower(currency)
	value := money.Format(amount, cur)
	if wholeUnitsOnly[cur] {
		if amount%100 != 0 {
			return nil, &gateway.RejectedError{Provider: Name, StatusCode: http.StatusUnprocessableEntity,
				Code: "DECIMALS_NOT_SUPPORTED", Message: fmt.Sprintf("PayPal accepts whole %s amounts only", strings.ToUpper(cur))}
		}
		value = strconv.FormatInt(amount/100, 10)
	}
	return map[string]string{"currency_code": strings.ToUpper(cur), "value": value}, nil
}

func linkHref(links []link, rel string) string {
	for _, l := range links {
		if l.Rel == rel {
			return l.Href
		}
	}
	return ""
}

type link struct {
	Href string `json:"href"`
	Rel  string `json:"rel"`
}

type amount struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

func (a amount) minor() (int64, string, error) {
	if a.Value == "" {
		return 0, "", nil
	}
	n, err := money.Parse(a.Value, a.CurrencyCode)
	return n, strings.ToLower(a.CurrencyCode), err
}

type capture struct {
	ID                string `json:"id"`
	Status            string `json:"status"` // COMPLETED, PENDING, DECLINED, FAILED
	CustomID          string `json:"custom_id"`
	Amount            amount `json:"amount"`
	SupplementaryData struct {
		RelatedIDs struct {
			OrderID string `json:"order_id"`
		} `json:"related_ids"`
	} `json:"supplementary_data"`
}

type order struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Links         []link `json:"links"`
	PurchaseUnits []struct {
		CustomID string `json:"custom_id"`
		Amount   amount `json:"amount"`
		Payments struct {
			Captures []capture `json:"captures"`
		} `json:"payments"`
	} `json:"purchase_units"`
}

func (o order) state() (gateway.CheckoutState, error) {
	var st gateway.CheckoutState
	captureStatus := ""
	if len(o.PurchaseUnits) > 0 {
		pu := o.PurchaseUnits[0]
		var err error
		if st.Amount, st.Currency, err = pu.Amount.minor(); err != nil {
			return gateway.CheckoutState{}, err
		}
		if caps := pu.Payments.Captures; len(caps) > 0 {
			st.PaymentRef, captureStatus = caps[0].ID, caps[0].Status
		}
	}
	switch o.Status {
	case "APPROVED":
		st.Status = gateway.CheckoutAuthorized
	case "COMPLETED":
		st.Status = captureState(captureStatus)
	case "VOIDED":
		st.Status = gateway.CheckoutExpired
	default:
		st.Status = gateway.CheckoutOpen
	}
	return st, nil
}

func captureState(status string) gateway.CheckoutStatus {
	switch status {
	case "COMPLETED":
		return gateway.CheckoutPaid
	case "DECLINED", "FAILED":
		return gateway.CheckoutFailed
	default:
		return gateway.CheckoutOpen // PENDING: settles later with PAYMENT.CAPTURE.COMPLETED
	}
}

type subscription struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	CustomID string `json:"custom_id"`
	Links    []link `json:"links"`
}

func (s subscription) state() gateway.CheckoutState {
	st := gateway.CheckoutState{PaymentRef: s.ID, Status: gateway.CheckoutOpen}
	switch s.Status {
	case "ACTIVE":
		st.Status = gateway.CheckoutPaid
	case "CANCELLED", "EXPIRED":
		st.Status = gateway.CheckoutExpired
	case "SUSPENDED":
		st.Status = gateway.CheckoutFailed
	}
	return st
}

type refund struct {
	ID       string `json:"id"`
	Status   string `json:"status"` // COMPLETED, PENDING, CANCELLED, FAILED
	CustomID string `json:"custom_id"`
	Amount   amount `json:"amount"`
}

func (r refund) toGateway() gateway.Refund {
	out := gateway.Refund{Ref: r.ID, Status: gateway.RefundPending}
	switch r.Status {
	case "COMPLETED":
		out.Status = gateway.RefundSucceeded
	case "CANCELLED", "FAILED":
		out.Status = gateway.RefundFailed
	}
	return out
}

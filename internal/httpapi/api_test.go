package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
	"github.com/tanweerdev/unified-payments-assessment/internal/httpapi"
	"github.com/tanweerdev/unified-payments-assessment/internal/payment"
	"github.com/tanweerdev/unified-payments-assessment/internal/postgres/pgtest"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/paypal"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/stripe"
	"github.com/tanweerdev/unified-payments-assessment/internal/sandbox"
)

const (
	acmeKey   = "acme_test_key"
	globexKey = "globex_test_key"
	planID    = "P-E2EPLAN"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// stack runs the whole system in process: API, worker, Postgres, and the
// sandbox delivering signed webhooks back to the API.
type stack struct {
	t       *testing.T
	api     *httptest.Server
	sandbox *httptest.Server
	sb      *sandbox.Server
	svc     *payment.Service
	cfg     payment.Config
	clock   *clock
}

func newStack(t *testing.T) *stack {
	t.Helper()
	ctx := context.Background()
	store := pgtest.New(t)
	if err := store.UpsertTenant(ctx, "acme", "Acme", acmeKey); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTenant(ctx, "globex", "Globex", globexKey); err != nil {
		t.Fatal(err)
	}

	var router http.Handler
	ready := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-ready
		router.ServeHTTP(w, r)
	}))

	log := slog.New(slog.DiscardHandler)
	sb := sandbox.New(sandbox.Config{
		StripeWebhookURL:    api.URL + "/v1/webhooks/stripe",
		StripeWebhookSecret: "whsec_e2e",
		PayPalWebhookURL:    api.URL + "/v1/webhooks/paypal",
		PayPalWebhookID:     "WH-E2E",
		PayPalPlanID:        planID,
		SlowResponse:        600 * time.Millisecond,
		Log:                 log,
	})
	sbSrv := httptest.NewServer(sb)
	t.Cleanup(func() {
		sb.Close()
		sbSrv.Close()
		api.Close()
	})

	providers := gateway.NewRegistry()
	providers.Register(stripe.Name, stripe.New(stripe.Config{APIBase: sbSrv.URL, SecretKey: "sk_test_e2e", WebhookSecret: "whsec_e2e"}))
	providers.Register(paypal.Name, paypal.New(paypal.Config{APIBase: sbSrv.URL, ClientID: "client", ClientSecret: "secret", WebhookID: "WH-E2E", PlanID: planID}))

	s := &stack{t: t, api: api, sandbox: sbSrv, sb: sb, clock: &clock{now: time.Now()}}
	s.cfg = payment.DefaultConfig()
	s.cfg.CallTimeout = 300 * time.Millisecond
	s.cfg.Now = s.clock.Now
	s.svc = payment.NewService(store, providers, s.cfg, log)

	reg := prometheus.NewRegistry()
	router = httpapi.NewRouter(httpapi.Deps{
		Service:  s.svc,
		Tenants:  store,
		Metrics:  httpapi.NewMetrics(reg),
		Gatherer: reg,
		Log:      log,
	})
	close(ready)
	return s
}

// work moves the clock on by d and runs one worker pass.
func (s *stack) work(d time.Duration) {
	s.t.Helper()
	s.clock.Advance(d)
	if err := s.svc.Reconcile(context.Background()); err != nil {
		s.t.Fatal(err)
	}
}

type response struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (r response) str(path ...string) string {
	var v any = r.body
	for _, key := range path {
		m, _ := v.(map[string]any)
		v = m[key]
	}
	s, _ := v.(string)
	return s
}

func (s *stack) call(method, url string, header map[string]string, body string) response {
	s.t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	json.Unmarshal(raw, &out.body)
	return out
}

func (s *stack) post(path, apiKey, idempotencyKey, body string) response {
	return s.call(http.MethodPost, s.api.URL+path, map[string]string{"X-API-Key": apiKey, "Idempotency-Key": idempotencyKey}, body)
}

func (s *stack) get(path, apiKey string) response {
	return s.call(http.MethodGet, s.api.URL+path, map[string]string{"X-API-Key": apiKey}, "")
}

func (s *stack) control(path, body string) response {
	return s.call(http.MethodPost, s.sandbox.URL+path, nil, body)
}

func want(t *testing.T, r response, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("HTTP %d, want %d: %s", r.status, status, r.raw)
	}
}

func checkoutBody(provider, mode string, amount int) string {
	b, _ := json.Marshal(map[string]any{
		"provider":     provider,
		"mode":         mode,
		"amount_minor": amount,
		"currency":     "usd",
		"interval":     map[string]string{"subscription": "month"}[mode],
		"description":  "Order 42",
		"success_url":  "https://shop.test/ok?order=42&step=done",
		"cancel_url":   "https://shop.test/cancel",
	})
	return string(b)
}

func deliveryOutcomes(r response) []string {
	var out []string
	deliveries, _ := r.body["deliveries"].([]any)
	for _, d := range deliveries {
		body, _ := d.(map[string]any)["body"].(map[string]any)
		outcome, _ := body["outcome"].(string)
		out = append(out, outcome)
	}
	return out
}

func TestStripePaymentEndToEnd(t *testing.T) {
	s := newStack(t)

	created := s.post("/v1/checkouts", acmeKey, "order-42", checkoutBody("stripe", "payment", 2500))
	want(t, created, http.StatusCreated)
	id := created.str("id")
	if created.str("status") != "pending" || !strings.HasPrefix(created.str("checkout_url"), "https://checkout.stripe.com/") {
		t.Fatalf("checkout: %s", created.raw)
	}

	replay := s.post("/v1/checkouts", acmeKey, "order-42", checkoutBody("stripe", "payment", 2500))
	want(t, replay, http.StatusOK)
	if replay.str("id") != id || replay.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %s", replay.raw)
	}
	want(t, s.post("/v1/checkouts", acmeKey, "order-42", checkoutBody("paypal", "payment", 2500)), http.StatusUnprocessableEntity)

	// The customer pays; Stripe delivers the webhook twice.
	paid := s.control("/sandbox/checkouts/"+created.str("provider_checkout_id")+"/pay?deliveries=2", "")
	want(t, paid, http.StatusOK)
	if got := deliveryOutcomes(paid); len(got) != 2 || got[0] != "applied" || got[1] != "duplicate" {
		t.Fatalf("delivery outcomes = %v, want [applied duplicate]", got)
	}

	p := s.get("/v1/payments/"+id, acmeKey)
	want(t, p, http.StatusOK)
	if p.str("status") != "succeeded" || !strings.HasPrefix(p.str("provider_payment_id"), "pi_") {
		t.Fatalf("payment: %s", p.raw)
	}
	want(t, s.get("/v1/payments/"+id, globexKey), http.StatusNotFound)

	refund := s.post("/v1/payments/"+id+"/refunds", acmeKey, "refund-1", `{"amount_minor":1000}`)
	want(t, refund, http.StatusCreated)
	if refund.str("status") != "succeeded" || !strings.HasPrefix(refund.str("provider_refund_id"), "re_") {
		t.Fatalf("refund: %s", refund.raw)
	}
	want(t, s.post("/v1/payments/"+id+"/refunds", acmeKey, "refund-1", `{"amount_minor":1000}`), http.StatusOK)
	want(t, s.post("/v1/payments/"+id+"/refunds", globexKey, "refund-2", `{}`), http.StatusNotFound)
	want(t, s.post("/v1/payments/"+id+"/refunds", acmeKey, "refund-3", `{"amount_minor":2000}`), http.StatusBadRequest)

	after := s.get("/v1/payments/"+id, acmeKey)
	if after.str("status") != "partially_refunded" || after.body["refunded_minor"] != float64(1000) {
		t.Fatalf("after refund: %s", after.raw)
	}
}

func TestPayPalOrderIsCapturedOnApproval(t *testing.T) {
	s := newStack(t)
	created := s.post("/v1/checkouts", acmeKey, "order-1", checkoutBody("paypal", "payment", 2500))
	want(t, created, http.StatusCreated)

	approved := s.control("/sandbox/checkouts/"+created.str("provider_checkout_id")+"/pay", "")
	if got := deliveryOutcomes(approved); len(got) != 1 || got[0] != "applied" {
		t.Fatalf("approval outcome = %v: %s", got, approved.raw)
	}

	p := s.get("/v1/payments/"+created.str("id"), acmeKey)
	if p.str("status") != "succeeded" || p.str("provider_payment_id") == "" {
		t.Fatalf("payment after approval: %s", p.raw)
	}
	want(t, s.post("/v1/payments/"+created.str("id")+"/refunds", acmeKey, "refund-1", ``), http.StatusCreated)
	if got := s.get("/v1/payments/"+created.str("id"), acmeKey); got.str("status") != "refunded" {
		t.Fatalf("after refund: %s", got.raw)
	}
}

func TestPayPalDeclinedCaptureFailsPayment(t *testing.T) {
	s := newStack(t)
	created := s.post("/v1/checkouts", acmeKey, "order-1", checkoutBody("paypal", "payment", 2500))
	want(t, created, http.StatusCreated)
	s.control("/sandbox/checkouts/"+created.str("provider_checkout_id")+"/fail", "")

	p := s.get("/v1/payments/"+created.str("id"), acmeKey)
	if p.str("status") != "failed" || !strings.Contains(p.str("failure_reason"), "INSTRUMENT_DECLINED") {
		t.Fatalf("payment: %s", p.raw)
	}
}

func TestSubscriptionsWithBothProviders(t *testing.T) {
	s := newStack(t)
	for _, provider := range []string{"stripe", "paypal"} {
		t.Run(provider, func(t *testing.T) {
			created := s.post("/v1/checkouts", acmeKey, "sub-"+provider, checkoutBody(provider, "subscription", 2500))
			want(t, created, http.StatusCreated)
			s.control("/sandbox/checkouts/"+created.str("provider_checkout_id")+"/pay", "")

			p := s.get("/v1/payments/"+created.str("id"), acmeKey)
			if p.str("status") != "succeeded" || p.str("interval") != "month" {
				t.Fatalf("subscription: %s", p.raw)
			}
			want(t, s.post("/v1/payments/"+created.str("id")+"/refunds", acmeKey, "refund-"+provider, ``), http.StatusUnprocessableEntity)
		})
	}
}

func TestPayPalSubscriptionMustMatchThePlan(t *testing.T) {
	s := newStack(t)
	r := s.post("/v1/checkouts", acmeKey, "sub-1", checkoutBody("paypal", "subscription", 999))
	want(t, r, http.StatusUnprocessableEntity)
	if r.str("error", "code") != "provider_rejected" || !strings.Contains(r.str("error", "message"), "PLAN_MISMATCH") {
		t.Fatalf("response: %s", r.raw)
	}
}

func TestUnansweredCheckoutReturns202UnknownAndTheWorkerFinishesIt(t *testing.T) {
	s := newStack(t)
	want(t, s.control("/sandbox/faults", `{"provider":"stripe","kind":"unavailable","count":1}`), http.StatusOK)

	r := s.post("/v1/checkouts", acmeKey, "order-1", checkoutBody("stripe", "payment", 2500))
	want(t, r, http.StatusAccepted)
	if r.str("status") != "unknown" || r.body["attempts"] != float64(1) || r.str("checkout_url") != "" {
		t.Fatalf("response: %s", r.raw)
	}
	replay := s.post("/v1/checkouts", acmeKey, "order-1", checkoutBody("stripe", "payment", 2500))
	want(t, replay, http.StatusAccepted)

	s.work(s.cfg.RetryBackoff)
	p := s.get("/v1/payments/"+r.str("id"), acmeKey)
	if p.str("status") != "pending" || p.str("checkout_url") == "" {
		t.Fatalf("after the worker: %s", p.raw)
	}
}

func TestProviderTimeoutNeverOpensTwoCheckouts(t *testing.T) {
	s := newStack(t)
	// The sandbox opens the session but answers after the call timeout.
	want(t, s.control("/sandbox/faults", `{"provider":"stripe","kind":"timeout","count":1}`), http.StatusOK)

	r := s.post("/v1/checkouts", acmeKey, "order-1", checkoutBody("stripe", "payment", 2500))
	want(t, r, http.StatusAccepted)

	s.work(s.cfg.RetryBackoff)
	p := s.get("/v1/payments/"+r.str("id"), acmeKey)
	if p.str("status") != "pending" || p.str("provider_checkout_id") == "" {
		t.Fatalf("after the worker: %s", p.raw)
	}
	if n := s.sb.CheckoutCount(); n != 1 {
		t.Fatalf("the provider opened %d checkouts, want 1", n)
	}
}

func TestForgedWebhooksAreRejectedAndCounted(t *testing.T) {
	s := newStack(t)
	event := `{"id":"evt_forged","type":"checkout.session.completed","data":{"object":{"id":"cs_x"}}}`
	// A current timestamp, so the check that fails is the signature itself.
	forgedSig := fmt.Sprintf("t=%d,v1=%064x", time.Now().Unix(), 0)

	want(t, s.call(http.MethodPost, s.api.URL+"/v1/webhooks/stripe", map[string]string{"Stripe-Signature": forgedSig}, event), http.StatusBadRequest)
	want(t, s.call(http.MethodPost, s.api.URL+"/v1/webhooks/paypal", nil, `{"id":"WH-1"}`), http.StatusBadRequest)
	want(t, s.call(http.MethodPost, s.api.URL+"/v1/webhooks/acme-pay", nil, event), http.StatusNotFound)

	metrics := s.call(http.MethodGet, s.api.URL+"/metrics", nil, "").raw
	for _, line := range []string{
		`paygate_webhooks_total{outcome="invalid",provider="stripe"} 1`,
		`paygate_webhooks_total{outcome="invalid",provider="paypal"} 1`,
		`paygate_webhooks_total{outcome="unknown_provider",provider="unknown"} 1`,
	} {
		if !strings.Contains(metrics, line) {
			t.Errorf("metrics missing %s", line)
		}
	}
}

func TestRequestValidation(t *testing.T) {
	s := newStack(t)
	want(t, s.call(http.MethodGet, s.api.URL+"/v1/payments/pay_x", nil, ""), http.StatusUnauthorized)
	want(t, s.get("/v1/payments/pay_x", "wrong-key"), http.StatusUnauthorized)
	want(t, s.get("/v1/payments/pay_x", acmeKey), http.StatusNotFound)

	noKey := s.call(http.MethodPost, s.api.URL+"/v1/checkouts", map[string]string{"X-API-Key": acmeKey}, checkoutBody("stripe", "payment", 2500))
	want(t, noKey, http.StatusBadRequest)

	valid := checkoutBody("stripe", "payment", 2500)
	cases := map[string]string{
		"unknown field":    `{"provider":"stripe","mode":"payment","amount_minor":1,"currency":"usd","tip":5}`,
		"unknown provider": strings.Replace(valid, `"stripe"`, `"bitcoin"`, 1),
		"zero amount":      checkoutBody("stripe", "payment", 0),
		"bad currency":     strings.Replace(valid, `"usd"`, `"dollars"`, 1),
		"relative url":     strings.Replace(valid, `https://shop.test/cancel`, `/cancel`, 1),
		"not json":         `amount=2500`,
		"trailing data":    valid + `{"provider":"paypal"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := s.post("/v1/checkouts", acmeKey, "k-"+name, body)
			want(t, r, http.StatusBadRequest)
			if r.str("error", "code") != "invalid_request" {
				t.Fatalf("error: %s", r.raw)
			}
		})
	}

	// A rejected request leaves its key free for the corrected request.
	want(t, s.post("/v1/checkouts", acmeKey, "k-zero amount", valid), http.StatusCreated)
}

func TestHealth(t *testing.T) {
	s := newStack(t)
	r := s.call(http.MethodGet, s.api.URL+"/healthz", nil, "")
	want(t, r, http.StatusOK)
	if r.str("status") != "ok" {
		t.Fatalf("health: %s", r.raw)
	}
}

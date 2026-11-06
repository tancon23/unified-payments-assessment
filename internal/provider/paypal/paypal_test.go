package paypal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/paypal"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/providertest"
	"github.com/tanweerdev/unified-payments-assessment/internal/sandbox"
)

const (
	webhookID = "WH-TEST"
	planID    = "P-LOCALSANDBOXPLAN"
)

func sandboxProvider(t *testing.T) (*paypal.Provider, *providertest.Sandbox) {
	t.Helper()
	sb := providertest.NewSandbox(t, sandbox.Config{PayPalWebhookID: webhookID, PayPalPlanID: planID})
	return paypal.New(paypal.Config{APIBase: sb.URL, ClientID: "client", ClientSecret: "secret", WebhookID: webhookID, PlanID: planID}), sb
}

func TestContract(t *testing.T) {
	p, sb := sandboxProvider(t)
	providertest.Run(t, providertest.Harness{Provider: p, Pay: sb.Pay})
}

func TestCapturingACapturedOrderReportsTheCapture(t *testing.T) {
	ctx := context.Background()
	p, sb := sandboxProvider(t)
	co, err := p.CreateCheckout(ctx, providertest.Params(gateway.ModePayment))
	if err != nil {
		t.Fatal(err)
	}
	sb.Pay(t, co.Ref)

	first, err := p.Capture(ctx, gateway.CaptureParams{IdempotencyKey: "capture-1", CheckoutRef: co.Ref})
	if err != nil {
		t.Fatal(err)
	}
	// A new key hits ORDER_ALREADY_CAPTURED, which reports the capture instead
	// of failing.
	again, err := p.Capture(ctx, gateway.CaptureParams{IdempotencyKey: "capture-2", CheckoutRef: co.Ref})
	if err != nil || again != first {
		t.Fatalf("second capture = %+v, %v; want %+v", again, err, first)
	}
}

func rejectedCode(err error) string {
	var rejected *gateway.RejectedError
	if errors.As(err, &rejected) {
		return rejected.Code
	}
	return ""
}

func TestSubscriptionsUseTheConfiguredPlan(t *testing.T) {
	ctx := context.Background()
	p, sb := sandboxProvider(t)

	plan, err := p.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan != (paypal.Plan{ID: planID, Amount: 2500, Currency: "usd", Interval: gateway.IntervalMonth}) {
		t.Fatalf("plan = %+v", plan)
	}

	cheaper := providertest.Params(gateway.ModeSubscription)
	cheaper.Amount = 999
	yearly := providertest.Params(gateway.ModeSubscription)
	yearly.Interval = gateway.IntervalYear
	for name, params := range map[string]gateway.CheckoutParams{"amount": cheaper, "interval": yearly} {
		if _, err := p.CreateCheckout(ctx, params); rejectedCode(err) != "PLAN_MISMATCH" {
			t.Errorf("%s differs from the plan: want PLAN_MISMATCH, got %v", name, err)
		}
	}

	unconfigured := paypal.New(paypal.Config{APIBase: sb.URL, ClientID: "client", ClientSecret: "secret"})
	if _, err := unconfigured.CreateCheckout(ctx, providertest.Params(gateway.ModeSubscription)); rejectedCode(err) != "PLAN_NOT_CONFIGURED" {
		t.Errorf("no plan configured: want PLAN_NOT_CONFIGURED, got %v", err)
	}
}

// fakeAPI answers the token endpoint and hands every other request to h.
func fakeAPI(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestWholeUnitCurrencies(t *testing.T) {
	var sent []byte
	base := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"id":"ORDER1","status":"PAYER_ACTION_REQUIRED"}`))
	})
	p := paypal.New(paypal.Config{APIBase: base, ClientID: "c", ClientSecret: "s"})
	params := providertest.Params(gateway.ModePayment)
	params.Currency = "huf"

	params.Amount = 1050
	if _, err := p.CreateCheckout(context.Background(), params); rejectedCode(err) != "DECIMALS_NOT_SUPPORTED" {
		t.Fatalf("10.50 HUF: want DECIMALS_NOT_SUPPORTED, got %v", err)
	}
	params.Amount = 1000
	if _, err := p.CreateCheckout(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(sent, []byte(`"value":"10"`)) {
		t.Fatalf("10.00 HUF sent as %s, want value \"10\"", sent)
	}
}

func signedHeaders() http.Header {
	h := http.Header{}
	for _, name := range []string{"Paypal-Auth-Algo", "Paypal-Cert-Url", "Paypal-Transmission-Id", "Paypal-Transmission-Sig", "Paypal-Transmission-Time"} {
		h.Set(name, "x")
	}
	return h
}

func TestVerificationSendsTheEventByteForByte(t *testing.T) {
	var sent []byte
	base := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"verification_status":"SUCCESS"}`))
	})
	p := paypal.New(paypal.Config{APIBase: base, ClientID: "c", ClientSecret: "s", WebhookID: webhookID})
	event := []byte("{\n  \"id\": \"WH-1\",\n  \"event_type\": \"X\",\n  \"resource\": {\"url\": \"https://a.test/?a=1&b=<2>\"}\n}")

	if _, err := p.VerifyWebhook(context.Background(), signedHeaders(), event); err != nil {
		t.Fatal(err)
	}
	var req struct {
		WebhookID    string          `json:"webhook_id"`
		WebhookEvent json.RawMessage `json:"webhook_event"`
	}
	if err := json.Unmarshal(sent, &req); err != nil {
		t.Fatalf("verification request is not JSON: %v\n%s", err, sent)
	}
	if req.WebhookID != webhookID || !bytes.Equal(req.WebhookEvent, event) {
		t.Fatalf("verification request changed the event:\n got %s\nwant %s", req.WebhookEvent, event)
	}
}

func TestVerifyWebhookRejectsForgeries(t *testing.T) {
	ctx := context.Background()
	p, sb := sandboxProvider(t)
	co, err := p.CreateCheckout(ctx, providertest.Params(gateway.ModePayment))
	if err != nil {
		t.Fatal(err)
	}
	genuine := sb.Pay(t, co.Ref)

	tampered := bytes.Replace(genuine.Body, []byte(`"25.00"`), []byte(`"0.01"`), 1)
	if _, err := p.VerifyWebhook(ctx, genuine.Header, tampered); !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("tampered body: want ErrInvalidSignature, got %v", err)
	}
	for _, h := range []string{"Paypal-Transmission-Sig", "Paypal-Transmission-Id", "Paypal-Cert-Url"} {
		header := genuine.Header.Clone()
		header.Del(h)
		if _, err := p.VerifyWebhook(ctx, header, genuine.Body); !errors.Is(err, gateway.ErrInvalidSignature) {
			t.Errorf("without %s: want ErrInvalidSignature, got %v", h, err)
		}
	}
	other := paypal.New(paypal.Config{APIBase: sb.URL, ClientID: "client", ClientSecret: "secret", WebhookID: "WH-ANOTHER-ENDPOINT"})
	if _, err := other.VerifyWebhook(ctx, genuine.Header, genuine.Body); !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("delivery meant for another webhook id: want ErrInvalidSignature, got %v", err)
	}
	unset := paypal.New(paypal.Config{APIBase: sb.URL, ClientID: "client", ClientSecret: "secret"})
	if _, err := unset.VerifyWebhook(ctx, genuine.Header, genuine.Body); !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("no webhook id configured: want ErrInvalidSignature, got %v", err)
	}
	if _, err := p.VerifyWebhook(ctx, genuine.Header, []byte("not json")); !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("non-JSON body: want ErrInvalidSignature, got %v", err)
	}
}

func TestVerificationOutageIsTransient(t *testing.T) {
	base := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	p := paypal.New(paypal.Config{APIBase: base, ClientID: "c", ClientSecret: "s", WebhookID: webhookID})
	if _, err := p.VerifyWebhook(context.Background(), signedHeaders(), []byte(`{"id":"WH-1"}`)); !errors.Is(err, gateway.ErrTransient) {
		t.Fatalf("want ErrTransient so PayPal redelivers, got %v", err)
	}
}

func TestRejectedTokenIsRefreshed(t *testing.T) {
	var tokenRequests atomic.Int32
	var rejectNext atomic.Bool
	rejectNext.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			tokenRequests.Add(1)
			w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		if rejectNext.Swap(false) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid_token"}`))
			return
		}
		w.Write([]byte(`{"id":"ORDER1","status":"PAYER_ACTION_REQUIRED"}`))
	}))
	t.Cleanup(srv.Close)
	p := paypal.New(paypal.Config{APIBase: srv.URL, ClientID: "c", ClientSecret: "s"})
	ctx := context.Background()

	if _, err := p.GetCheckout(ctx, "ORDER1"); !errors.Is(err, gateway.ErrTransient) {
		t.Fatalf("rejected token: want ErrTransient so the call is retried, got %v", err)
	}
	if _, err := p.GetCheckout(ctx, "ORDER1"); err != nil {
		t.Fatalf("retry with a fresh token: %v", err)
	}
	if n := tokenRequests.Load(); n != 2 {
		t.Fatalf("token requests = %d, want 2", n)
	}
}

func TestWaitForTokenEndsAtTheCallersDeadline(t *testing.T) {
	fetching, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(fetching)
		<-release
		w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	}))
	defer srv.Close()
	defer close(release)
	p := paypal.New(paypal.Config{APIBase: srv.URL, ClientID: "c", ClientSecret: "s"})

	go p.GetCheckout(context.Background(), "ORDER1") // stalls fetching the token
	<-fetching

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.GetCheckout(ctx, "ORDER2")
	if !errors.Is(err, gateway.ErrTransient) || time.Since(start) > time.Second {
		t.Fatalf("waiting behind a stalled token fetch: err = %v after %s", err, time.Since(start))
	}
}

func TestOrderPayPalNoLongerKnowsIsExpired(t *testing.T) {
	base := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"name":"RESOURCE_NOT_FOUND","details":[{"issue":"INVALID_RESOURCE_ID"}]}`))
	})
	p := paypal.New(paypal.Config{APIBase: base, ClientID: "c", ClientSecret: "s"})
	st, err := p.GetCheckout(context.Background(), "ORDER1")
	if err != nil || st.Status != gateway.CheckoutExpired {
		t.Fatalf("state = %+v, %v; want expired", st, err)
	}
}

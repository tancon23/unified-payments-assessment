// Package sandbox imitates the parts of the Stripe and PayPal sandbox APIs
// the gateway uses, following their documented request and response shapes.
// It keeps state, honours idempotency keys, delivers signed webhooks and can
// inject faults, so the gateway runs end to end with no provider account.
//
// Control endpoints play the customer and the network:
//
//	POST /sandbox/checkouts/{ref}/pay?deliveries=N   customer pays (webhook sent N times)
//	POST /sandbox/checkouts/{ref}/fail               payment fails
//	POST /sandbox/checkouts/{ref}/expire             checkout expires
//	POST /sandbox/faults                             {"provider":"stripe","kind":"timeout","count":1}
//
// It holds one PayPal billing plan (Config.PayPalPlanID), charging
// PlanAmount PlanCurrency every PlanInterval, as if created in the PayPal
// dashboard.
package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	"github.com/tanweerdev/unified-payments-assessment/internal/provider/paypal"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/stripe"
)

type Config struct {
	PublicURL           string // where the sandbox is reachable, used in links it returns
	StripeWebhookURL    string
	StripeWebhookSecret string
	PayPalWebhookURL    string
	PayPalWebhookID     string
	PayPalPlanID        string
	// SlowResponse is how long a "timeout" fault holds the response after
	// the request has been processed.
	SlowResponse time.Duration
	HTTPClient   *http.Client
	Log          *slog.Logger
}

type Server struct {
	cfg        Config
	mux        *http.ServeMux
	signingKey []byte
	// stop ends deliveries still waiting to be sent.
	stop       context.Context
	stopCancel context.CancelFunc

	mu      sync.Mutex
	replies map[string]*recorded // provider + idempotency key -> first response; nil while in flight
	faults  map[string]Fault     // provider -> fault to inject
	tokens  map[string]bool

	sessions      map[string]*stripeSession
	stripeRefunds map[string]*stripeRefund

	orders         map[string]*ppOrder
	subscriptions  map[string]*ppSubscription
	ppRefunds      map[string]*ppRefund
	declineCapture map[string]bool
}

type recorded struct {
	status int
	body   []byte
}

// The plan the sandbox holds under Config.PayPalPlanID.
const (
	PlanAmount   = "25.00"
	PlanCurrency = "USD"
	PlanInterval = "MONTH"
)

// Fault makes the next Count API calls to Provider misbehave. Kind
// "unavailable" answers 503 without acting; "timeout" acts, then answers
// after Config.SlowResponse, so the caller cannot know whether it worked.
type Fault struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Count    int    `json:"count"`
}

func New(cfg Config) *Server {
	cfg.SlowResponse = cmp.Or(cfg.SlowResponse, 12*time.Second)
	cfg.HTTPClient = cmp.Or(cfg.HTTPClient, &http.Client{Timeout: 30 * time.Second})
	cfg.Log = cmp.Or(cfg.Log, slog.Default())
	cfg.PayPalPlanID = cmp.Or(cfg.PayPalPlanID, "P-LOCALSANDBOXPLAN")

	key := make([]byte, 32)
	rand.Read(key)
	stop, stopCancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:            cfg,
		mux:            http.NewServeMux(),
		signingKey:     key,
		stop:           stop,
		stopCancel:     stopCancel,
		replies:        map[string]*recorded{},
		faults:         map[string]Fault{},
		tokens:         map[string]bool{},
		sessions:       map[string]*stripeSession{},
		stripeRefunds:  map[string]*stripeRefund{},
		orders:         map[string]*ppOrder{},
		subscriptions:  map[string]*ppSubscription{},
		ppRefunds:      map[string]*ppRefund{},
		declineCapture: map[string]bool{},
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Close drops webhook deliveries still waiting to be sent.
func (s *Server) Close() { s.stopCancel() }

// CheckoutCount reports how many checkouts (Stripe sessions, PayPal orders
// and subscriptions) the sandbox has opened.
func (s *Server) CheckoutCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions) + len(s.orders) + len(s.subscriptions)
}

func (s *Server) routes() {
	stripeRoute := func(pattern string, h http.HandlerFunc) {
		s.mux.HandleFunc(pattern, s.api(stripe.Name, s.stripeAuth(h)))
	}
	stripeRoute("POST /v1/checkout/sessions", s.stripeCreateSession)
	stripeRoute("GET /v1/checkout/sessions/{id}", s.stripeGetSession)
	stripeRoute("POST /v1/refunds", s.stripeCreateRefund)
	stripeRoute("GET /v1/refunds/{id}", s.stripeGetRefund)

	paypalRoute := func(pattern string, h http.HandlerFunc) {
		s.mux.HandleFunc(pattern, s.api(paypal.Name, s.paypalAuth(h)))
	}
	s.mux.HandleFunc("POST /v1/oauth2/token", s.api(paypal.Name, s.paypalToken))
	paypalRoute("POST /v2/checkout/orders", s.paypalCreateOrder)
	paypalRoute("GET /v2/checkout/orders/{id}", s.paypalGetOrder)
	paypalRoute("POST /v2/checkout/orders/{id}/capture", s.paypalCaptureOrder)
	paypalRoute("GET /v1/billing/plans/{id}", s.paypalGetPlan)
	paypalRoute("POST /v1/billing/subscriptions", s.paypalCreateSubscription)
	paypalRoute("GET /v1/billing/subscriptions/{id}", s.paypalGetSubscription)
	paypalRoute("POST /v2/payments/captures/{id}/refund", s.paypalRefundCapture)
	paypalRoute("GET /v2/payments/refunds/{id}", s.paypalGetRefund)
	paypalRoute("POST /v1/notifications/verify-webhook-signature", s.paypalVerifySignature)

	s.mux.HandleFunc("POST /sandbox/checkouts/{ref}/{action}", s.controlCheckout)
	s.mux.HandleFunc("POST /sandbox/faults", s.controlFaults)
}

var idempotencyHeaders = map[string]string{
	stripe.Name: "Idempotency-Key",
	paypal.Name: "PayPal-Request-Id",
}

// api wraps a provider endpoint with fault injection and idempotency.
func (s *Server) api(provider string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := s.takeFault(provider)
		if kind == "unavailable" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "service unavailable"})
			return
		}
		rec := httptest.NewRecorder()
		s.idempotent(provider, h)(rec, r)
		if kind == "timeout" {
			time.Sleep(s.cfg.SlowResponse)
		}
		copyResponse(w, rec)
	}
}

// idempotent replays the first response to a repeated idempotency key, as
// both providers do, instead of acting twice. A repeat that arrives while
// the first request is still running gets 409, as at Stripe.
func (s *Server) idempotent(provider string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyHeaders[provider])
		if r.Method != http.MethodPost || key == "" {
			h(w, r)
			return
		}
		id := provider + ":" + key
		s.mu.Lock()
		reply, seen := s.replies[id]
		if !seen {
			s.replies[id] = nil
		}
		s.mu.Unlock()
		switch {
		case seen && reply == nil:
			writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{
				"type": "idempotency_error", "message": "There is currently another in-progress request using this Idempotent Key."}})
			return
		case seen:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(reply.status)
			w.Write(reply.body)
			return
		}

		rec := httptest.NewRecorder()
		h(rec, r)
		s.mu.Lock()
		if rec.Code < 500 {
			s.replies[id] = &recorded{rec.Code, rec.Body.Bytes()}
		} else {
			delete(s.replies, id)
		}
		s.mu.Unlock()
		copyResponse(w, rec)
	}
}

func (s *Server) takeFault(provider string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.faults[provider]
	if !ok {
		return ""
	}
	if f.Count--; f.Count <= 0 {
		delete(s.faults, provider)
	} else {
		s.faults[provider] = f
	}
	return f.Kind
}

func (s *Server) controlFaults(w http.ResponseWriter, r *http.Request) {
	var f Fault
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil ||
		(f.Provider != stripe.Name && f.Provider != paypal.Name) ||
		(f.Kind != "unavailable" && f.Kind != "timeout") || f.Count < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `want {"provider":"stripe|paypal","kind":"unavailable|timeout","count":N}`})
		return
	}
	s.mu.Lock()
	s.faults[f.Provider] = f
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, f)
}

// event is a webhook waiting to be delivered. Its id stays the same across
// redeliveries, as with the real providers.
type event struct {
	id       string
	provider string
	typ      string
	resource json.RawMessage
}

var eventIDPrefixes = map[string]string{stripe.Name: "evt_", paypal.Name: "WH-"}

func newEvent(provider, typ string, resource []byte) *event {
	return &event{
		id:       eventIDPrefixes[provider] + randomString(upperAlnum, 24),
		provider: provider,
		typ:      typ,
		resource: resource,
	}
}

var errNotFound = errors.New("no such checkout")

// controlCheckout plays the customer: it moves a checkout on and delivers
// the webhook the provider would send, answering with the gateway's replies.
func (s *Server) controlCheckout(w http.ResponseWriter, r *http.Request) {
	ref, action := r.PathValue("ref"), r.PathValue("action")
	deliveries := 1
	if raw := r.URL.Query().Get("deliveries"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 5 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "deliveries must be 1 to 5"})
			return
		}
		deliveries = n
	}

	ev, err := s.advanceCheckout(ref, action)
	switch {
	case errors.Is(err, errNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	case err != nil:
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	out := map[string]any{"checkout": ref, "action": action, "deliveries": []delivery{}}
	if ev != nil {
		out["event_type"] = ev.typ
		out["deliveries"] = s.deliver(*ev, deliveries)
	} else {
		out["note"] = "the provider sends no webhook for this; the gateway's reconciliation picks it up"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) advanceCheckout(ref, action string) (*event, error) {
	if action != "pay" && action != "fail" && action != "expire" {
		return nil, fmt.Errorf("action must be pay, fail or expire")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[ref]; ok {
		return s.advanceSession(sess, action)
	}
	if o, ok := s.orders[ref]; ok {
		return s.advanceOrder(o, action)
	}
	if sub, ok := s.subscriptions[ref]; ok {
		return s.advanceSubscription(sub, action)
	}
	return nil, fmt.Errorf("%w %q", errNotFound, ref)
}

type delivery struct {
	Attempt int             `json:"attempt"`
	Status  int             `json:"status"`
	Body    json.RawMessage `json:"body,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// deliver sends one event n times with the same event id, the way
// providers redeliver, and returns what the gateway answered each time.
func (s *Server) deliver(ev event, n int) []delivery {
	var url string
	var body []byte
	var sign func([]byte) http.Header
	switch ev.provider {
	case stripe.Name:
		url, body, sign = s.cfg.StripeWebhookURL, s.stripeEnvelope(ev), s.stripeHeaders
	default:
		url, body, sign = s.cfg.PayPalWebhookURL, s.paypalEnvelope(ev), s.paypalHeaders
	}

	results := make([]delivery, 0, n)
	for attempt := 1; attempt <= n; attempt++ {
		d := delivery{Attempt: attempt}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err == nil {
			req.Header = sign(body)
			req.Header.Set("Content-Type", "application/json")
			var resp *http.Response
			if resp, err = s.cfg.HTTPClient.Do(req); err == nil {
				d.Status = resp.StatusCode
				var reply json.RawMessage
				if json.NewDecoder(resp.Body).Decode(&reply) == nil {
					d.Body = reply
				}
				resp.Body.Close()
			}
		}
		if err != nil {
			d.Error = err.Error()
		}
		s.cfg.Log.Info("webhook delivered", "provider", ev.provider, "type", ev.typ, "attempt", attempt, "status", d.Status, "err", d.Error)
		results = append(results, d)
	}
	return results
}

// deliverSoon delivers an event shortly after the current response, retrying
// with backoff until the gateway accepts it, as providers do. Close stops it.
func (s *Server) deliverSoon(ev event) {
	go func() {
		wait := 300 * time.Millisecond
		for range 5 {
			select {
			case <-s.stop.Done():
				return
			case <-time.After(wait):
			}
			if d := s.deliver(ev, 1)[0]; d.Status >= 200 && d.Status < 300 {
				return
			}
			wait *= 3
		}
	}()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeRaw(w, status, body)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

func copyResponse(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes())
}

// mustJSON marshals sandbox-owned values, which always encode.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

const (
	lowerAlnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	upperAlnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

func randomString(alphabet string, n int) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

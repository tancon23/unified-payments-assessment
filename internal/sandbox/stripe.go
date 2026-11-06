package sandbox

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tanweerdev/unified-payments-assessment/internal/provider/stripe"
)

type stripeSession struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Mode              string            `json:"mode"`
	Status            string            `json:"status"`
	PaymentStatus     string            `json:"payment_status"`
	AmountTotal       int64             `json:"amount_total"`
	Currency          string            `json:"currency"`
	ClientReferenceID string            `json:"client_reference_id"`
	Metadata          map[string]string `json:"metadata"`
	PaymentIntent     *string           `json:"payment_intent"`
	Subscription      *string           `json:"subscription"`
	URL               *string           `json:"url"`
	SuccessURL        string            `json:"success_url"`
	CancelURL         string            `json:"cancel_url"`
	Livemode          bool              `json:"livemode"`
	Created           int64             `json:"created"`
	ExpiresAt         int64             `json:"expires_at"`
}

type stripeRefund struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	PaymentIntent string            `json:"payment_intent"`
	Status        string            `json:"status"`
	Metadata      map[string]string `json:"metadata"`
	Created       int64             `json:"created"`
}

func stripeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"type": "invalid_request_error", "code": code, "message": msg,
	}})
}

func (s *Server) stripeAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sk_test_") {
			stripeError(w, http.StatusUnauthorized, "api_key_invalid", "Invalid API Key provided.")
			return
		}
		h(w, r)
	}
}

func (s *Server) stripeCreateSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		stripeError(w, http.StatusBadRequest, "parameter_invalid", "Unreadable form body.")
		return
	}
	f := r.PostForm
	amount, err := strconv.ParseInt(f.Get("line_items[0][price_data][unit_amount]"), 10, 64)
	mode := f.Get("mode")
	switch {
	case err != nil || amount <= 0:
		stripeError(w, http.StatusBadRequest, "parameter_invalid_integer", "Invalid integer: line_items[0][price_data][unit_amount].")
		return
	case mode != "payment" && mode != "subscription":
		stripeError(w, http.StatusBadRequest, "parameter_invalid_empty", "Missing required param: mode.")
		return
	case mode == "subscription" && f.Get("line_items[0][price_data][recurring][interval]") == "":
		stripeError(w, http.StatusBadRequest, "parameter_missing", "Subscription mode requires a recurring price.")
		return
	case f.Get("success_url") == "":
		stripeError(w, http.StatusBadRequest, "parameter_missing", "Missing required param: success_url.")
		return
	}

	now := time.Now()
	id := "cs_test_" + randomString(lowerAlnum+"ABCDEFGHIJKLMNOPQRSTUVWXYZ", 58)
	url := "https://checkout.stripe.com/c/pay/" + id
	sess := &stripeSession{
		ID:                id,
		Object:            "checkout.session",
		Mode:              mode,
		Status:            "open",
		PaymentStatus:     "unpaid",
		AmountTotal:       amount,
		Currency:          f.Get("line_items[0][price_data][currency]"),
		ClientReferenceID: f.Get("client_reference_id"),
		Metadata:          map[string]string{"payment_id": f.Get("metadata[payment_id]")},
		URL:               &url,
		SuccessURL:        f.Get("success_url"),
		CancelURL:         f.Get("cancel_url"),
		Created:           now.Unix(),
		ExpiresAt:         now.Add(24 * time.Hour).Unix(),
	}
	s.mu.Lock()
	s.sessions[id] = sess
	body := mustJSON(sess)
	s.mu.Unlock()
	writeRaw(w, http.StatusOK, body)
}

func (s *Server) stripeGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	sess, ok := s.sessions[id]
	var body []byte
	if ok {
		body = mustJSON(sess)
	}
	s.mu.Unlock()
	if !ok {
		stripeError(w, http.StatusNotFound, "resource_missing", fmt.Sprintf("No such checkout.session: '%s'", id))
		return
	}
	writeRaw(w, http.StatusOK, body)
}

func (s *Server) stripeCreateRefund(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		stripeError(w, http.StatusBadRequest, "parameter_invalid", "Unreadable form body.")
		return
	}
	f := r.PostForm
	intent := f.Get("payment_intent")
	amount, err := strconv.ParseInt(f.Get("amount"), 10, 64)
	if err != nil || amount <= 0 {
		stripeError(w, http.StatusBadRequest, "parameter_invalid_integer", "Invalid integer: amount.")
		return
	}

	s.mu.Lock()
	var paid *stripeSession
	for _, sess := range s.sessions {
		if sess.PaymentIntent != nil && *sess.PaymentIntent == intent {
			paid = sess
		}
	}
	if paid == nil {
		s.mu.Unlock()
		stripeError(w, http.StatusNotFound, "resource_missing", fmt.Sprintf("No such payment_intent: '%s'", intent))
		return
	}
	var refunded int64
	for _, ref := range s.stripeRefunds {
		if ref.PaymentIntent == intent {
			refunded += ref.Amount
		}
	}
	if amount > paid.AmountTotal-refunded {
		s.mu.Unlock()
		stripeError(w, http.StatusBadRequest, "amount_too_large", "Refund amount is greater than the unrefunded amount on the charge.")
		return
	}
	ref := &stripeRefund{
		ID:            "re_" + randomString(lowerAlnum, 24),
		Object:        "refund",
		Amount:        amount,
		Currency:      paid.Currency,
		PaymentIntent: intent,
		Status:        "succeeded",
		Metadata:      map[string]string{"refund_id": f.Get("metadata[refund_id]")},
		Created:       time.Now().Unix(),
	}
	s.stripeRefunds[ref.ID] = ref
	body := mustJSON(ref)
	s.mu.Unlock()

	writeRaw(w, http.StatusOK, body)
	s.deliverSoon(*newEvent(stripe.Name, "refund.created", body))
}

func (s *Server) stripeGetRefund(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	ref, ok := s.stripeRefunds[id]
	var body []byte
	if ok {
		body = mustJSON(ref)
	}
	s.mu.Unlock()
	if !ok {
		stripeError(w, http.StatusNotFound, "resource_missing", fmt.Sprintf("No such refund: '%s'", id))
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// advanceSession moves an open session on. The caller holds s.mu.
func (s *Server) advanceSession(sess *stripeSession, action string) (*event, error) {
	if sess.Status != "open" {
		return nil, fmt.Errorf("checkout session is %s, not open", sess.Status)
	}
	sess.URL = nil
	var typ string
	switch action {
	case "pay":
		sess.Status, sess.PaymentStatus = "complete", "paid"
		if sess.Mode == "subscription" {
			sub := "sub_" + randomString(lowerAlnum, 24)
			sess.Subscription = &sub
		} else {
			pi := "pi_" + randomString(lowerAlnum, 24)
			sess.PaymentIntent = &pi
		}
		typ = "checkout.session.completed"
	case "fail":
		sess.Status = "complete"
		typ = "checkout.session.async_payment_failed"
	case "expire":
		sess.Status = "expired"
		typ = "checkout.session.expired"
	}
	return newEvent(stripe.Name, typ, mustJSON(sess)), nil
}

func (s *Server) stripeEnvelope(ev event) []byte {
	return mustJSON(map[string]any{
		"id":          ev.id,
		"object":      "event",
		"api_version": "2024-06-20",
		"created":     time.Now().Unix(),
		"type":        ev.typ,
		"livemode":    false,
		"data":        map[string]any{"object": ev.resource},
	})
}

func (s *Server) stripeHeaders(body []byte) http.Header {
	return http.Header{"Stripe-Signature": {stripe.SignatureHeader(s.cfg.StripeWebhookSecret, body, time.Now())}}
}

package sandbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/money"
	"github.com/tancon23/unified-payments-assessment/internal/provider/paypal"
)

type ppAmount struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

type ppLink struct {
	Href   string `json:"href"`
	Rel    string `json:"rel"`
	Method string `json:"method"`
}

type ppCapture struct {
	ID                string   `json:"id"`
	Status            string   `json:"status"`
	Amount            ppAmount `json:"amount"`
	CustomID          string   `json:"custom_id"`
	FinalCapture      bool     `json:"final_capture"`
	SupplementaryData struct {
		RelatedIDs struct {
			OrderID string `json:"order_id"`
		} `json:"related_ids"`
	} `json:"supplementary_data"`
	CreateTime string `json:"create_time"`
}

type ppPayments struct {
	Captures []ppCapture `json:"captures"`
}

type ppPurchaseUnit struct {
	ReferenceID string      `json:"reference_id"`
	CustomID    string      `json:"custom_id"`
	Description string      `json:"description,omitempty"`
	Amount      ppAmount    `json:"amount"`
	Payments    *ppPayments `json:"payments,omitempty"`
}

type ppOrder struct {
	ID            string           `json:"id"`
	Status        string           `json:"status"`
	Intent        string           `json:"intent"`
	PurchaseUnits []ppPurchaseUnit `json:"purchase_units"`
	Links         []ppLink         `json:"links"`
	CreateTime    string           `json:"create_time"`
}

type ppSubscription struct {
	ID         string   `json:"id"`
	Status     string   `json:"status"`
	PlanID     string   `json:"plan_id"`
	CustomID   string   `json:"custom_id"`
	Links      []ppLink `json:"links"`
	CreateTime string   `json:"create_time"`
}

type ppRefund struct {
	ID         string   `json:"id"`
	Status     string   `json:"status"`
	Amount     ppAmount `json:"amount"`
	CustomID   string   `json:"custom_id"`
	CreateTime string   `json:"create_time"`
	captureID  string
}

const paypalCheckoutHost = "https://www.sandbox.paypal.com"

func paypalError(w http.ResponseWriter, status int, name, issue, description string) {
	writeJSON(w, status, map[string]any{
		"name":    name,
		"message": description,
		"details": []map[string]string{{"issue": issue, "description": description}},
	})
}

func now3339() string { return time.Now().UTC().Format(time.RFC3339) }

func (s *Server) paypalToken(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id == "" || secret == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client", "error_description": "Client Authentication failed"})
		return
	}
	token := "A21AA" + randomString(upperAlnum+lowerAlnum, 60)
	s.mu.Lock()
	s.tokens[token] = true
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":        "https://uri.paypal.com/services/payments/payment",
		"access_token": token,
		"token_type":   "Bearer",
		"app_id":       "APP-80W284485P519543T",
		"expires_in":   32400,
	})
}

func (s *Server) paypalAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		ok := s.tokens[token]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token", "error_description": "Token signature verification failed"})
			return
		}
		h(w, r)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		paypalError(w, http.StatusBadRequest, "INVALID_REQUEST", "MALFORMED_REQUEST_JSON", "The request JSON is not well formed.")
		return false
	}
	return true
}

func validAmount(a ppAmount) bool {
	n, err := money.Parse(a.Value, a.CurrencyCode)
	return err == nil && n > 0 && len(a.CurrencyCode) == 3
}

func (s *Server) paypalCreateOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Intent        string           `json:"intent"`
		PurchaseUnits []ppPurchaseUnit `json:"purchase_units"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Intent != "CAPTURE" || len(req.PurchaseUnits) != 1 || !validAmount(req.PurchaseUnits[0].Amount) {
		paypalError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "INVALID_PARAMETER_VALUE", "The value of a field is invalid.")
		return
	}
	id := randomString(upperAlnum, 17)
	pu := req.PurchaseUnits[0]
	pu.ReferenceID = "default"
	pu.Payments = nil
	o := &ppOrder{
		ID:            id,
		Status:        "PAYER_ACTION_REQUIRED",
		Intent:        "CAPTURE",
		PurchaseUnits: []ppPurchaseUnit{pu},
		Links: []ppLink{
			{Href: s.cfg.PublicURL + "/v2/checkout/orders/" + id, Rel: "self", Method: "GET"},
			{Href: paypalCheckoutHost + "/checkoutnow?token=" + id, Rel: "payer-action", Method: "GET"},
		},
		CreateTime: now3339(),
	}
	s.mu.Lock()
	s.orders[id] = o
	body := mustJSON(o)
	s.mu.Unlock()
	writeRaw(w, http.StatusOK, body)
}

func (s *Server) paypalGetOrder(w http.ResponseWriter, r *http.Request) {
	s.getJSON(w, func() (any, bool) { o, ok := s.orders[r.PathValue("id")]; return o, ok })
}

func (s *Server) paypalCaptureOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	o, ok := s.orders[id]
	var issue string
	switch {
	case !ok:
		s.mu.Unlock()
		paypalError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "INVALID_RESOURCE_ID", "Specified resource ID does not exist.")
		return
	case o.Status == "COMPLETED":
		issue = "ORDER_ALREADY_CAPTURED"
	case o.Status != "APPROVED":
		issue = "ORDER_NOT_APPROVED"
	case s.declineCapture[id]:
		issue = "INSTRUMENT_DECLINED"
	}
	if issue != "" {
		s.mu.Unlock()
		paypalError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", issue, "The requested action could not be performed.")
		return
	}

	pu := &o.PurchaseUnits[0]
	c := ppCapture{
		ID:           randomString(upperAlnum, 17),
		Status:       "COMPLETED",
		Amount:       pu.Amount,
		CustomID:     pu.CustomID,
		FinalCapture: true,
		CreateTime:   now3339(),
	}
	c.SupplementaryData.RelatedIDs.OrderID = id
	pu.Payments = &ppPayments{Captures: []ppCapture{c}}
	o.Status = "COMPLETED"
	body, captureJSON := mustJSON(o), mustJSON(c)
	s.mu.Unlock()

	writeRaw(w, http.StatusCreated, body)
	s.deliverSoon(*newEvent(paypal.Name, "PAYMENT.CAPTURE.COMPLETED", captureJSON))
}

func (s *Server) paypalGetPlan(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != s.cfg.PayPalPlanID {
		paypalError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "INVALID_RESOURCE_ID", "Specified resource ID does not exist.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         s.cfg.PayPalPlanID,
		"product_id": "PROD-LOCALSANDBOX",
		"name":       "Sandbox monthly plan",
		"status":     "ACTIVE",
		"billing_cycles": []map[string]any{{
			"frequency":      map[string]any{"interval_unit": PlanInterval, "interval_count": 1},
			"tenure_type":    "REGULAR",
			"sequence":       1,
			"total_cycles":   0,
			"pricing_scheme": map[string]any{"fixed_price": ppAmount{CurrencyCode: PlanCurrency, Value: PlanAmount}},
		}},
	})
}

func (s *Server) paypalCreateSubscription(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanID   string `json:"plan_id"`
		CustomID string `json:"custom_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.PlanID != s.cfg.PayPalPlanID {
		paypalError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "INVALID_PLAN", "The plan does not exist.")
		return
	}
	id := "I-" + randomString(upperAlnum, 12)
	sub := &ppSubscription{
		ID:       id,
		Status:   "APPROVAL_PENDING",
		PlanID:   req.PlanID,
		CustomID: req.CustomID,
		Links: []ppLink{
			{Href: paypalCheckoutHost + "/webapps/billing/subscriptions?ba_token=BA-" + randomString(upperAlnum, 17), Rel: "approve", Method: "GET"},
			{Href: s.cfg.PublicURL + "/v1/billing/subscriptions/" + id, Rel: "self", Method: "GET"},
		},
		CreateTime: now3339(),
	}
	s.subscriptions[id] = sub
	writeRaw(w, http.StatusCreated, mustJSON(sub))
}

func (s *Server) paypalGetSubscription(w http.ResponseWriter, r *http.Request) {
	s.getJSON(w, func() (any, bool) { sub, ok := s.subscriptions[r.PathValue("id")]; return sub, ok })
}

func (s *Server) paypalRefundCapture(w http.ResponseWriter, r *http.Request) {
	captureID := r.PathValue("id")
	var req struct {
		Amount   ppAmount `json:"amount"`
		CustomID string   `json:"custom_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	amount, err := money.Parse(req.Amount.Value, req.Amount.CurrencyCode)
	if err != nil || amount <= 0 {
		paypalError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "INVALID_PARAMETER_VALUE", "The refund amount is invalid.")
		return
	}

	s.mu.Lock()
	captured, ok := s.findCapture(captureID)
	if !ok {
		s.mu.Unlock()
		paypalError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "INVALID_RESOURCE_ID", "Specified resource ID does not exist.")
		return
	}
	paid, _ := money.Parse(captured.Amount.Value, captured.Amount.CurrencyCode)
	var refunded int64
	for _, ref := range s.ppRefunds {
		if ref.captureID == captureID {
			n, _ := money.Parse(ref.Amount.Value, ref.Amount.CurrencyCode)
			refunded += n
		}
	}
	if amount > paid-refunded {
		s.mu.Unlock()
		paypalError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "REFUND_AMOUNT_EXCEEDED", "The refund amount must be less than or equal to the capture amount that has not yet been refunded.")
		return
	}
	ref := &ppRefund{
		ID:         randomString(upperAlnum, 17),
		Status:     "COMPLETED",
		Amount:     req.Amount,
		CustomID:   req.CustomID,
		CreateTime: now3339(),
		captureID:  captureID,
	}
	s.ppRefunds[ref.ID] = ref
	body := mustJSON(ref)
	s.mu.Unlock()

	writeRaw(w, http.StatusCreated, body)
	s.deliverSoon(*newEvent(paypal.Name, "PAYMENT.CAPTURE.REFUNDED", body))
}

// findCapture looks a capture up across orders. The caller holds s.mu.
func (s *Server) findCapture(id string) (ppCapture, bool) {
	for _, o := range s.orders {
		for _, pu := range o.PurchaseUnits {
			if pu.Payments == nil {
				continue
			}
			for _, c := range pu.Payments.Captures {
				if c.ID == id {
					return c, true
				}
			}
		}
	}
	return ppCapture{}, false
}

func (s *Server) paypalGetRefund(w http.ResponseWriter, r *http.Request) {
	s.getJSON(w, func() (any, bool) { ref, ok := s.ppRefunds[r.PathValue("id")]; return ref, ok })
}

// getJSON writes the object find returns, read under the lock.
func (s *Server) getJSON(w http.ResponseWriter, find func() (any, bool)) {
	s.mu.Lock()
	v, ok := find()
	var body []byte
	if ok {
		body = mustJSON(v)
	}
	s.mu.Unlock()
	if !ok {
		paypalError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "INVALID_RESOURCE_ID", "Specified resource ID does not exist.")
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// advanceOrder moves an order awaiting the buyer on. The caller holds s.mu.
func (s *Server) advanceOrder(o *ppOrder, action string) (*event, error) {
	if o.Status != "PAYER_ACTION_REQUIRED" {
		return nil, fmt.Errorf("order is %s, not awaiting the buyer", o.Status)
	}
	switch action {
	case "expire":
		// PayPal sends no webhook when a buyer abandons an order.
		o.Status = "VOIDED"
		return nil, nil
	case "fail":
		// The buyer approves, but the funding source is declined at capture.
		s.declineCapture[o.ID] = true
	}
	o.Status = "APPROVED"
	return newEvent(paypal.Name, "CHECKOUT.ORDER.APPROVED", mustJSON(o)), nil
}

// advanceSubscription moves a subscription awaiting approval on. The caller
// holds s.mu.
func (s *Server) advanceSubscription(sub *ppSubscription, action string) (*event, error) {
	if sub.Status != "APPROVAL_PENDING" {
		return nil, fmt.Errorf("subscription is %s, not awaiting approval", sub.Status)
	}
	types := map[string]struct{ status, event string }{
		"pay":    {"ACTIVE", "BILLING.SUBSCRIPTION.ACTIVATED"},
		"fail":   {"CANCELLED", "BILLING.SUBSCRIPTION.CANCELLED"},
		"expire": {"EXPIRED", "BILLING.SUBSCRIPTION.EXPIRED"},
	}
	t := types[action]
	sub.Status = t.status
	return newEvent(paypal.Name, t.event, mustJSON(sub)), nil
}

var paypalResourceTypes = map[string]string{
	"CHECKOUT.ORDER.APPROVED":   "checkout-order",
	"PAYMENT.CAPTURE.COMPLETED": "capture",
	"PAYMENT.CAPTURE.REFUNDED":  "refund",
}

func (s *Server) paypalEnvelope(ev event) []byte {
	resourceType, ok := paypalResourceTypes[ev.typ]
	if !ok {
		resourceType = "subscription"
	}
	return mustJSON(map[string]any{
		"id":            ev.id,
		"event_version": "1.0",
		"create_time":   now3339(),
		"resource_type": resourceType,
		"event_type":    ev.typ,
		"summary":       ev.typ,
		"resource":      ev.resource,
	})
}

func (s *Server) certURL() string {
	return s.cfg.PublicURL + "/v1/notifications/certs/CERT-sandbox"
}

func (s *Server) paypalHeaders(body []byte) http.Header {
	id := randomString(lowerAlnum, 32)
	at := now3339()
	return http.Header{
		"Paypal-Transmission-Id":   {id},
		"Paypal-Transmission-Time": {at},
		"Paypal-Transmission-Sig":  {s.paypalSignature(id, at, s.cfg.PayPalWebhookID, body)},
		"Paypal-Cert-Url":          {s.certURL()},
		"Paypal-Auth-Algo":         {"SHA256withRSA"},
	}
}

// paypalSignature signs the fields PayPal signs: transmission id and time,
// webhook id and a CRC32 of the body. PayPal uses an RSA certificate; the
// sandbox is the only party that ever checks its own signatures, so a
// keyed HMAC stands in. The gateway's code path is the same either way.
//
// As at PayPal, the CRC covers the exact bytes, so a client that re-encodes
// the event when verifying it fails the check.
func (s *Server) paypalSignature(transmissionID, transmissionTime, webhookID string, body []byte) string {
	msg := fmt.Sprintf("%s|%s|%s|%d", transmissionID, transmissionTime, webhookID, crc32.ChecksumIEEE(body))
	mac := hmac.New(sha256.New, s.signingKey)
	mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) paypalVerifySignature(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AuthAlgo         string          `json:"auth_algo"`
		CertURL          string          `json:"cert_url"`
		TransmissionID   string          `json:"transmission_id"`
		TransmissionSig  string          `json:"transmission_sig"`
		TransmissionTime string          `json:"transmission_time"`
		WebhookID        string          `json:"webhook_id"`
		WebhookEvent     json.RawMessage `json:"webhook_event"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	expected := s.paypalSignature(req.TransmissionID, req.TransmissionTime, req.WebhookID, req.WebhookEvent)
	status := "FAILURE"
	if req.AuthAlgo == "SHA256withRSA" && req.CertURL == s.certURL() && req.WebhookID == s.cfg.PayPalWebhookID &&
		hmac.Equal([]byte(req.TransmissionSig), []byte(expected)) {
		status = "SUCCESS"
	}
	writeJSON(w, http.StatusOK, map[string]string{"verification_status": status})
}

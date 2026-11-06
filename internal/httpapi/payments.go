package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
	"github.com/tancon23/unified-payments-assessment/internal/payment"
)

const (
	maxBodyBytes          = 1 << 20
	maxIdempotencyKeyLen  = 255
	idempotentReplayedHdr = "Idempotent-Replayed"
)

type checkoutRequest struct {
	Provider    string `json:"provider"`
	Mode        string `json:"mode"`
	Amount      int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Interval    string `json:"interval"`
	Description string `json:"description"`
	SuccessURL  string `json:"success_url"`
	CancelURL   string `json:"cancel_url"`
}

type refundRequest struct {
	Amount int64 `json:"amount_minor"`
}

type paymentResponse struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	Mode           string `json:"mode"`
	Amount         int64  `json:"amount_minor"`
	Currency       string `json:"currency"`
	Interval       string `json:"interval,omitempty"`
	Description    string `json:"description"`
	Status         string `json:"status"`
	AmountRefunded int64  `json:"refunded_minor"`
	CheckoutURL    string `json:"checkout_url,omitempty"`
	CheckoutRef    string `json:"provider_checkout_id,omitempty"`
	PaymentRef     string `json:"provider_payment_id,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
	followupResponse
	Refunds   []refundResponse `json:"refunds"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

type refundResponse struct {
	ID            string `json:"id"`
	PaymentID     string `json:"payment_id"`
	Amount        int64  `json:"amount_minor"`
	Status        string `json:"status"`
	ProviderRef   string `json:"provider_refund_id,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	followupResponse
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// followupResponse tells the client the gateway is still working on the
// item, or has handed it to a person.
type followupResponse struct {
	Attempts   int    `json:"attempts"`
	Flagged    bool   `json:"flagged"`
	FlagReason string `json:"flag_reason,omitempty"`
}

func toFollowupResponse(f payment.Followup) followupResponse {
	return followupResponse{Attempts: f.Attempts, Flagged: f.Flagged, FlagReason: f.FlagReason}
}

func toPaymentResponse(p payment.Payment) paymentResponse {
	out := paymentResponse{
		ID:               p.ID,
		Provider:         p.Provider,
		Mode:             string(p.Mode),
		Amount:           p.Amount,
		Currency:         p.Currency,
		Interval:         string(p.Interval),
		Description:      p.Description,
		Status:           string(p.Status),
		AmountRefunded:   p.AmountRefunded,
		CheckoutURL:      p.CheckoutURL,
		CheckoutRef:      p.CheckoutRef,
		PaymentRef:       p.PaymentRef,
		FailureReason:    p.FailureReason,
		followupResponse: toFollowupResponse(p.Followup),
		Refunds:          make([]refundResponse, 0, len(p.Refunds)),
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
	for _, r := range p.Refunds {
		out.Refunds = append(out.Refunds, toRefundResponse(r))
	}
	return out
}

func toRefundResponse(r payment.Refund) refundResponse {
	return refundResponse{
		ID:               r.ID,
		PaymentID:        r.PaymentID,
		Amount:           r.Amount,
		Status:           string(r.Status),
		ProviderRef:      r.ProviderRef,
		FailureReason:    r.FailureReason,
		followupResponse: toFollowupResponse(r.Followup),
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

func (h *handler) createCheckout(c *gin.Context) {
	key, ok := h.idempotencyKey(c)
	if !ok {
		return
	}
	var req checkoutRequest
	if !h.decode(c, &req) {
		return
	}

	p, replayed, err := h.svc.CreateCheckout(c.Request.Context(), tenant(c), key, payment.CheckoutRequest{
		Provider:    req.Provider,
		Mode:        gateway.Mode(req.Mode),
		Amount:      req.Amount,
		Currency:    req.Currency,
		Interval:    gateway.Interval(req.Interval),
		Description: req.Description,
		SuccessURL:  req.SuccessURL,
		CancelURL:   req.CancelURL,
	})
	switch {
	case err != nil:
		h.failWith(c, err, "payment", p.ID, func() any { return toPaymentResponse(p) })
	case p.Status == payment.StatusUnknown:
		// The provider has not answered; the worker finishes the job.
		h.respond(c, http.StatusAccepted, replayed, toPaymentResponse(p))
	case replayed:
		h.respond(c, http.StatusOK, true, toPaymentResponse(p))
	default:
		h.respond(c, http.StatusCreated, false, toPaymentResponse(p))
	}
}

func (h *handler) getPayment(c *gin.Context) {
	p, err := h.svc.GetPayment(c.Request.Context(), tenant(c), c.Param("id"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toPaymentResponse(p))
}

func (h *handler) createRefund(c *gin.Context) {
	key, ok := h.idempotencyKey(c)
	if !ok {
		return
	}
	var req refundRequest
	if !h.decode(c, &req) {
		return
	}

	r, replayed, err := h.svc.Refund(c.Request.Context(), tenant(c), c.Param("id"), key, payment.RefundRequest{Amount: req.Amount})
	switch {
	case err != nil:
		h.failWith(c, err, "refund", r.ID, func() any { return toRefundResponse(r) })
	case r.Status.Open():
		// Not confirmed yet; a webhook or the worker settles it.
		h.respond(c, http.StatusAccepted, replayed, toRefundResponse(r))
	case replayed:
		h.respond(c, http.StatusOK, true, toRefundResponse(r))
	default:
		h.respond(c, http.StatusCreated, false, toRefundResponse(r))
	}
}

func (h *handler) respond(c *gin.Context, status int, replayed bool, body any) {
	if replayed {
		c.Header(idempotentReplayedHdr, "true")
	}
	c.JSON(status, body)
}

func (h *handler) idempotencyKey(c *gin.Context) (string, bool) {
	key := c.GetHeader("Idempotency-Key")
	if key == "" || len(key) > maxIdempotencyKeyLen {
		h.abort(c, http.StatusBadRequest, "invalid_request", "Idempotency-Key header is required (at most 255 characters)")
		return "", false
	}
	return key, true
}

// decode reads a JSON body strictly: unknown fields and anything after the
// object are errors, and an empty body decodes to the zero value.
func (h *handler) decode(c *gin.Context, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(into)
	if errors.Is(err, io.EOF) {
		return true
	}
	if err == nil && dec.More() {
		err = errors.New("unexpected data after the JSON object")
	}
	if err != nil {
		h.abort(c, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

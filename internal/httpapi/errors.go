package httpapi

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
	"github.com/tanweerdev/unified-payments-assessment/internal/payment"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorStatus maps a service error to an HTTP status and a stable code.
func errorStatus(err error) (int, string) {
	var validation *payment.ValidationError
	switch {
	case errors.As(err, &validation):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, payment.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, payment.ErrUnknownProvider):
		return http.StatusNotFound, "unknown_provider"
	case errors.Is(err, payment.ErrInvalidWebhook):
		return http.StatusBadRequest, "invalid_webhook"
	case errors.Is(err, payment.ErrIdempotencyMismatch):
		return http.StatusUnprocessableEntity, "idempotency_key_reused"
	case errors.Is(err, payment.ErrUnsupported):
		return http.StatusUnprocessableEntity, "unsupported"
	case errors.Is(err, payment.ErrInProgress):
		return http.StatusConflict, "request_in_progress"
	case errors.Is(err, payment.ErrInvalidState):
		return http.StatusConflict, "invalid_state"
	case errors.Is(err, payment.ErrProviderUnavailable):
		return http.StatusServiceUnavailable, "provider_unavailable"
	case gateway.IsRejected(err):
		return http.StatusUnprocessableEntity, "provider_rejected"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

func (h *handler) fail(c *gin.Context, err error) {
	h.failWith(c, err, "", "", nil)
}

// failWith writes err as a JSON error. When the request got far enough to
// create a resource, that resource is included under resourceKey so the
// client learns its id and status.
func (h *handler) failWith(c *gin.Context, err error, resourceKey, resourceID string, resource func() any) {
	status, code := errorStatus(err)
	msg := err.Error()
	if status == http.StatusInternalServerError {
		h.log.Error("request failed", "route", c.FullPath(), "err", err)
		msg = "internal error"
	}
	if status == http.StatusServiceUnavailable {
		c.Header("Retry-After", "5")
	}
	body := gin.H{"error": apiError{Code: code, Message: msg}}
	if resourceID != "" && resource != nil {
		body[resourceKey] = resource()
	}
	c.JSON(status, body)
}

func (h *handler) abort(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": apiError{Code: code, Message: msg}})
}

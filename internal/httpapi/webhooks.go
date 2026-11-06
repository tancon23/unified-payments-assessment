package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tanweerdev/unified-payments-assessment/internal/payment"
)

func (h *handler) webhook(c *gin.Context) {
	start := time.Now()
	provider := c.Param("provider")

	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes))
	if err != nil {
		h.abort(c, http.StatusBadRequest, "invalid_request", "unreadable body")
		return
	}

	res, err := h.svc.HandleWebhook(c.Request.Context(), provider, c.Request.Header, body)
	h.metrics.observe(provider, err, res.Outcome, time.Since(start))
	if err != nil {
		if errors.Is(err, payment.ErrInvalidWebhook) {
			h.log.Warn("rejected webhook", "provider", provider, "remote", c.ClientIP(), "err", err)
		}
		// A non-2xx answer makes the provider redeliver, which is what a
		// verification outage or database error needs.
		h.fail(c, err)
		return
	}

	h.log.Info("webhook processed",
		"provider", provider, "event_id", res.EventID, "event_type", res.EventType,
		"outcome", res.Outcome, "detail", res.Detail)
	c.JSON(http.StatusOK, gin.H{"event_id": res.EventID, "outcome": res.Outcome, "detail": res.Detail})
}

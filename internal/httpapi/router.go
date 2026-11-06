// Package httpapi exposes the payment service over HTTP.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tancon23/unified-payments-assessment/internal/payment"
)

// TenantAuthenticator resolves an API key to the tenant that owns it.
type TenantAuthenticator interface {
	TenantForAPIKey(ctx context.Context, apiKey string) (tenantID string, found bool, err error)
}

type Deps struct {
	Service  *payment.Service
	Tenants  TenantAuthenticator
	Metrics  *Metrics
	Gatherer prometheus.Gatherer
	Log      *slog.Logger
}

type handler struct {
	svc     *payment.Service
	tenants TenantAuthenticator
	metrics *Metrics
	log     *slog.Logger
}

// NewRouter returns the API:
//
//	POST /v1/checkouts               open a checkout (tenant, idempotent)
//	GET  /v1/payments/:id            read a payment and its refunds (tenant)
//	POST /v1/payments/:id/refunds    refund a payment (tenant, idempotent)
//	POST /v1/webhooks/:provider      provider webhooks (signature-checked)
//	GET  /healthz, GET /metrics
func NewRouter(d Deps) http.Handler {
	h := &handler{svc: d.Service, tenants: d.Tenants, metrics: d.Metrics, log: d.Log}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), h.logRequests)

	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/metrics", gin.WrapH(promhttp.HandlerFor(d.Gatherer, promhttp.HandlerOpts{})))
	r.POST("/v1/webhooks/:provider", h.webhook)

	api := r.Group("/v1", h.authenticate)
	api.POST("/checkouts", h.createCheckout)
	api.GET("/payments/:id", h.getPayment)
	api.POST("/payments/:id/refunds", h.createRefund)
	return r
}

const tenantKey = "tenant"

// authenticate resolves the X-API-Key header to a tenant. Every tenant
// route reads its tenant from here and nowhere else.
func (h *handler) authenticate(c *gin.Context) {
	key := c.GetHeader("X-API-Key")
	if key == "" {
		h.abort(c, http.StatusUnauthorized, "unauthenticated", "X-API-Key header is required")
		return
	}
	tenantID, found, err := h.tenants.TenantForAPIKey(c.Request.Context(), key)
	switch {
	case err != nil:
		h.fail(c, err)
		c.Abort()
	case !found:
		h.abort(c, http.StatusUnauthorized, "unauthenticated", "unknown API key")
	default:
		c.Set(tenantKey, tenantID)
		c.Next()
	}
}

func tenant(c *gin.Context) string { return c.GetString(tenantKey) }

func (h *handler) logRequests(c *gin.Context) {
	start := time.Now()
	c.Next()
	h.log.Info("request",
		"method", c.Request.Method,
		"route", c.FullPath(),
		"status", c.Writer.Status(),
		"duration", time.Since(start),
		"tenant", tenant(c))
}

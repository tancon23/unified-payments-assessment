package httpapi

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tancon23/unified-payments-assessment/internal/payment"
)

// Metrics counts webhook deliveries by provider and outcome and times them.
type Metrics struct {
	webhooks *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "paygate_webhooks_total",
			Help: "Webhook deliveries by provider and outcome.",
		}, []string{"provider", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "paygate_webhook_duration_seconds",
			Help:    "Time to verify and apply a webhook delivery.",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider"}),
	}
	reg.MustRegister(m.webhooks, m.duration)
	return m
}

// webhookErrorOutcomes labels deliveries that failed before an outcome.
var webhookErrorOutcomes = []struct {
	err   error
	label string
}{
	{payment.ErrInvalidWebhook, "invalid"},
	{payment.ErrUnknownProvider, "unknown_provider"},
	{payment.ErrProviderUnavailable, "verification_unavailable"},
}

func (m *Metrics) observe(provider string, err error, outcome payment.Outcome, took time.Duration) {
	label := string(outcome)
	if err != nil {
		label = "error"
		for _, o := range webhookErrorOutcomes {
			if errors.Is(err, o.err) {
				label = o.label
				break
			}
		}
	}
	if errors.Is(err, payment.ErrUnknownProvider) {
		provider = "unknown" // the path segment is caller-controlled; keep label values bounded
	}
	m.webhooks.WithLabelValues(provider, label).Inc()
	m.duration.WithLabelValues(provider).Observe(took.Seconds())
}

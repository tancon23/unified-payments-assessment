package providertest

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/sandbox"
)

// Sandbox runs the local provider stand-in for a test and collects the
// webhooks it sends.
type Sandbox struct {
	URL    string
	Server *sandbox.Server

	mu         sync.Mutex
	deliveries []Delivery
	arrived    chan struct{}
}

// NewSandbox starts the stand-in with cfg, sending its webhooks to the
// Sandbox instead of a gateway.
func NewSandbox(t *testing.T, cfg sandbox.Config) *Sandbox {
	t.Helper()
	s := &Sandbox{arrived: make(chan struct{}, 1)}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.deliveries = append(s.deliveries, Delivery{Header: r.Header.Clone(), Body: body})
		s.mu.Unlock()
		select {
		case s.arrived <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(sink.Close)

	cfg.StripeWebhookURL, cfg.PayPalWebhookURL = sink.URL, sink.URL
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	s.Server = sandbox.New(cfg)
	srv := httptest.NewServer(s.Server)
	t.Cleanup(func() {
		s.Server.Close()
		srv.Close()
	})
	s.URL = srv.URL
	return s
}

// Pay has the customer pay a checkout and returns the webhook the stand-in
// sent about it.
func (s *Sandbox) Pay(t *testing.T, checkoutRef string) Delivery {
	t.Helper()
	resp, err := http.Post(s.URL+"/sandbox/checkouts/"+checkoutRef+"/pay", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("paying %s: HTTP %d", checkoutRef, resp.StatusCode)
	}
	return s.Next(t, checkoutRef)
}

// Next returns the first webhook not yet taken that mentions ref, waiting
// up to five seconds for it.
func (s *Sandbox) Next(t *testing.T, ref string) Delivery {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		for i, d := range s.deliveries {
			if bytes.Contains(d.Body, []byte(ref)) {
				s.deliveries = append(s.deliveries[:i], s.deliveries[i+1:]...)
				s.mu.Unlock()
				return d
			}
		}
		s.mu.Unlock()
		select {
		case <-s.arrived:
		case <-deadline:
			t.Fatalf("no webhook about %s arrived", ref)
		}
	}
}

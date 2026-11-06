package payment

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

// CheckoutRequest asks for a hosted checkout with one provider.
type CheckoutRequest struct {
	Provider    string
	Mode        gateway.Mode
	Amount      int64 // minor units
	Currency    string
	Interval    gateway.Interval
	Description string
	SuccessURL  string
	CancelURL   string
}

const (
	maxAmount         = 99_999_999 // the lowest per-charge ceiling across providers
	maxDescriptionLen = 127        // PayPal's limit for item names
)

func (r CheckoutRequest) normalized() CheckoutRequest {
	r.Currency = strings.ToLower(strings.TrimSpace(r.Currency))
	r.Description = strings.TrimSpace(r.Description)
	if r.Description == "" {
		r.Description = "Payment"
		if r.Mode == gateway.ModeSubscription {
			r.Description = "Subscription"
		}
	}
	return r
}

func (r CheckoutRequest) validate(providers *gateway.Registry) error {
	if _, ok := providers.Get(r.Provider); !ok {
		return &ValidationError{"provider", "must be one of " + strings.Join(providers.Names(), ", ")}
	}
	switch r.Mode {
	case gateway.ModePayment:
		if r.Interval != "" {
			return &ValidationError{"interval", "is only allowed for subscriptions"}
		}
	case gateway.ModeSubscription:
		switch r.Interval {
		case gateway.IntervalDay, gateway.IntervalWeek, gateway.IntervalMonth, gateway.IntervalYear:
		default:
			return &ValidationError{"interval", "must be day, week, month or year"}
		}
	default:
		return &ValidationError{"mode", "must be payment or subscription"}
	}
	if r.Amount < 1 || r.Amount > maxAmount {
		return &ValidationError{"amount_minor", fmt.Sprintf("must be between 1 and %d", maxAmount)}
	}
	if len(r.Currency) != 3 || strings.Trim(r.Currency, "abcdefghijklmnopqrstuvwxyz") != "" {
		return &ValidationError{"currency", "must be a three-letter ISO 4217 code"}
	}
	if len(r.Description) > maxDescriptionLen {
		return &ValidationError{"description", fmt.Sprintf("must be at most %d characters", maxDescriptionLen)}
	}
	for field, raw := range map[string]string{"success_url": r.SuccessURL, "cancel_url": r.CancelURL} {
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return &ValidationError{field, "must be an absolute http(s) URL"}
		}
	}
	return nil
}

// CreateCheckout records a payment and opens a hosted checkout for it. It
// makes one call to the provider; when that call gets no answer, the
// payment comes back as unknown and the worker finishes the job. Repeating
// a call with the same idempotency key returns the stored payment with
// replayed=true; the same key with a different request is refused.
func (s *Service) CreateCheckout(ctx context.Context, tenantID, idempotencyKey string, req CheckoutRequest) (p Payment, replayed bool, err error) {
	req = req.normalized()
	if err := req.validate(s.providers); err != nil {
		return Payment{}, false, err
	}
	hash := hashRequest(req)

	var created bool
	err = s.store.Tx(ctx, func(tx Tx) error {
		var err error
		p, created, err = tx.CreatePayment(ctx, Payment{
			ID:             newID("pay"),
			TenantID:       tenantID,
			IdempotencyKey: idempotencyKey,
			RequestHash:    hash,
			Provider:       req.Provider,
			Mode:           req.Mode,
			Amount:         req.Amount,
			Currency:       req.Currency,
			Interval:       req.Interval,
			Description:    req.Description,
			SuccessURL:     req.SuccessURL,
			CancelURL:      req.CancelURL,
			Status:         StatusPending,
			// The worker picks the payment up if this request dies before
			// recording the provider's answer.
			Followup: Followup{NextCheckAt: s.cfg.Now().Add(s.claimTTL())},
		})
		return err
	})
	if err != nil {
		return Payment{}, false, err
	}
	if !created {
		if !bytes.Equal(p.RequestHash, hash) {
			return Payment{}, false, ErrIdempotencyMismatch
		}
		return p, true, nil
	}

	provider, _ := s.providers.Get(req.Provider)
	p, c, err := s.claimPayment(ctx, p.ID)
	if err != nil {
		return Payment{}, false, err
	}
	p, err = s.openCheckout(ctx, provider, p, c)
	return p, false, err
}

// needsCheckout reports whether a payment still waits for its checkout page.
func needsCheckout(p Payment) bool {
	return p.CheckoutRef == "" && (p.Status == StatusPending || p.Status == StatusUnknown) && !p.Flagged
}

// openCheckout asks the provider for a checkout page for a claimed payment.
// The provider idempotency key derives from the payment id, so asking again
// after an unanswered call returns the checkout the provider already made.
func (s *Service) openCheckout(ctx context.Context, provider gateway.Provider, p Payment, c claim) (Payment, error) {
	if !needsCheckout(p) {
		return p, s.release(ctx, c) // someone else finished first
	}

	var co gateway.Checkout
	callErr := s.call(ctx, func(ctx context.Context) (err error) {
		co, err = provider.CreateCheckout(ctx, gateway.CheckoutParams{
			IdempotencyKey: "checkout-" + p.ID,
			PaymentID:      p.ID,
			Mode:           p.Mode,
			Amount:         p.Amount,
			Currency:       p.Currency,
			Interval:       p.Interval,
			Description:    p.Description,
			SuccessURL:     p.SuccessURL,
			CancelURL:      p.CancelURL,
		})
		return err
	})

	err := s.settle(ctx, c, func(ctx context.Context, tx Tx) error {
		cur, err := tx.LockPayment(ctx, p.ID)
		if err != nil || !needsCheckout(cur) {
			return err
		}
		switch {
		case callErr == nil:
			if err := cur.moveTo(StatusPending); err != nil {
				return err
			}
			cur.CheckoutRef, cur.CheckoutURL = co.Ref, co.URL
			cur.Attempts = 0
			cur.NextCheckAt = s.cfg.Now().Add(s.cfg.FirstCheck)
		case gateway.IsRejected(callErr):
			failPayment(&cur, callErr.Error())
		default:
			if err := cur.moveTo(StatusUnknown); err != nil {
				return err
			}
			s.noAnswer(&cur.Followup, "create checkout", "payment", cur.ID, callErr)
		}
		p = cur
		return tx.UpdatePayment(ctx, cur)
	})
	if err != nil {
		return p, err
	}
	if gateway.IsRejected(callErr) {
		return p, callErr
	}
	return p, nil
}

// applyCheckout moves a payment to the state its provider reported. Every
// status change after a checkout opens goes through here, whether the news
// came from a webhook, a capture or the worker.
func (s *Service) applyCheckout(ctx context.Context, tx Tx, p Payment, st gateway.CheckoutState, reason string) (Payment, Result, error) {
	next, ok := checkoutStatuses[st.Status]
	if !ok {
		return p, ignored("checkout is still open"), nil
	}
	if st.Amount != 0 && (st.Amount != p.Amount || !strings.EqualFold(st.Currency, p.Currency)) {
		s.log.Error("provider amount does not match payment",
			"payment", p.ID, "expected", p.Amount, "expected_currency", p.Currency,
			"reported", st.Amount, "reported_currency", st.Currency)
		return p, rejected("provider reported %d %s for a payment of %d %s", st.Amount, st.Currency, p.Amount, p.Currency), nil
	}
	if p.Status == next {
		return p, ignored("payment is already %s", next), nil
	}
	if !p.Status.CanBecome(next) {
		return p, ignored("payment is %s and cannot become %s", p.Status, next), nil
	}

	if err := p.moveTo(next); err != nil {
		return p, Result{}, err
	}
	if st.PaymentRef != "" {
		p.PaymentRef = st.PaymentRef
	}
	if next == StatusFailed {
		p.FailureReason = cmp.Or(reason, "provider reported the payment failed")
	}
	return p, applied("payment %s", next), tx.UpdatePayment(ctx, p)
}

// failPayment marks a payment failed after the provider refused it.
func failPayment(p *Payment, reason string) bool {
	if p.moveTo(StatusFailed) != nil {
		return false
	}
	p.FailureReason = reason
	return true
}

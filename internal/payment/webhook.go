package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

// Outcome says what applying a piece of provider news did.
type Outcome string

const (
	OutcomeApplied   Outcome = "applied"   // state changed
	OutcomeDuplicate Outcome = "duplicate" // event already processed
	OutcomeIgnored   Outcome = "ignored"   // nothing to change
	OutcomeRejected  Outcome = "rejected"  // contradicts what the gateway knows, e.g. a different amount
)

type Result struct {
	Outcome Outcome
	Detail  string
}

func (r Result) String() string { return string(r.Outcome) + ": " + r.Detail }

func applied(format string, args ...any) Result {
	return Result{OutcomeApplied, fmt.Sprintf(format, args...)}
}

func ignored(format string, args ...any) Result {
	return Result{OutcomeIgnored, fmt.Sprintf(format, args...)}
}

func rejected(format string, args ...any) Result {
	return Result{OutcomeRejected, fmt.Sprintf(format, args...)}
}

// WebhookResult is the outcome of one webhook delivery.
type WebhookResult struct {
	EventID   string
	EventType string
	Result
}

var eventCheckoutStatus = map[gateway.EventKind]gateway.CheckoutStatus{
	gateway.EventCheckoutAuthorized: gateway.CheckoutAuthorized,
	gateway.EventCheckoutPaid:       gateway.CheckoutPaid,
	gateway.EventCheckoutFailed:     gateway.CheckoutFailed,
	gateway.EventCheckoutExpired:    gateway.CheckoutExpired,
}

var eventRefundStatus = map[gateway.EventKind]gateway.RefundStatus{
	gateway.EventRefundSucceeded: gateway.RefundSucceeded,
	gateway.EventRefundFailed:    gateway.RefundFailed,
}

// HandleWebhook verifies a delivery and applies it exactly once. The event
// id is recorded in the same transaction as the state change, so a
// redelivered event, even one arriving concurrently, changes nothing.
func (s *Service) HandleWebhook(ctx context.Context, providerName string, header http.Header, body []byte) (WebhookResult, error) {
	provider, ok := s.providers.Get(providerName)
	if !ok {
		return WebhookResult{}, ErrUnknownProvider
	}
	// Verification can call the provider (PayPal), so it gets the same bound
	// as any other provider call.
	verifyCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	ev, err := provider.VerifyWebhook(verifyCtx, header, body)
	cancel()
	switch {
	case errors.Is(err, gateway.ErrTransient):
		return WebhookResult{}, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	case err != nil:
		return WebhookResult{}, fmt.Errorf("%w: %v", ErrInvalidWebhook, err)
	}

	out := WebhookResult{EventID: ev.ID, EventType: ev.Type}
	var paymentID string
	err = s.store.Tx(ctx, func(tx Tx) error {
		fresh, err := tx.RecordEvent(ctx, providerName, ev)
		if err != nil {
			return err
		}
		if !fresh {
			out.Result = Result{OutcomeDuplicate, "event already processed"}
			return nil
		}
		if out.Result, paymentID, err = s.applyEvent(ctx, tx, providerName, ev); err != nil {
			return err
		}
		return tx.SetEventOutcome(ctx, providerName, ev.ID, out.Result.String())
	})
	if err != nil {
		return WebhookResult{}, err
	}

	if ev.Kind == gateway.EventCheckoutAuthorized && out.Outcome == OutcomeApplied {
		// Capture now rather than waiting for the worker. A capture that gets
		// no answer leaves the payment authorized, and the worker retries it.
		if err := s.capture(ctx, provider, paymentID); err != nil {
			s.log.Warn("capture after approval failed", "payment", paymentID, "err", err)
		}
	}
	return out, nil
}

func (s *Service) applyEvent(ctx context.Context, tx Tx, provider string, ev gateway.Event) (Result, string, error) {
	if status, ok := eventCheckoutStatus[ev.Kind]; ok {
		p, err := tx.LockPaymentForEvent(ctx, provider, ev.PaymentID, ev.CheckoutRef)
		if errors.Is(err, ErrNotFound) {
			return ignored("no %s payment matches the event", provider), "", nil
		}
		if err != nil {
			return Result{}, "", err
		}
		st := gateway.CheckoutState{Status: status, PaymentRef: ev.PaymentRef, Amount: ev.Amount, Currency: ev.Currency}
		_, res, err := s.applyCheckout(ctx, tx, p, st, "")
		return res, p.ID, err
	}

	if status, ok := eventRefundStatus[ev.Kind]; ok {
		found, err := tx.FindRefundForEvent(ctx, provider, ev.RefundID, ev.RefundRef)
		if errors.Is(err, ErrNotFound) {
			return ignored("no %s refund matches the event", provider), "", nil
		}
		if err != nil {
			return Result{}, "", err
		}
		p, r, err := lockRefund(ctx, tx, found)
		if err != nil {
			return Result{}, "", err
		}
		if ev.Amount != 0 && ev.Amount != r.Amount {
			s.log.Error("provider refund amount does not match refund", "refund", r.ID, "expected", r.Amount, "reported", ev.Amount)
			return rejected("provider reported a refund of %d for a refund of %d", ev.Amount, r.Amount), p.ID, nil
		}
		_, res, err := s.applyRefund(ctx, tx, p, r, gateway.Refund{Ref: ev.RefundRef, Status: status}, "")
		return res, p.ID, err
	}

	return ignored("event type %s is not tracked", ev.Type), "", nil
}

// capture settles an authorized checkout with a provider that needs an
// explicit capture. Providers that capture on their own are left alone.
func (s *Service) capture(ctx context.Context, provider gateway.Provider, paymentID string) error {
	capturer, ok := provider.(gateway.Capturer)
	if !ok {
		return nil
	}
	p, c, err := s.claimPayment(ctx, paymentID)
	if err != nil {
		return err
	}
	return s.captureClaimed(ctx, capturer, p, c)
}

// captureClaimed captures a claimed, authorized payment and records the
// answer. The capture key derives from the payment id, so capturing again
// after an unanswered call cannot take the money twice.
func (s *Service) captureClaimed(ctx context.Context, capturer gateway.Capturer, p Payment, c claim) error {
	if p.Status != StatusAuthorized || p.Flagged {
		return s.release(ctx, c)
	}

	var st gateway.CheckoutState
	callErr := s.call(ctx, func(ctx context.Context) (err error) {
		st, err = capturer.Capture(ctx, gateway.CaptureParams{IdempotencyKey: "capture-" + p.ID, CheckoutRef: p.CheckoutRef})
		return err
	})

	return s.settle(ctx, c, func(ctx context.Context, tx Tx) error {
		cur, err := tx.LockPayment(ctx, p.ID)
		if err != nil || cur.Status != StatusAuthorized {
			return err
		}
		switch {
		case callErr == nil:
			_, _, err = s.applyCheckout(ctx, tx, cur, st, "")
		case gateway.IsRejected(callErr):
			_, _, err = s.applyCheckout(ctx, tx, cur, gateway.CheckoutState{Status: gateway.CheckoutFailed}, callErr.Error())
		default:
			s.noAnswer(&cur.Followup, "capture", "payment", cur.ID, callErr)
			err = tx.UpdatePayment(ctx, cur)
		}
		return err
	})
}

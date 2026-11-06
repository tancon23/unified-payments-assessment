package payment

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

// RefundRequest asks for money back on a payment.
type RefundRequest struct {
	Amount int64 // minor units; 0 refunds everything still refundable
}

// Refund returns money on a succeeded one-off payment. The payment row is
// locked while the refund is reserved, so concurrent refunds can never add
// up to more than was paid. The refund makes one call to the provider; when
// that call gets no answer, the refund comes back as unknown and the worker
// finishes the job. A repeat with the same idempotency key returns the
// stored refund with replayed=true.
func (s *Service) Refund(ctx context.Context, tenantID, paymentID, idempotencyKey string, req RefundRequest) (r Refund, replayed bool, err error) {
	if req.Amount < 0 {
		return Refund{}, false, &ValidationError{"amount_minor", "must not be negative"}
	}
	hash := hashRequest(struct {
		PaymentID string
		RefundRequest
	}{paymentID, req})

	err = s.store.Tx(ctx, func(tx Tx) error {
		p, err := tx.LockTenantPayment(ctx, tenantID, paymentID)
		if err != nil {
			return err
		}
		r, err = tx.RefundByIdempotencyKey(ctx, tenantID, idempotencyKey)
		switch {
		case err == nil:
			replayed = true
			if !bytes.Equal(r.RequestHash, hash) {
				return ErrIdempotencyMismatch
			}
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if r, err = s.reserveRefund(ctx, tx, p, idempotencyKey, hash, req.Amount); err != nil {
			return err
		}
		return tx.CreateRefund(ctx, r)
	})
	if err != nil || replayed {
		return r, replayed, err
	}

	p, r, c, err := s.claimRefund(ctx, r)
	if err != nil {
		return r, false, err
	}
	r, err = s.sendRefund(ctx, p, r, c)
	return r, false, err
}

func (s *Service) reserveRefund(ctx context.Context, tx Tx, p Payment, key string, hash []byte, amount int64) (Refund, error) {
	if p.Mode == gateway.ModeSubscription {
		return Refund{}, fmt.Errorf("%w: refunds of subscription payments", ErrUnsupported)
	}
	if !p.Status.Refundable() {
		return Refund{}, fmt.Errorf("%w: payment is %s", ErrInvalidState, p.Status)
	}
	reserved, err := tx.ReservedRefundAmount(ctx, p.ID)
	if err != nil {
		return Refund{}, err
	}
	remaining := p.Amount - reserved
	amount = cmp.Or(amount, remaining)
	if amount <= 0 || amount > remaining {
		return Refund{}, &ValidationError{"amount_minor", fmt.Sprintf("refundable amount is %d", remaining)}
	}
	return Refund{
		ID:             newID("re"),
		PaymentID:      p.ID,
		TenantID:       p.TenantID,
		IdempotencyKey: key,
		RequestHash:    hash,
		Amount:         amount,
		Status:         RefundUnknown,
		// The worker picks the refund up if this request dies before
		// recording the provider's answer.
		Followup: Followup{NextCheckAt: s.cfg.Now().Add(s.claimTTL())},
	}, nil
}

// needsSending reports whether a refund still waits for the provider to
// answer its submission.
func needsSending(r Refund) bool { return r.Status == RefundUnknown && !r.Flagged }

// sendRefund submits a claimed refund to the provider and records the
// answer. The provider idempotency key derives from the refund id, so
// resubmitting after an unanswered call can never refund twice.
func (s *Service) sendRefund(ctx context.Context, p Payment, r Refund, c claim) (Refund, error) {
	provider, ok := s.providers.Get(p.Provider)
	if !ok || !needsSending(r) {
		return r, s.release(ctx, c)
	}

	var res gateway.Refund
	callErr := s.call(ctx, func(ctx context.Context) (err error) {
		res, err = provider.Refund(ctx, gateway.RefundParams{
			IdempotencyKey: "refund-" + r.ID,
			RefundID:       r.ID,
			PaymentRef:     p.PaymentRef,
			Amount:         r.Amount,
			Currency:       p.Currency,
		})
		return err
	})

	err := s.settle(ctx, c, func(ctx context.Context, tx Tx) error {
		cp, cr, err := lockRefund(ctx, tx, r)
		if err != nil || !needsSending(cr) {
			return err
		}
		switch {
		case callErr == nil:
			r, _, err = s.applyRefund(ctx, tx, cp, cr, res, "")
		case gateway.IsRejected(callErr):
			r, _, err = s.applyRefund(ctx, tx, cp, cr, gateway.Refund{Status: gateway.RefundFailed}, callErr.Error())
		default:
			s.noAnswer(&cr.Followup, "refund", "refund", cr.ID, callErr)
			r, err = cr, tx.UpdateRefund(ctx, cr)
		}
		return err
	})
	if err != nil {
		return r, err
	}
	if gateway.IsRejected(callErr) {
		return r, callErr
	}
	return r, nil
}

// applyRefund moves a refund to the outcome its provider reported. A
// succeeded refund adds to the payment's refunded amount in the same
// transaction, so the two can never disagree.
func (s *Service) applyRefund(ctx context.Context, tx Tx, p Payment, r Refund, res gateway.Refund, reason string) (Refund, Result, error) {
	next, ok := refundStatuses[res.Status]
	if !ok {
		s.log.Warn("provider reported an unrecognised refund status; treating it as pending", "refund", r.ID, "status", res.Status)
		next = RefundPending
	}
	if !r.Status.Open() {
		if next != r.Status && next != RefundPending {
			s.log.Error("provider reports a different outcome for a settled refund; needs manual review",
				"refund", r.ID, "recorded", r.Status, "reported", next)
		}
		return r, ignored("refund is already %s", r.Status), nil
	}

	if r.ProviderRef == "" {
		r.ProviderRef = res.Ref
	}
	if next == RefundPending && r.Status == RefundPending {
		r.NextCheckAt = s.nextCheck(r.CreatedAt)
		return r, ignored("refund is still pending at the provider"), tx.UpdateRefund(ctx, r)
	}
	if err := r.moveTo(next); err != nil {
		return r, Result{}, err
	}
	switch next {
	case RefundPending:
		r.NextCheckAt = s.nextCheck(r.CreatedAt)
	case RefundSucceeded:
		p.AmountRefunded += r.Amount
		if err := p.moveTo(p.refundedStatus()); err != nil {
			return r, Result{}, err
		}
		if err := tx.UpdatePayment(ctx, p); err != nil {
			return r, Result{}, err
		}
	case RefundFailed:
		r.FailureReason = cmp.Or(reason, "provider reported the refund failed")
	}
	return r, applied("refund %s", next), tx.UpdateRefund(ctx, r)
}

package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

const reconcileBatch = 100

// RunWorker calls Reconcile every interval until ctx is done.
func (s *Service) RunWorker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("worker pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Reconcile finishes work that neither a webhook nor the original request
// did: checkouts and refunds whose provider call got no answer, captures
// still to make, and checkouts or refunds whose webhook never came. Every
// item is claimed before it is touched, so any number of workers can run
// at once.
func (s *Service) Reconcile(ctx context.Context) error {
	var payments []Payment
	var refunds []Refund
	err := s.store.Tx(ctx, func(tx Tx) (err error) {
		now := s.cfg.Now()
		if payments, err = tx.DuePayments(ctx, now, reconcileBatch); err != nil {
			return err
		}
		refunds, err = tx.DueRefunds(ctx, now, reconcileBatch)
		return err
	})
	if err != nil {
		return err
	}

	for _, p := range payments {
		if err := s.reconcilePayment(ctx, p.ID); err != nil && !errors.Is(err, ErrInProgress) {
			s.log.Error("reconciling payment", "payment", p.ID, "err", err)
		}
	}
	for _, r := range refunds {
		if err := s.reconcileRefund(ctx, r); err != nil && !errors.Is(err, ErrInProgress) {
			s.log.Error("reconciling refund", "refund", r.ID, "err", err)
		}
	}
	return nil
}

func (s *Service) reconcilePayment(ctx context.Context, id string) error {
	p, c, err := s.claimPayment(ctx, id)
	if err != nil {
		return err
	}
	provider, ok := s.providers.Get(p.Provider)
	capturer, canCapture := provider.(gateway.Capturer)
	switch {
	case !p.Status.Open() || p.Flagged:
		return s.release(ctx, c)
	case !ok:
		return s.postpone(ctx, c, fmt.Errorf("provider %q is not configured", p.Provider))
	case needsCheckout(p):
		_, err := s.openCheckout(ctx, provider, p, c)
		return err
	case p.Status == StatusAuthorized && canCapture:
		return s.captureClaimed(ctx, capturer, p, c)
	case s.cfg.Now().Sub(p.CreatedAt) > s.cfg.MaxOpenAge:
		return s.settle(ctx, c, func(ctx context.Context, tx Tx) error {
			cur, err := tx.LockPayment(ctx, id)
			if err != nil {
				return err
			}
			s.flag(&cur.Followup, "checkout still open after "+s.cfg.MaxOpenAge.String())
			s.log.Error("payment flagged for review", "payment", id, "reason", cur.FlagReason)
			return tx.UpdatePayment(ctx, cur)
		})
	}
	return s.refreshCheckout(ctx, provider, p, c)
}

// refreshCheckout asks the provider where a claimed checkout stands, for
// when its webhook is late or lost.
func (s *Service) refreshCheckout(ctx context.Context, provider gateway.Provider, p Payment, c claim) error {
	var st gateway.CheckoutState
	callErr := s.call(ctx, func(ctx context.Context) (err error) {
		st, err = provider.GetCheckout(ctx, p.CheckoutRef)
		return err
	})

	err := s.settle(ctx, c, func(ctx context.Context, tx Tx) error {
		cur, err := tx.LockPayment(ctx, p.ID)
		if err != nil {
			return err
		}
		if callErr == nil {
			if cur, _, err = s.applyCheckout(ctx, tx, cur, st, ""); err != nil {
				return err
			}
		}
		p = cur
		if !cur.Status.Open() {
			return nil
		}
		cur.NextCheckAt = s.nextCheck(cur.CreatedAt)
		return tx.UpdatePayment(ctx, cur)
	})
	if err != nil {
		return err
	}
	if callErr != nil {
		return fmt.Errorf("checking checkout %s: %w", p.CheckoutRef, callErr)
	}
	if p.Status == StatusAuthorized {
		return s.capture(ctx, provider, p.ID)
	}
	return nil
}

func (s *Service) reconcileRefund(ctx context.Context, r Refund) error {
	p, r, c, err := s.claimRefund(ctx, r)
	if err != nil {
		return err
	}
	provider, ok := s.providers.Get(p.Provider)
	age := s.cfg.Now().Sub(r.CreatedAt)
	switch {
	case !r.Status.Open() || r.Flagged:
		return s.release(ctx, c)
	case !ok:
		return s.postpone(ctx, c, fmt.Errorf("provider %q is not configured", p.Provider))
	case needsSending(r) && age > s.cfg.RefundRetryWindow:
		// Past this point the provider may have forgotten the idempotency
		// key, so a resubmission could refund twice.
		return s.flagRefund(ctx, c, r, "provider never confirmed the refund; check the provider dashboard before retrying")
	case needsSending(r):
		_, err := s.sendRefund(ctx, p, r, c)
		return err
	case age > s.cfg.MaxOpenAge:
		return s.flagRefund(ctx, c, r, "refund still pending at the provider after "+s.cfg.MaxOpenAge.String())
	}
	return s.refreshRefund(ctx, provider, p, r, c)
}

func (s *Service) flagRefund(ctx context.Context, c claim, r Refund, reason string) error {
	s.log.Error("refund flagged for review", "refund", r.ID, "payment", r.PaymentID, "reason", reason)
	return s.settle(ctx, c, func(ctx context.Context, tx Tx) error {
		_, cur, err := lockRefund(ctx, tx, r)
		if err != nil {
			return err
		}
		s.flag(&cur.Followup, reason)
		return tx.UpdateRefund(ctx, cur)
	})
}

func (s *Service) refreshRefund(ctx context.Context, provider gateway.Provider, p Payment, r Refund, c claim) error {
	var res gateway.Refund
	callErr := s.call(ctx, func(ctx context.Context) (err error) {
		res, err = provider.GetRefund(ctx, r.ProviderRef)
		return err
	})
	err := s.settle(ctx, c, func(ctx context.Context, tx Tx) error {
		cp, cr, err := lockRefund(ctx, tx, r)
		if err != nil {
			return err
		}
		if callErr == nil {
			_, _, err = s.applyRefund(ctx, tx, cp, cr, res, "")
			return err
		}
		cr.NextCheckAt = s.nextCheck(cr.CreatedAt)
		return tx.UpdateRefund(ctx, cr)
	})
	if err != nil {
		return err
	}
	if callErr != nil {
		return fmt.Errorf("checking refund %s: %w", r.ProviderRef, callErr)
	}
	return nil
}

// postpone moves a claimed item's next check back, so an item the worker
// cannot handle right now does not hold up the rest of the queue.
func (s *Service) postpone(ctx context.Context, c claim, cause error) error {
	next := s.cfg.Now().Add(s.cfg.MaxCheckInterval)
	err := s.settle(ctx, c, func(ctx context.Context, tx Tx) error {
		if c.resource == ResourceRefund {
			r, err := tx.LockRefund(ctx, c.id)
			if err != nil {
				return err
			}
			r.NextCheckAt = next
			return tx.UpdateRefund(ctx, r)
		}
		p, err := tx.LockPayment(ctx, c.id)
		if err != nil {
			return err
		}
		p.NextCheckAt = next
		return tx.UpdatePayment(ctx, p)
	})
	return errors.Join(cause, err)
}

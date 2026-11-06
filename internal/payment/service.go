package payment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

type Config struct {
	// CallTimeout bounds a single call to a provider.
	CallTimeout time.Duration
	// MaxAttempts is how many provider calls with no answer a checkout,
	// capture or refund gets before it is flagged for a person to look at.
	// Nothing is ever marked failed unless the provider says so.
	MaxAttempts int
	// RetryBackoff is the wait before the first retry. It doubles with each
	// attempt, up to MaxRetryBackoff.
	RetryBackoff    time.Duration
	MaxRetryBackoff time.Duration
	// FirstCheck is how long after a checkout or refund starts that the
	// worker first asks the provider about it, in case its webhook never
	// arrives.
	FirstCheck time.Duration
	// MaxCheckInterval caps the growing gap between those checks.
	MaxCheckInterval time.Duration
	// MaxOpenAge is how long a checkout or refund may stay open before the
	// worker stops checking it and flags it.
	MaxOpenAge time.Duration
	// RefundRetryWindow is how long an unanswered refund may be resubmitted.
	// It must stay inside the provider's idempotency-key retention (24 hours
	// at Stripe), so a resubmission can never refund twice.
	RefundRetryWindow time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func DefaultConfig() Config {
	return Config{
		CallTimeout:       10 * time.Second,
		MaxAttempts:       5,
		RetryBackoff:      30 * time.Second,
		MaxRetryBackoff:   15 * time.Minute,
		FirstCheck:        15 * time.Minute,
		MaxCheckInterval:  time.Hour,
		MaxOpenAge:        7 * 24 * time.Hour,
		RefundRetryWindow: 23 * time.Hour,
	}
}

type Service struct {
	store     Store
	providers *gateway.Registry
	cfg       Config
	log       *slog.Logger
}

func NewService(store Store, providers *gateway.Registry, cfg Config, log *slog.Logger) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{store: store, providers: providers, cfg: cfg, log: log}
}

// GetPayment returns a tenant's payment with its refunds.
func (s *Service) GetPayment(ctx context.Context, tenantID, id string) (Payment, error) {
	var p Payment
	err := s.store.Tx(ctx, func(tx Tx) (err error) {
		p, err = tx.GetPayment(ctx, tenantID, id)
		return err
	})
	return p, err
}

// call makes one provider call bounded by CallTimeout. It is detached from
// the caller's cancellation: once a call starts, its outcome must be
// recorded even if the API client hangs up.
func (s *Service) call(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.CallTimeout)
	defer cancel()
	return fn(ctx)
}

// retryLater records a provider call that got no answer: one more attempt,
// then a wait that doubles each time. When the attempts run out it flags
// the item instead and reports true.
func (s *Service) retryLater(f *Followup) (flagged bool) {
	f.Attempts++
	if f.Attempts >= s.cfg.MaxAttempts {
		s.flag(f, fmt.Sprintf("no answer from the provider after %d attempts", f.Attempts))
		return true
	}
	wait := s.cfg.RetryBackoff << (f.Attempts - 1)
	f.NextCheckAt = s.cfg.Now().Add(min(wait, s.cfg.MaxRetryBackoff))
	return false
}

// noAnswer records and logs a provider call that got no answer.
func (s *Service) noAnswer(f *Followup, op, kind, id string, err error) {
	if s.retryLater(f) {
		s.log.Error("provider never answered; flagged for review", kind, id, "op", op, "attempts", f.Attempts, "err", err)
		return
	}
	s.log.Warn("provider did not answer; will retry", kind, id, "op", op, "attempt", f.Attempts, "retry_at", f.NextCheckAt, "err", err)
}

func (s *Service) flag(f *Followup, reason string) {
	f.Flagged = true
	f.FlagReason = reason
	f.NextCheckAt = time.Time{}
}

// nextCheck spaces checks of an open checkout or refund out as it ages: a
// minute at first, then half its age, up to MaxCheckInterval.
func (s *Service) nextCheck(created time.Time) time.Time {
	now := s.cfg.Now()
	return now.Add(min(max(now.Sub(created)/2, time.Minute), s.cfg.MaxCheckInterval))
}

// claim is held while talking to a provider about one payment or refund.
type claim struct {
	resource Resource
	id       string
	token    string
}

// claimTTL outlasts a provider call, so a claim never lapses while its
// holder is still waiting on the provider.
func (s *Service) claimTTL() time.Duration { return s.cfg.CallTimeout + 30*time.Second }

func (s *Service) take(ctx context.Context, tx Tx, r Resource, id string) (claim, error) {
	token, ok, err := tx.Claim(ctx, r, id, s.claimTTL())
	if err != nil {
		return claim{}, err
	}
	if !ok {
		return claim{}, ErrInProgress
	}
	return claim{r, id, token}, nil
}

func (s *Service) claimPayment(ctx context.Context, id string) (Payment, claim, error) {
	var p Payment
	var c claim
	err := s.store.Tx(ctx, func(tx Tx) (err error) {
		if c, err = s.take(ctx, tx, ResourcePayment, id); err != nil {
			return err
		}
		p, err = tx.LockPayment(ctx, id)
		return err
	})
	return p, c, err
}

// claimRefund claims r and returns it fresh along with its payment. The
// payment is locked before the claim touches the refund row, keeping the
// lock order.
func (s *Service) claimRefund(ctx context.Context, r Refund) (Payment, Refund, claim, error) {
	var p Payment
	var c claim
	err := s.store.Tx(ctx, func(tx Tx) (err error) {
		if p, err = tx.LockPayment(ctx, r.PaymentID); err != nil {
			return err
		}
		if c, err = s.take(ctx, tx, ResourceRefund, r.ID); err != nil {
			return err
		}
		r, err = tx.LockRefund(ctx, r.ID)
		return err
	})
	return p, r, c, err
}

// settle records the outcome of a provider call and releases the claim in
// one transaction. Like call, it ignores the caller's cancellation; fn must
// use the context it is given.
func (s *Service) settle(ctx context.Context, c claim, fn func(context.Context, Tx) error) error {
	ctx = context.WithoutCancel(ctx)
	return s.store.Tx(ctx, func(tx Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return tx.Release(ctx, c.resource, c.id, c.token)
	})
}

func (s *Service) release(ctx context.Context, c claim) error {
	return s.settle(ctx, c, func(context.Context, Tx) error { return nil })
}

// lockRefund locks a refund's payment and then the refund itself, in the
// order every transaction uses.
func lockRefund(ctx context.Context, tx Tx, r Refund) (Payment, Refund, error) {
	p, err := tx.LockPayment(ctx, r.PaymentID)
	if err != nil {
		return Payment{}, Refund{}, err
	}
	r, err = tx.LockRefund(ctx, r.ID)
	return p, r, err
}

func newID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// hashRequest fingerprints a request so a reused idempotency key can be told
// apart from a genuine repeat.
func hashRequest(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("hashing request: %v", err)) // only plain structs are hashed
	}
	sum := sha256.Sum256(b)
	return sum[:]
}

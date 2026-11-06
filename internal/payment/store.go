package payment

import (
	"context"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

// Store persists payments, refunds and processed webhook events.
type Store interface {
	// Tx runs fn in one transaction, committing when fn returns nil.
	Tx(ctx context.Context, fn func(Tx) error) error
}

// Resource names what a claim covers.
type Resource string

const (
	ResourcePayment Resource = "payment"
	ResourceRefund  Resource = "refund"
)

// Tx is what the service can do inside a transaction. Lock* methods take a
// row lock held until the transaction ends. A payment is always locked
// before any of its refunds, which rules out deadlocks.
type Tx interface {
	// CreatePayment inserts p unless its tenant already used the idempotency
	// key, in which case it returns the existing payment and created=false.
	CreatePayment(ctx context.Context, p Payment) (stored Payment, created bool, err error)
	// GetPayment returns a tenant's payment with its refunds. A payment of
	// another tenant is reported as ErrNotFound, like a missing one.
	GetPayment(ctx context.Context, tenantID, id string) (Payment, error)
	LockPayment(ctx context.Context, id string) (Payment, error)
	// LockTenantPayment locks a payment only if it belongs to the tenant.
	LockTenantPayment(ctx context.Context, tenantID, id string) (Payment, error)
	// LockPaymentForEvent finds a provider's payment by gateway id, or by
	// checkout ref when the event carries no id.
	LockPaymentForEvent(ctx context.Context, provider, paymentID, checkoutRef string) (Payment, error)
	UpdatePayment(ctx context.Context, p Payment) error
	// DuePayments lists open, unflagged payments whose NextCheckAt has
	// passed and that nobody holds a claim on.
	DuePayments(ctx context.Context, now time.Time, limit int) ([]Payment, error)

	RefundByIdempotencyKey(ctx context.Context, tenantID, key string) (Refund, error)
	// CreateRefund inserts r. It returns ErrIdempotencyMismatch when the
	// tenant already used the key, which can only have been for another
	// payment because the caller holds this payment's lock.
	CreateRefund(ctx context.Context, r Refund) error
	// FindRefundForEvent finds a provider's refund by gateway id or provider
	// ref without locking it.
	FindRefundForEvent(ctx context.Context, provider, refundID, refundRef string) (Refund, error)
	LockRefund(ctx context.Context, id string) (Refund, error)
	UpdateRefund(ctx context.Context, r Refund) error
	// ReservedRefundAmount sums a payment's refunds that are not failed.
	ReservedRefundAmount(ctx context.Context, paymentID string) (int64, error)
	DueRefunds(ctx context.Context, now time.Time, limit int) ([]Refund, error)

	// Claim gives the caller sole use of a payment or refund for ttl, so only
	// one caller at a time talks to the provider about it. It returns a token
	// for Release, or ok=false when someone else holds an unexpired claim.
	Claim(ctx context.Context, r Resource, id string, ttl time.Duration) (token string, ok bool, err error)
	// Release ends a claim. It does nothing if the claim has since expired
	// and passed to someone else.
	Release(ctx context.Context, r Resource, id, token string) error

	// RecordEvent stores a webhook event id and reports whether it is new.
	RecordEvent(ctx context.Context, provider string, ev gateway.Event) (bool, error)
	SetEventOutcome(ctx context.Context, provider, eventID, outcome string) error
}

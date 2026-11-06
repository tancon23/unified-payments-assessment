// Package payment holds the business rules: the payment and refund state
// machines, idempotency, refund limits, webhook handling and the retry and
// reconciliation work. It reaches providers only through package gateway.
package payment

import (
	"fmt"
	"slices"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

// Status is where a payment stands.
type Status string

const (
	StatusPending           Status = "pending"    // checkout open, waiting for the customer
	StatusUnknown           Status = "unknown"    // the provider gave no answer while the checkout was opened
	StatusAuthorized        Status = "authorized" // customer approved; capture outstanding
	StatusSucceeded         Status = "succeeded"
	StatusFailed            Status = "failed" // only ever set when the provider says so
	StatusExpired           Status = "expired"
	StatusPartiallyRefunded Status = "partially_refunded"
	StatusRefunded          Status = "refunded"
)

// transitions lists every legal status change. Anything else is refused, so
// a late or out-of-order webhook can never move a payment backwards.
var transitions = map[Status][]Status{
	StatusPending:           {StatusUnknown, StatusAuthorized, StatusSucceeded, StatusFailed, StatusExpired},
	StatusUnknown:           {StatusPending, StatusAuthorized, StatusSucceeded, StatusFailed, StatusExpired},
	StatusAuthorized:        {StatusSucceeded, StatusFailed},
	StatusSucceeded:         {StatusPartiallyRefunded, StatusRefunded},
	StatusPartiallyRefunded: {StatusRefunded},
}

// CanBecome reports whether a payment in status s may move to next.
func (s Status) CanBecome(next Status) bool { return slices.Contains(transitions[s], next) }

// Open reports whether the payment still waits on the customer or provider.
func (s Status) Open() bool {
	return s == StatusPending || s == StatusUnknown || s == StatusAuthorized
}

// Refundable reports whether money can still be returned on the payment.
func (s Status) Refundable() bool { return s == StatusSucceeded || s == StatusPartiallyRefunded }

// checkoutStatuses maps a provider's checkout state to the payment status it
// implies. An open checkout implies no change.
var checkoutStatuses = map[gateway.CheckoutStatus]Status{
	gateway.CheckoutAuthorized: StatusAuthorized,
	gateway.CheckoutPaid:       StatusSucceeded,
	gateway.CheckoutFailed:     StatusFailed,
	gateway.CheckoutExpired:    StatusExpired,
}

// Followup is the retry and reconciliation bookkeeping shared by payments
// and refunds.
type Followup struct {
	// Attempts counts provider calls that got no answer for the current step.
	Attempts int
	// Flagged is set when the item ran out of attempts or stayed open too
	// long. Nothing retries it until a person has looked at it.
	Flagged    bool
	FlagReason string
	// NextCheckAt is when the worker next looks at the item. It is zero once
	// the item is final or flagged.
	NextCheckAt time.Time
}

type Payment struct {
	ID             string
	TenantID       string
	IdempotencyKey string
	RequestHash    []byte
	Provider       string
	Mode           gateway.Mode
	Amount         int64 // minor units
	Currency       string
	Interval       gateway.Interval
	Description    string
	SuccessURL     string
	CancelURL      string
	Status         Status
	AmountRefunded int64 // minor units
	CheckoutRef    string
	CheckoutURL    string
	PaymentRef     string
	FailureReason  string
	Followup
	CreatedAt time.Time
	UpdatedAt time.Time
	Refunds   []Refund // loaded by Tx.GetPayment only
}

// moveTo changes the payment's status. Every status change goes through
// here, and it refuses any move the state machine does not allow. A new
// status starts a new step, so the attempt count and flag reset.
func (p *Payment) moveTo(next Status) error {
	if p.Status == next {
		return nil
	}
	if !p.Status.CanBecome(next) {
		return fmt.Errorf("%w: payment %s from %s to %s", ErrInvalidTransition, p.ID, p.Status, next)
	}
	p.Status = next
	p.Followup = Followup{NextCheckAt: p.NextCheckAt}
	if !next.Open() {
		p.NextCheckAt = time.Time{}
	}
	return nil
}

func (p Payment) refundedStatus() Status {
	if p.AmountRefunded >= p.Amount {
		return StatusRefunded
	}
	return StatusPartiallyRefunded
}

// RefundStatus is where a refund stands.
type RefundStatus string

const (
	RefundUnknown   RefundStatus = "unknown" // sent, or about to be, with no answer yet
	RefundPending   RefundStatus = "pending" // the provider accepted it and is still processing
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed" // only ever set when the provider says so
)

var refundTransitions = map[RefundStatus][]RefundStatus{
	RefundUnknown: {RefundPending, RefundSucceeded, RefundFailed},
	RefundPending: {RefundSucceeded, RefundFailed},
}

// Open reports whether the refund still waits on the provider.
func (s RefundStatus) Open() bool { return s == RefundUnknown || s == RefundPending }

var refundStatuses = map[gateway.RefundStatus]RefundStatus{
	gateway.RefundPending:   RefundPending,
	gateway.RefundSucceeded: RefundSucceeded,
	gateway.RefundFailed:    RefundFailed,
}

type Refund struct {
	ID             string
	PaymentID      string
	TenantID       string
	IdempotencyKey string
	RequestHash    []byte
	Amount         int64 // minor units
	Status         RefundStatus
	ProviderRef    string
	FailureReason  string
	Followup
	CreatedAt time.Time
	UpdatedAt time.Time
}

// moveTo changes the refund's status, refusing moves the state machine does
// not allow.
func (r *Refund) moveTo(next RefundStatus) error {
	if r.Status == next {
		return nil
	}
	if !slices.Contains(refundTransitions[r.Status], next) {
		return fmt.Errorf("%w: refund %s from %s to %s", ErrInvalidTransition, r.ID, r.Status, next)
	}
	r.Status = next
	r.Followup = Followup{NextCheckAt: r.NextCheckAt}
	if !next.Open() {
		r.NextCheckAt = time.Time{}
	}
	return nil
}

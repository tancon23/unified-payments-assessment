// Package gateway is the provider-neutral contract between the payment
// service and payment providers. The service depends only on this package;
// each adapter translates it to and from one provider's API.
package gateway

import (
	"context"
	"net/http"
)

// Mode selects a one-off payment or a recurring subscription.
type Mode string

const (
	ModePayment      Mode = "payment"
	ModeSubscription Mode = "subscription"
)

// Interval is the billing period of a subscription.
type Interval string

const (
	IntervalDay   Interval = "day"
	IntervalWeek  Interval = "week"
	IntervalMonth Interval = "month"
	IntervalYear  Interval = "year"
)

// CheckoutParams describes a hosted checkout to open with a provider.
type CheckoutParams struct {
	// IdempotencyKey is forwarded to the provider so that a retried call
	// returns the original checkout instead of opening a second one.
	IdempotencyKey string
	// PaymentID is the gateway's own id. Providers store it on their object
	// and echo it back in webhooks.
	PaymentID   string
	Mode        Mode
	Amount      int64    // minor units
	Currency    string   // lower-case ISO 4217
	Interval    Interval // subscriptions only
	Description string
	SuccessURL  string
	CancelURL   string
}

// Checkout is an opened hosted checkout.
type Checkout struct {
	Ref string // provider id: Stripe Checkout Session, PayPal order or subscription
	URL string // page the customer is sent to
}

// CheckoutStatus is where a checkout stands at the provider.
type CheckoutStatus string

const (
	CheckoutOpen       CheckoutStatus = "open"       // waiting for the customer
	CheckoutAuthorized CheckoutStatus = "authorized" // approved; funds must still be captured
	CheckoutPaid       CheckoutStatus = "paid"
	CheckoutFailed     CheckoutStatus = "failed"
	CheckoutExpired    CheckoutStatus = "expired"
)

// CheckoutState is a checkout as the provider currently reports it.
type CheckoutState struct {
	Status CheckoutStatus
	// PaymentRef identifies the money movement once paid: a Stripe payment
	// intent, a PayPal capture, or the subscription id for subscriptions.
	PaymentRef string
	Amount     int64 // 0 when the provider did not report it
	Currency   string
}

// CaptureParams identifies an authorized checkout to capture.
type CaptureParams struct {
	IdempotencyKey string
	CheckoutRef    string
}

// RefundParams describes a refund of a paid checkout.
type RefundParams struct {
	IdempotencyKey string
	RefundID       string // gateway's id, echoed back in webhooks
	PaymentRef     string
	Amount         int64
	Currency       string
}

// RefundStatus is where a refund stands at the provider.
type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

// Refund is a refund as the provider reports it.
type Refund struct {
	Ref    string
	Status RefundStatus
}

// EventKind is what a webhook means to the gateway. Provider event types the
// gateway does not act on map to EventIgnored.
type EventKind string

const (
	EventIgnored            EventKind = ""
	EventCheckoutAuthorized EventKind = "checkout.authorized"
	EventCheckoutPaid       EventKind = "checkout.paid"
	EventCheckoutFailed     EventKind = "checkout.failed"
	EventCheckoutExpired    EventKind = "checkout.expired"
	EventRefundSucceeded    EventKind = "refund.succeeded"
	EventRefundFailed       EventKind = "refund.failed"
)

// Event is a verified webhook translated into provider-neutral terms.
type Event struct {
	ID   string // provider event id; redeliveries of one event share it
	Type string // provider's own event type, kept for audit and logs
	Kind EventKind

	PaymentID   string // gateway payment id echoed back, when present
	CheckoutRef string
	PaymentRef  string

	RefundID  string // gateway refund id echoed back, when present
	RefundRef string

	Amount   int64 // 0 when the event carries no amount
	Currency string
}

// Provider is implemented once per payment provider.
type Provider interface {
	CreateCheckout(ctx context.Context, p CheckoutParams) (Checkout, error)
	GetCheckout(ctx context.Context, checkoutRef string) (CheckoutState, error)
	Refund(ctx context.Context, p RefundParams) (Refund, error)
	GetRefund(ctx context.Context, refundRef string) (Refund, error)
	// VerifyWebhook authenticates a delivery and translates it. Anything that
	// cannot be proven to come from the provider yields ErrInvalidSignature.
	VerifyWebhook(ctx context.Context, header http.Header, body []byte) (Event, error)
}

// Capturer is implemented by providers whose approved checkouts must be
// captured explicitly, such as PayPal orders. Providers that capture on their
// own, such as Stripe Checkout, do not implement it.
type Capturer interface {
	Capture(ctx context.Context, p CaptureParams) (CheckoutState, error)
}

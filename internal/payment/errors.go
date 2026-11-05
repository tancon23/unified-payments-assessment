package payment

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrIdempotencyMismatch = errors.New("idempotency key was already used for a different request")
	ErrInProgress          = errors.New("another request for this resource is in progress")
	ErrInvalidState        = errors.New("operation not allowed in the current state")
	ErrInvalidTransition   = errors.New("status change not allowed")
	ErrUnsupported         = errors.New("operation not supported")
	ErrUnknownProvider     = errors.New("unknown provider")
	ErrInvalidWebhook      = errors.New("invalid webhook")
	// ErrProviderUnavailable means a provider needed to verify a webhook did
	// not answer. The provider redelivers the webhook later.
	ErrProviderUnavailable = errors.New("payment provider unavailable")
)

// ValidationError reports a request field the service cannot accept.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }

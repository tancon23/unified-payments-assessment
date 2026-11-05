package gateway

import (
	"errors"
	"fmt"
)

var (
	// ErrTransient marks failures where the provider was unreachable or
	// briefly unavailable: network errors, timeouts, 429 and 5xx. The outcome
	// is unknown, and the call is safe to repeat with the same idempotency key.
	ErrTransient = errors.New("provider temporarily unavailable")

	// ErrInvalidSignature marks a webhook that could not be authenticated.
	ErrInvalidSignature = errors.New("webhook signature invalid")
)

// RejectedError is a definitive refusal from a provider. Repeating the same
// request will not succeed.
type RejectedError struct {
	Provider   string
	StatusCode int
	Code       string
	Message    string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("%s rejected the request (HTTP %d %s): %s", e.Provider, e.StatusCode, e.Code, e.Message)
}

// IsRejected reports whether err is a definitive provider refusal.
func IsRejected(err error) bool {
	var rejected *RejectedError
	return errors.As(err, &rejected)
}

// Package paddle is a placeholder Paddle Billing adapter. It satisfies the
// gateway contract and passes the shared contract tests as a placeholder:
// every call is refused definitively and every webhook is rejected. It is
// not registered until it is implemented.
package paddle

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
)

// Name identifies Paddle in API requests, webhook URLs and errors.
const Name = "paddle"

type Provider struct{}

var _ gateway.Provider = Provider{}

func New() Provider { return Provider{} }

func notImplemented() error {
	return &gateway.RejectedError{Provider: Name, StatusCode: http.StatusBadRequest, Code: "not_implemented", Message: "the Paddle adapter is not implemented yet"}
}

func (Provider) CreateCheckout(context.Context, gateway.CheckoutParams) (gateway.Checkout, error) {
	return gateway.Checkout{}, notImplemented()
}

func (Provider) GetCheckout(context.Context, string) (gateway.CheckoutState, error) {
	return gateway.CheckoutState{}, notImplemented()
}

func (Provider) Refund(context.Context, gateway.RefundParams) (gateway.Refund, error) {
	return gateway.Refund{}, notImplemented()
}

func (Provider) GetRefund(context.Context, string) (gateway.Refund, error) {
	return gateway.Refund{}, notImplemented()
}

// VerifyWebhook refuses every delivery, so nothing unverified is accepted.
func (Provider) VerifyWebhook(context.Context, http.Header, []byte) (gateway.Event, error) {
	return gateway.Event{}, fmt.Errorf("%w: Paddle webhooks are not implemented yet", gateway.ErrInvalidSignature)
}

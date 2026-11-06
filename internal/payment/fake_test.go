package payment_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
)

// fault is an error a fakeProvider returns once. With afterActing set, the
// provider acts first and then fails, like a timeout after the request
// reached the provider.
type fault struct {
	err         error
	afterActing bool
}

var (
	unavailable  = fault{err: fmt.Errorf("%w: HTTP 503", gateway.ErrTransient)}
	timedOut     = fault{err: fmt.Errorf("%w: context deadline exceeded", gateway.ErrTransient), afterActing: true}
	providerSaid = func(code string) fault {
		return fault{err: &gateway.RejectedError{Provider: "fake", StatusCode: 402, Code: code, Message: code}}
	}
)

// fakeProvider is a scriptable provider. Like real ones, it returns the same
// object for a repeated idempotency key.
type fakeProvider struct {
	mu        sync.Mutex
	faults    map[string][]fault
	calls     map[string]int
	checkouts map[string]gateway.Checkout      // idempotency key -> checkout
	states    map[string]gateway.CheckoutState // checkout ref -> state
	refunds   map[string]gateway.Refund        // idempotency key -> refund
	refundsBy map[string]gateway.Refund        // refund ref -> refund
	// refundOutcome is what new refunds report; succeeded when empty.
	refundOutcome gateway.RefundStatus
	// hook, when set, runs at the start of every call.
	hook func(op string)
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		faults:    map[string][]fault{},
		calls:     map[string]int{},
		checkouts: map[string]gateway.Checkout{},
		states:    map[string]gateway.CheckoutState{},
		refunds:   map[string]gateway.Refund{},
		refundsBy: map[string]gateway.Refund{},
	}
}

func (f *fakeProvider) inject(op string, faults ...fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[op] = append(f.faults[op], faults...)
}

// begin counts a call and pops its next fault. The caller holds f.mu.
func (f *fakeProvider) begin(op string) (fault, bool) {
	f.calls[op]++
	if f.hook != nil {
		f.hook(op)
	}
	queue := f.faults[op]
	if len(queue) == 0 {
		return fault{}, false
	}
	f.faults[op] = queue[1:]
	return queue[0], true
}

func (f *fakeProvider) callCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func (f *fakeProvider) distinctCheckouts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.checkouts)
}

func (f *fakeProvider) distinctRefunds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refunds)
}

func (f *fakeProvider) setHook(hook func(op string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = hook
}

func (f *fakeProvider) setRefundOutcome(status gateway.RefundStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refundOutcome = status
}

func (f *fakeProvider) setState(ref string, st gateway.CheckoutState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[ref] = st
}

func (f *fakeProvider) CreateCheckout(_ context.Context, p gateway.CheckoutParams) (gateway.Checkout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flt, faulty := f.begin("create")
	if faulty && !flt.afterActing {
		return gateway.Checkout{}, flt.err
	}
	co, ok := f.checkouts[p.IdempotencyKey]
	if !ok {
		ref := fmt.Sprintf("chk_%d", len(f.checkouts)+1)
		co = gateway.Checkout{Ref: ref, URL: "https://pay.test/" + ref}
		f.checkouts[p.IdempotencyKey] = co
		f.states[ref] = gateway.CheckoutState{Status: gateway.CheckoutOpen, Amount: p.Amount, Currency: p.Currency}
	}
	if faulty {
		return gateway.Checkout{}, flt.err
	}
	return co, nil
}

func (f *fakeProvider) GetCheckout(_ context.Context, ref string) (gateway.CheckoutState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt, faulty := f.begin("get"); faulty {
		return gateway.CheckoutState{}, flt.err
	}
	return f.states[ref], nil
}

func (f *fakeProvider) Refund(_ context.Context, p gateway.RefundParams) (gateway.Refund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flt, faulty := f.begin("refund")
	if faulty && !flt.afterActing {
		return gateway.Refund{}, flt.err
	}
	r, ok := f.refunds[p.IdempotencyKey]
	if !ok {
		status := f.refundOutcome
		if status == "" {
			status = gateway.RefundSucceeded
		}
		r = gateway.Refund{Ref: fmt.Sprintf("rf_%d", len(f.refunds)+1), Status: status}
		f.refunds[p.IdempotencyKey] = r
		f.refundsBy[r.Ref] = r
	}
	if faulty {
		return gateway.Refund{}, flt.err
	}
	return r, nil
}

func (f *fakeProvider) GetRefund(_ context.Context, ref string) (gateway.Refund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt, faulty := f.begin("get_refund"); faulty {
		return gateway.Refund{}, flt.err
	}
	return f.refundsBy[ref], nil
}

func (f *fakeProvider) settleRefund(ref string, status gateway.RefundStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.refundsBy[ref]
	r.Status = status
	f.refundsBy[ref] = r
}

const validSignature = "valid"

// VerifyWebhook accepts a gateway.Event as JSON when the test signature
// header is present.
func (f *fakeProvider) VerifyWebhook(_ context.Context, h http.Header, body []byte) (gateway.Event, error) {
	if h.Get("X-Test-Signature") != validSignature {
		return gateway.Event{}, gateway.ErrInvalidSignature
	}
	var ev gateway.Event
	err := json.Unmarshal(body, &ev)
	return ev, err
}

// capturingProvider needs an explicit capture after approval, like PayPal.
type capturingProvider struct {
	*fakeProvider
}

func (c capturingProvider) Capture(_ context.Context, p gateway.CaptureParams) (gateway.CheckoutState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	flt, faulty := c.begin("capture")
	if faulty && !flt.afterActing {
		return gateway.CheckoutState{}, flt.err
	}
	st := c.states[p.CheckoutRef]
	if st.Status != gateway.CheckoutPaid {
		st.Status, st.PaymentRef = gateway.CheckoutPaid, "cap_"+p.CheckoutRef
		c.states[p.CheckoutRef] = st
	}
	if faulty {
		return gateway.CheckoutState{}, flt.err
	}
	return st, nil
}

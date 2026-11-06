package payment_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
	"github.com/tancon23/unified-payments-assessment/internal/payment"
	"github.com/tancon23/unified-payments-assessment/internal/postgres"
	"github.com/tancon23/unified-payments-assessment/internal/postgres/pgtest"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t         *testing.T
	ctx       context.Context
	store     *postgres.Store
	svc       *payment.Service
	cfg       payment.Config
	fake      *fakeProvider
	capturing capturingProvider
	clock     *clock
}

const (
	acme   = "acme"
	globex = "globex"
)

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	store := pgtest.New(t)
	for _, id := range []string{acme, globex} {
		if err := store.UpsertTenant(ctx, id, id, id+"-key"); err != nil {
			t.Fatal(err)
		}
	}

	e := &env{t: t, ctx: ctx, store: store, fake: newFakeProvider(), capturing: capturingProvider{newFakeProvider()}, clock: &clock{now: time.Now()}}
	providers := gateway.NewRegistry()
	providers.Register("fake", e.fake)
	providers.Register("capturing", e.capturing)

	e.cfg = payment.DefaultConfig()
	e.cfg.CallTimeout = time.Second
	e.cfg.Now = e.clock.Now
	e.svc = payment.NewService(store, providers, e.cfg, slog.New(slog.DiscardHandler))
	return e
}

func checkoutRequest(provider string) payment.CheckoutRequest {
	return payment.CheckoutRequest{
		Provider:   provider,
		Mode:       gateway.ModePayment,
		Amount:     2500,
		Currency:   "usd",
		SuccessURL: "https://shop.test/ok",
		CancelURL:  "https://shop.test/cancel",
	}
}

func (e *env) checkout(tenant, key, provider string) payment.Payment {
	e.t.Helper()
	p, _, err := e.svc.CreateCheckout(e.ctx, tenant, key, checkoutRequest(provider))
	if err != nil {
		e.t.Fatalf("checkout: %v", err)
	}
	return p
}

func (e *env) deliver(provider string, ev gateway.Event) payment.WebhookResult {
	e.t.Helper()
	res, err := e.deliverErr(provider, ev, validSignature)
	if err != nil {
		e.t.Fatalf("webhook: %v", err)
	}
	return res
}

func (e *env) deliverErr(provider string, ev gateway.Event, signature string) (payment.WebhookResult, error) {
	body, _ := json.Marshal(ev)
	return e.svc.HandleWebhook(e.ctx, provider, http.Header{"X-Test-Signature": {signature}}, body)
}

func paidEvent(id string, p payment.Payment) gateway.Event {
	return gateway.Event{
		ID: id, Type: "test.paid", Kind: gateway.EventCheckoutPaid,
		PaymentID: p.ID, CheckoutRef: p.CheckoutRef, PaymentRef: "pi_" + p.ID,
		Amount: p.Amount, Currency: p.Currency,
	}
}

func (e *env) paid(tenant, key string) payment.Payment {
	e.t.Helper()
	p := e.checkout(tenant, key, "fake")
	e.deliver("fake", paidEvent("evt_paid_"+p.ID, p))
	return e.get(tenant, p.ID)
}

func (e *env) get(tenant, id string) payment.Payment {
	e.t.Helper()
	p, err := e.svc.GetPayment(e.ctx, tenant, id)
	if err != nil {
		e.t.Fatalf("get payment: %v", err)
	}
	return p
}

// work moves the clock on by d and runs one worker pass.
func (e *env) work(d time.Duration) {
	e.t.Helper()
	e.clock.Advance(d)
	if err := e.svc.Reconcile(e.ctx); err != nil {
		e.t.Fatalf("worker pass: %v", err)
	}
}

func wantStatus(t *testing.T, p payment.Payment, want payment.Status) {
	t.Helper()
	if p.Status != want {
		t.Fatalf("payment %s is %s, want %s (failure %q, flag %q)", p.ID, p.Status, want, p.FailureReason, p.FlagReason)
	}
}

// --- Idempotent checkout ---------------------------------------------------

func TestCheckoutReplaysForSameKey(t *testing.T) {
	e := newEnv(t)
	first := e.checkout(acme, "order-1", "fake")
	again, replayed, err := e.svc.CreateCheckout(e.ctx, acme, "order-1", checkoutRequest("fake"))
	if err != nil || !replayed || again.ID != first.ID || again.CheckoutURL != first.CheckoutURL {
		t.Fatalf("replay = %+v, replayed=%v, err=%v; want payment %s", again, replayed, err, first.ID)
	}
	if n := e.fake.distinctCheckouts(); n != 1 {
		t.Fatalf("provider opened %d checkouts, want 1", n)
	}
}

func TestCheckoutRefusesKeyReusedForDifferentRequest(t *testing.T) {
	e := newEnv(t)
	e.checkout(acme, "order-1", "fake")
	req := checkoutRequest("fake")
	req.Amount = 9900
	if _, _, err := e.svc.CreateCheckout(e.ctx, acme, "order-1", req); !errors.Is(err, payment.ErrIdempotencyMismatch) {
		t.Fatalf("want ErrIdempotencyMismatch, got %v", err)
	}
}

func TestIdempotencyKeysAreScopedPerTenant(t *testing.T) {
	e := newEnv(t)
	a := e.checkout(acme, "order-1", "fake")
	g := e.checkout(globex, "order-1", "fake")
	if a.ID == g.ID || g.TenantID != globex {
		t.Fatalf("globex received acme's payment: %+v", g)
	}
}

func TestConcurrentCheckoutsWithSameKeyOpenOneCheckout(t *testing.T) {
	e := newEnv(t)
	const n = 20
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			p, _, err := e.svc.CreateCheckout(e.ctx, acme, "order-1", checkoutRequest("fake"))
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			ids <- p.ID
		})
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent requests saw %d payments, want 1", len(seen))
	}
	if c := e.fake.distinctCheckouts(); c != 1 {
		t.Fatalf("provider opened %d checkouts, want 1", c)
	}
}

// --- Provider gives no answer ----------------------------------------------

func TestUnansweredCheckoutIsUnknownUntilTheWorkerFinishesIt(t *testing.T) {
	e := newEnv(t)
	e.fake.inject("create", unavailable)
	p, replayed, err := e.svc.CreateCheckout(e.ctx, acme, "order-1", checkoutRequest("fake"))
	if err != nil || replayed {
		t.Fatalf("checkout: replayed=%v, %v", replayed, err)
	}
	wantStatus(t, p, payment.StatusUnknown)
	if p.Attempts != 1 || p.CheckoutURL != "" {
		t.Fatalf("payment = %+v, want 1 attempt and no checkout yet", p)
	}

	// A replay reports the same state; it does not call the provider again.
	again, replayed, err := e.svc.CreateCheckout(e.ctx, acme, "order-1", checkoutRequest("fake"))
	if err != nil || !replayed || again.Status != payment.StatusUnknown || e.fake.callCount("create") != 1 {
		t.Fatalf("replay = %s, replayed=%v, %v after %d calls", again.Status, replayed, err, e.fake.callCount("create"))
	}

	e.work(e.cfg.RetryBackoff)
	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusPending)
	if got.CheckoutURL == "" || got.Attempts != 0 {
		t.Fatalf("after the worker: %+v", got)
	}
}

func TestTimedOutCheckoutNeverOpensTwo(t *testing.T) {
	e := newEnv(t)
	// The provider acts, but its answer is lost.
	e.fake.inject("create", timedOut)
	p := e.checkout(acme, "order-1", "fake")
	wantStatus(t, p, payment.StatusUnknown)

	e.work(e.cfg.RetryBackoff)
	wantStatus(t, e.get(acme, p.ID), payment.StatusPending)
	if n := e.fake.distinctCheckouts(); n != 1 {
		t.Fatalf("provider opened %d checkouts, want 1", n)
	}
}

func TestCheckoutWithNoAnswerIsFlaggedNeverFailed(t *testing.T) {
	e := newEnv(t)
	e.fake.inject("create", unavailable, unavailable, unavailable, unavailable, unavailable)
	p := e.checkout(acme, "order-1", "fake")
	for range e.cfg.MaxAttempts {
		e.work(e.cfg.MaxRetryBackoff)
	}
	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusUnknown)
	if !got.Flagged || got.Attempts != e.cfg.MaxAttempts || got.FlagReason == "" {
		t.Fatalf("payment = %+v, want flagged after %d attempts", got.Followup, e.cfg.MaxAttempts)
	}
	if calls := e.fake.callCount("create"); calls != e.cfg.MaxAttempts {
		t.Fatalf("provider called %d times, want %d", calls, e.cfg.MaxAttempts)
	}
}

func TestCheckoutRejectedByProviderFails(t *testing.T) {
	e := newEnv(t)
	e.fake.inject("create", providerSaid("currency_unsupported"))
	p, _, err := e.svc.CreateCheckout(e.ctx, acme, "order-1", checkoutRequest("fake"))
	if !gateway.IsRejected(err) {
		t.Fatalf("want RejectedError, got %v", err)
	}
	wantStatus(t, p, payment.StatusFailed)
}

func TestAnswerIsRecordedWhenClientHangsUp(t *testing.T) {
	e := newEnv(t)
	ctx, hangUp := context.WithCancel(e.ctx)
	e.fake.setHook(func(string) { hangUp() })

	if _, _, err := e.svc.CreateCheckout(ctx, acme, "order-1", checkoutRequest("fake")); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	p, _, _ := e.svc.CreateCheckout(e.ctx, acme, "order-1", checkoutRequest("fake"))
	wantStatus(t, p, payment.StatusPending)
	if p.CheckoutURL == "" {
		t.Fatal("the checkout the provider opened was not recorded")
	}
}

// --- Worker ----------------------------------------------------------------

func TestLostWebhookIsRecoveredByTheWorker(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")
	e.fake.setState(p.CheckoutRef, gateway.CheckoutState{Status: gateway.CheckoutPaid, PaymentRef: "pi_1", Amount: 2500, Currency: "usd"})

	e.work(e.cfg.FirstCheck / 2)
	wantStatus(t, e.get(acme, p.ID), payment.StatusPending)

	e.work(e.cfg.FirstCheck)
	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusSucceeded)
	if got.PaymentRef != "pi_1" {
		t.Fatalf("payment ref = %q, want pi_1", got.PaymentRef)
	}
}

func TestAbandonedCheckoutExpires(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")
	e.work(e.cfg.FirstCheck)
	wantStatus(t, e.get(acme, p.ID), payment.StatusPending)

	e.fake.setState(p.CheckoutRef, gateway.CheckoutState{Status: gateway.CheckoutExpired})
	e.work(e.cfg.MaxCheckInterval)
	wantStatus(t, e.get(acme, p.ID), payment.StatusExpired)
}

func TestCheckoutOpenTooLongIsFlagged(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")
	e.work(e.cfg.MaxOpenAge + time.Hour)
	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusPending)
	if !got.Flagged {
		t.Fatalf("payment = %+v, want flagged", got.Followup)
	}
}

func TestWorkerPostponesItemsItCannotHandle(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")

	// A worker that no longer has the provider configured.
	others := gateway.NewRegistry()
	others.Register("capturing", e.capturing)
	worker := payment.NewService(e.store, others, e.cfg, slog.New(slog.DiscardHandler))
	e.clock.Advance(e.cfg.FirstCheck)
	if err := worker.Reconcile(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(acme, p.ID); !got.NextCheckAt.After(e.clock.Now()) {
		t.Fatalf("next check %v is not after now %v; the item would block the queue", got.NextCheckAt, e.clock.Now())
	}
}

// --- Tenant isolation ------------------------------------------------------

func TestTenantCannotReadOrRefundAnotherTenantsPayment(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	if _, err := e.svc.GetPayment(e.ctx, globex, p.ID); !errors.Is(err, payment.ErrNotFound) {
		t.Errorf("read: want ErrNotFound, got %v", err)
	}
	if _, _, err := e.svc.Refund(e.ctx, globex, p.ID, "refund-1", payment.RefundRequest{}); !errors.Is(err, payment.ErrNotFound) {
		t.Errorf("refund: want ErrNotFound, got %v", err)
	}
	wantStatus(t, e.get(acme, p.ID), payment.StatusSucceeded)
}

// --- Webhooks --------------------------------------------------------------

func TestDuplicateWebhookChangesStateOnce(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")
	ev := paidEvent("evt_1", p)

	if res := e.deliver("fake", ev); res.Outcome != payment.OutcomeApplied {
		t.Fatalf("first delivery: %v", res)
	}
	if res := e.deliver("fake", ev); res.Outcome != payment.OutcomeDuplicate {
		t.Fatalf("second delivery: %v", res)
	}
	wantStatus(t, e.get(acme, p.ID), payment.StatusSucceeded)
}

func TestConcurrentDuplicateWebhooksApplyOnce(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")
	ev := paidEvent("evt_1", p)

	const n = 10
	outcomes := make(chan payment.Outcome, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			res, err := e.deliverErr("fake", ev, validSignature)
			if err != nil {
				t.Errorf("delivery: %v", err)
			}
			outcomes <- res.Outcome
		})
	}
	wg.Wait()
	close(outcomes)
	count := map[payment.Outcome]int{}
	for o := range outcomes {
		count[o]++
	}
	if count[payment.OutcomeApplied] != 1 || count[payment.OutcomeDuplicate] != n-1 {
		t.Fatalf("outcomes = %v, want 1 applied and %d duplicates", count, n-1)
	}
}

func TestLateEventsCannotMovePaymentBackwards(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	for i, kind := range []gateway.EventKind{gateway.EventCheckoutFailed, gateway.EventCheckoutExpired, gateway.EventCheckoutAuthorized} {
		ev := gateway.Event{ID: fmt.Sprintf("evt_late_%d", i), Kind: kind, PaymentID: p.ID}
		if res := e.deliver("fake", ev); res.Outcome != payment.OutcomeIgnored {
			t.Errorf("late %s: outcome %v, want ignored", kind, res)
		}
	}
	wantStatus(t, e.get(acme, p.ID), payment.StatusSucceeded)
}

func TestWebhookWithWrongAmountIsRejected(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")
	ev := paidEvent("evt_1", p)
	ev.Amount = 1
	if res := e.deliver("fake", ev); res.Outcome != payment.OutcomeRejected {
		t.Fatalf("outcome %v, want rejected", res)
	}
	wantStatus(t, e.get(acme, p.ID), payment.StatusPending)
}

func TestWebhookFromAnotherProviderCannotTouchPayment(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")
	if res := e.deliver("capturing", paidEvent("evt_1", p)); res.Outcome != payment.OutcomeIgnored {
		t.Fatalf("outcome %v, want ignored", res)
	}
	wantStatus(t, e.get(acme, p.ID), payment.StatusPending)
}

func TestInvalidSignatureIsRejected(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")
	if _, err := e.deliverErr("fake", paidEvent("evt_1", p), "forged"); !errors.Is(err, payment.ErrInvalidWebhook) {
		t.Fatalf("want ErrInvalidWebhook, got %v", err)
	}
	if _, err := e.deliverErr("nope", paidEvent("evt_1", p), validSignature); !errors.Is(err, payment.ErrUnknownProvider) {
		t.Fatalf("want ErrUnknownProvider, got %v", err)
	}
	wantStatus(t, e.get(acme, p.ID), payment.StatusPending)
}

// --- Capture ---------------------------------------------------------------

func TestApprovedCheckoutIsCaptured(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "capturing")
	e.deliver("capturing", gateway.Event{ID: "evt_1", Kind: gateway.EventCheckoutAuthorized, PaymentID: p.ID, Amount: 2500, Currency: "usd"})

	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusSucceeded)
	if got.PaymentRef != "cap_"+p.CheckoutRef {
		t.Fatalf("payment ref = %q", got.PaymentRef)
	}
}

func TestUnansweredCaptureIsRetriedByTheWorker(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "capturing")
	e.capturing.inject("capture", timedOut)
	e.deliver("capturing", gateway.Event{ID: "evt_1", Kind: gateway.EventCheckoutAuthorized, PaymentID: p.ID})
	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusAuthorized)
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.Attempts)
	}

	e.work(e.cfg.RetryBackoff)
	wantStatus(t, e.get(acme, p.ID), payment.StatusSucceeded)
}

func TestDeclinedCaptureFailsPayment(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "capturing")
	e.capturing.inject("capture", providerSaid("INSTRUMENT_DECLINED"))
	e.deliver("capturing", gateway.Event{ID: "evt_1", Kind: gateway.EventCheckoutAuthorized, PaymentID: p.ID})
	wantStatus(t, e.get(acme, p.ID), payment.StatusFailed)
}

// --- Refunds ---------------------------------------------------------------

func (e *env) refund(tenant, paymentID, key string, amount int64) (payment.Refund, error) {
	r, _, err := e.svc.Refund(e.ctx, tenant, paymentID, key, payment.RefundRequest{Amount: amount})
	return r, err
}

func TestFullRefund(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	r, err := e.refund(acme, p.ID, "refund-1", 0)
	if err != nil || r.Status != payment.RefundSucceeded || r.Amount != 2500 {
		t.Fatalf("refund = %+v, %v", r, err)
	}
	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusRefunded)
	if got.AmountRefunded != 2500 || len(got.Refunds) != 1 {
		t.Fatalf("payment = %+v", got)
	}
	if _, err := e.refund(acme, p.ID, "refund-2", 0); !errors.Is(err, payment.ErrInvalidState) {
		t.Fatalf("refund of refunded payment: want ErrInvalidState, got %v", err)
	}
}

func TestPartialRefundsCannotExceedPayment(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	if _, err := e.refund(acme, p.ID, "refund-1", 1000); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.get(acme, p.ID), payment.StatusPartiallyRefunded)

	var validation *payment.ValidationError
	if _, err := e.refund(acme, p.ID, "refund-2", 1501); !errors.As(err, &validation) {
		t.Fatalf("over-refund: want ValidationError, got %v", err)
	}
	if _, err := e.refund(acme, p.ID, "refund-3", 1500); err != nil {
		t.Fatal(err)
	}
	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusRefunded)
	if got.AmountRefunded != 2500 {
		t.Fatalf("amount refunded = %d, want 2500", got.AmountRefunded)
	}
}

func TestConcurrentRefundsNeverExceedPayment(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	const n = 10
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { e.refund(acme, p.ID, fmt.Sprintf("refund-%d", i), 1000) })
	}
	wg.Wait()

	got := e.get(acme, p.ID)
	if got.AmountRefunded != 2000 || len(got.Refunds) != 2 || e.fake.distinctRefunds() != 2 {
		t.Fatalf("amount refunded %d over %d refunds (%d at provider); want 2000 over 2",
			got.AmountRefunded, len(got.Refunds), e.fake.distinctRefunds())
	}
}

func TestRefundReplayAndKeyReuse(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	first, err := e.refund(acme, p.ID, "refund-1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	again, replayed, err := e.svc.Refund(e.ctx, acme, p.ID, "refund-1", payment.RefundRequest{Amount: 1000})
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay = %+v, replayed=%v, %v", again, replayed, err)
	}
	if _, err := e.refund(acme, p.ID, "refund-1", 500); !errors.Is(err, payment.ErrIdempotencyMismatch) {
		t.Fatalf("same key, different amount: want ErrIdempotencyMismatch, got %v", err)
	}
	other := e.paid(acme, "order-2")
	if _, err := e.refund(acme, other.ID, "refund-1", 1000); !errors.Is(err, payment.ErrIdempotencyMismatch) {
		t.Fatalf("same key, another payment: want ErrIdempotencyMismatch, got %v", err)
	}
	if got := e.get(acme, p.ID); got.AmountRefunded != 1000 {
		t.Fatalf("amount refunded = %d, want 1000", got.AmountRefunded)
	}
}

func TestRefundOfUnpaidPaymentIsRefused(t *testing.T) {
	e := newEnv(t)
	p := e.checkout(acme, "order-1", "fake")
	if _, err := e.refund(acme, p.ID, "refund-1", 0); !errors.Is(err, payment.ErrInvalidState) {
		t.Fatalf("want ErrInvalidState, got %v", err)
	}
}

func TestSubscriptionRefundIsUnsupported(t *testing.T) {
	e := newEnv(t)
	req := checkoutRequest("fake")
	req.Mode, req.Interval = gateway.ModeSubscription, gateway.IntervalMonth
	p, _, err := e.svc.CreateCheckout(e.ctx, acme, "sub-1", req)
	if err != nil {
		t.Fatal(err)
	}
	e.deliver("fake", paidEvent("evt_1", p))
	if _, err := e.refund(acme, p.ID, "refund-1", 0); !errors.Is(err, payment.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestUnansweredRefundIsUnknownUntilTheWorkerFinishesIt(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	e.fake.inject("refund", timedOut)

	r, err := e.refund(acme, p.ID, "refund-1", 0)
	if err != nil || r.Status != payment.RefundUnknown || r.Attempts != 1 {
		t.Fatalf("refund = %+v, %v; want unknown after 1 attempt", r, err)
	}
	// The amount stays reserved while the outcome is unknown.
	var validation *payment.ValidationError
	if _, err := e.refund(acme, p.ID, "refund-2", 1); !errors.As(err, &validation) {
		t.Fatalf("second refund while the first is unknown: want ValidationError, got %v", err)
	}

	e.work(e.cfg.RetryBackoff)
	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusRefunded)
	if got.Refunds[0].Status != payment.RefundSucceeded || e.fake.distinctRefunds() != 1 {
		t.Fatalf("refund %+v with %d refunds at provider; want succeeded and 1", got.Refunds[0], e.fake.distinctRefunds())
	}
}

func TestRefundWithNoAnswerIsFlaggedNeverFailed(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	e.fake.inject("refund", unavailable, unavailable, unavailable, unavailable, unavailable)
	if _, err := e.refund(acme, p.ID, "refund-1", 0); err != nil {
		t.Fatal(err)
	}
	for range e.cfg.MaxAttempts {
		e.work(e.cfg.MaxRetryBackoff)
	}
	r := e.get(acme, p.ID).Refunds[0]
	if r.Status != payment.RefundUnknown || !r.Flagged || r.Attempts != e.cfg.MaxAttempts {
		t.Fatalf("refund = %+v, want unknown and flagged after %d attempts", r, e.cfg.MaxAttempts)
	}
}

func TestRefundPastTheRetryWindowIsFlaggedNotResent(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	e.fake.inject("refund", unavailable)
	if _, err := e.refund(acme, p.ID, "refund-1", 0); err != nil {
		t.Fatal(err)
	}
	e.work(e.cfg.RefundRetryWindow + time.Minute)
	r := e.get(acme, p.ID).Refunds[0]
	if !r.Flagged || r.Status != payment.RefundUnknown {
		t.Fatalf("refund = %+v, want unknown and flagged", r)
	}
	if calls := e.fake.callCount("refund"); calls != 1 {
		t.Fatalf("refund resubmitted after the retry window (%d calls)", calls)
	}
}

func TestPendingRefundSettlesThroughTheWorker(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	e.fake.setRefundOutcome(gateway.RefundPending)
	r, err := e.refund(acme, p.ID, "refund-1", 0)
	if err != nil || r.Status != payment.RefundPending || r.ProviderRef == "" {
		t.Fatalf("refund = %+v, %v", r, err)
	}

	e.fake.settleRefund(r.ProviderRef, gateway.RefundFailed)
	e.work(e.cfg.MaxCheckInterval)
	got := e.get(acme, p.ID)
	if got.Refunds[0].Status != payment.RefundFailed || got.AmountRefunded != 0 {
		t.Fatalf("payment %+v; want the refund failed and nothing refunded", got)
	}
	wantStatus(t, got, payment.StatusSucceeded)

	// The failed refund no longer reserves the amount.
	e.fake.setRefundOutcome(gateway.RefundSucceeded)
	if _, err := e.refund(acme, p.ID, "refund-2", 0); err != nil {
		t.Fatalf("second refund: %v", err)
	}
}

func TestUnrecognisedRefundStatusIsTreatedAsPending(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	e.fake.setRefundOutcome("requires_review")
	r, err := e.refund(acme, p.ID, "refund-1", 0)
	if err != nil || r.Status != payment.RefundPending {
		t.Fatalf("refund = %+v, %v; want pending", r, err)
	}
}

func TestRefundWebhookAfterSynchronousSuccessChangesNothing(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	r, err := e.refund(acme, p.ID, "refund-1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	ev := gateway.Event{ID: "evt_r", Kind: gateway.EventRefundSucceeded, RefundID: r.ID, RefundRef: r.ProviderRef, Amount: 1000}
	if res := e.deliver("fake", ev); res.Outcome != payment.OutcomeIgnored {
		t.Fatalf("outcome %v, want ignored", res)
	}
	if got := e.get(acme, p.ID); got.AmountRefunded != 1000 {
		t.Fatalf("amount refunded = %d, want 1000", got.AmountRefunded)
	}
}

func TestRefundAnswerIsRecordedWhenClientHangsUp(t *testing.T) {
	e := newEnv(t)
	p := e.paid(acme, "order-1")
	ctx, hangUp := context.WithCancel(e.ctx)
	e.fake.setHook(func(op string) {
		if op == "refund" {
			hangUp()
		}
	})
	if _, _, err := e.svc.Refund(ctx, acme, p.ID, "refund-1", payment.RefundRequest{}); err != nil {
		t.Fatalf("refund: %v", err)
	}
	got := e.get(acme, p.ID)
	wantStatus(t, got, payment.StatusRefunded)
	if got.Refunds[0].Status != payment.RefundSucceeded {
		t.Fatalf("refund = %+v", got.Refunds[0])
	}
}

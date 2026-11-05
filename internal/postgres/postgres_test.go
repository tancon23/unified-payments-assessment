package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
	"github.com/tanweerdev/unified-payments-assessment/internal/payment"
	"github.com/tanweerdev/unified-payments-assessment/internal/postgres"
	"github.com/tanweerdev/unified-payments-assessment/internal/postgres/pgtest"
)

func setup(t *testing.T) (*postgres.Store, payment.Payment) {
	t.Helper()
	ctx := context.Background()
	store := pgtest.New(t)
	for _, id := range []string{"acme", "globex"} {
		if err := store.UpsertTenant(ctx, id, id, id+"-key"); err != nil {
			t.Fatal(err)
		}
	}
	var p payment.Payment
	err := store.Tx(ctx, func(tx payment.Tx) (err error) {
		p, _, err = tx.CreatePayment(ctx, payment.Payment{
			ID: "pay_1", TenantID: "acme", IdempotencyKey: "k", RequestHash: []byte{1},
			Provider: "stripe", Mode: gateway.ModePayment, Amount: 2500, Currency: "usd",
			Description: "Order", SuccessURL: "https://shop.test/ok", CancelURL: "https://shop.test/cancel",
			Status: payment.StatusSucceeded,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, p
}

func claim(t *testing.T, store *postgres.Store, id string, ttl time.Duration) (string, bool) {
	t.Helper()
	var token string
	var ok bool
	err := store.Tx(context.Background(), func(tx payment.Tx) (err error) {
		token, ok, err = tx.Claim(context.Background(), payment.ResourcePayment, id, ttl)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return token, ok
}

func release(t *testing.T, store *postgres.Store, id, token string) {
	t.Helper()
	err := store.Tx(context.Background(), func(tx payment.Tx) error {
		return tx.Release(context.Background(), payment.ResourcePayment, id, token)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExpiredClaimHolderCannotReleaseTheNextClaim(t *testing.T) {
	store, p := setup(t)

	stale, ok := claim(t, store, p.ID, time.Millisecond)
	if !ok {
		t.Fatal("first claim refused")
	}
	time.Sleep(50 * time.Millisecond)
	current, ok := claim(t, store, p.ID, time.Minute)
	if !ok {
		t.Fatal("claim after expiry refused")
	}

	release(t, store, p.ID, stale)
	if _, ok := claim(t, store, p.ID, time.Minute); ok {
		t.Fatal("a stale holder released the current claim")
	}
	release(t, store, p.ID, current)
	if _, ok := claim(t, store, p.ID, time.Minute); !ok {
		t.Fatal("the current holder could not release its claim")
	}
}

func TestRefundMustBelongToThePaymentsTenant(t *testing.T) {
	store, p := setup(t)
	err := store.Tx(context.Background(), func(tx payment.Tx) error {
		return tx.CreateRefund(context.Background(), payment.Refund{
			ID: "re_1", PaymentID: p.ID, TenantID: "globex", IdempotencyKey: "k",
			RequestHash: []byte{1}, Amount: 100, Status: payment.RefundUnknown,
		})
	})
	if err == nil {
		t.Fatal("a refund was stored under another tenant than its payment")
	}
}

func TestConcurrentMigrationsDoNotRace(t *testing.T) {
	pool := pgtest.Open(t)
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- postgres.Migrate(context.Background(), pool) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("migration: %v", err)
		}
	}
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrating an up-to-date database: %v", err)
	}
}

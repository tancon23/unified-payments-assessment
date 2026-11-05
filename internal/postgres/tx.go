package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tanweerdev/unified-payments-assessment/internal/gateway"
	"github.com/tanweerdev/unified-payments-assessment/internal/payment"
)

type txStore struct {
	tx pgx.Tx
}

var _ payment.Tx = (*txStore)(nil)

const paymentColumns = `id, tenant_id, idempotency_key, request_hash, provider, mode, amount_minor,
	currency, billing_interval, description, success_url, cancel_url, status, refunded_minor,
	COALESCE(checkout_ref, ''), COALESCE(checkout_url, ''), COALESCE(payment_ref, ''),
	failure_reason, attempts, flagged, flag_reason, next_check_at, created_at, updated_at`

// refundColumns expects the refunds table aliased as r.
const refundColumns = `r.id, r.payment_id, r.tenant_id, r.idempotency_key, r.request_hash, r.amount_minor,
	r.status, COALESCE(r.provider_ref, ''), r.failure_reason, r.attempts, r.flagged, r.flag_reason,
	r.next_check_at, r.created_at, r.updated_at`

func scanPayment(row pgx.Row) (payment.Payment, error) {
	var p payment.Payment
	var next *time.Time
	err := row.Scan(&p.ID, &p.TenantID, &p.IdempotencyKey, &p.RequestHash, &p.Provider, &p.Mode, &p.Amount,
		&p.Currency, &p.Interval, &p.Description, &p.SuccessURL, &p.CancelURL, &p.Status, &p.AmountRefunded,
		&p.CheckoutRef, &p.CheckoutURL, &p.PaymentRef,
		&p.FailureReason, &p.Attempts, &p.Flagged, &p.FlagReason, &next, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, payment.ErrNotFound
	}
	if next != nil {
		p.NextCheckAt = *next
	}
	return p, err
}

func scanRefund(row pgx.Row) (payment.Refund, error) {
	var r payment.Refund
	var next *time.Time
	err := row.Scan(&r.ID, &r.PaymentID, &r.TenantID, &r.IdempotencyKey, &r.RequestHash, &r.Amount,
		&r.Status, &r.ProviderRef, &r.FailureReason, &r.Attempts, &r.Flagged, &r.FlagReason,
		&next, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Refund{}, payment.ErrNotFound
	}
	if next != nil {
		r.NextCheckAt = *next
	}
	return r, err
}

func collect[T any](rows pgx.Rows, err error, scan func(pgx.Row) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (T, error) { return scan(row) })
}

// nullTime stores the zero time as NULL.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func expectOneRow(tag interface{ RowsAffected() int64 }, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return payment.ErrNotFound
	}
	return nil
}

func (t *txStore) CreatePayment(ctx context.Context, p payment.Payment) (payment.Payment, bool, error) {
	stored, err := scanPayment(t.tx.QueryRow(ctx, `
		INSERT INTO payments (id, tenant_id, idempotency_key, request_hash, provider, mode, amount_minor,
		                      currency, billing_interval, description, success_url, cancel_url, status, next_check_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
		RETURNING `+paymentColumns,
		p.ID, p.TenantID, p.IdempotencyKey, p.RequestHash, p.Provider, p.Mode, p.Amount,
		p.Currency, p.Interval, p.Description, p.SuccessURL, p.CancelURL, p.Status, nullTime(p.NextCheckAt)))
	if !errors.Is(err, payment.ErrNotFound) {
		return stored, err == nil, err
	}
	// The key is taken. A concurrent insert of it has committed by now:
	// ON CONFLICT waits for it, and each statement sees fresh data.
	stored, err = scanPayment(t.tx.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE tenant_id = $1 AND idempotency_key = $2`,
		p.TenantID, p.IdempotencyKey))
	return stored, false, err
}

func (t *txStore) GetPayment(ctx context.Context, tenantID, id string) (payment.Payment, error) {
	p, err := scanPayment(t.tx.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE id = $1 AND tenant_id = $2`, id, tenantID))
	if err != nil {
		return payment.Payment{}, err
	}
	rows, err := t.tx.Query(ctx,
		`SELECT `+refundColumns+` FROM refunds r WHERE r.payment_id = $1 ORDER BY r.created_at, r.id`, id)
	p.Refunds, err = collect(rows, err, scanRefund)
	return p, err
}

func (t *txStore) LockPayment(ctx context.Context, id string) (payment.Payment, error) {
	return scanPayment(t.tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE id = $1 FOR UPDATE`, id))
}

func (t *txStore) LockTenantPayment(ctx context.Context, tenantID, id string) (payment.Payment, error) {
	return scanPayment(t.tx.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID))
}

func (t *txStore) LockPaymentForEvent(ctx context.Context, provider, paymentID, checkoutRef string) (payment.Payment, error) {
	return scanPayment(t.tx.QueryRow(ctx, `
		SELECT `+paymentColumns+` FROM payments
		WHERE provider = $1 AND (id = $2 OR checkout_ref = $3)
		ORDER BY id = $2 DESC
		LIMIT 1
		FOR UPDATE`, provider, paymentID, checkoutRef))
}

func (t *txStore) UpdatePayment(ctx context.Context, p payment.Payment) error {
	return expectOneRow(t.tx.Exec(ctx, `
		UPDATE payments SET
			status = $2, refunded_minor = $3,
			checkout_ref = NULLIF($4, ''), checkout_url = NULLIF($5, ''), payment_ref = NULLIF($6, ''),
			failure_reason = $7, attempts = $8, flagged = $9, flag_reason = $10,
			next_check_at = $11, updated_at = now()
		WHERE id = $1`,
		p.ID, p.Status, p.AmountRefunded, p.CheckoutRef, p.CheckoutURL, p.PaymentRef,
		p.FailureReason, p.Attempts, p.Flagged, p.FlagReason, nullTime(p.NextCheckAt)))
}

func (t *txStore) DuePayments(ctx context.Context, now time.Time, limit int) ([]payment.Payment, error) {
	rows, err := t.tx.Query(ctx, `
		SELECT `+paymentColumns+` FROM payments
		WHERE status IN ('pending', 'unknown', 'authorized') AND NOT flagged AND next_check_at <= $1
		  AND (claimed_until IS NULL OR claimed_until < now())
		ORDER BY next_check_at
		LIMIT $2`, now, limit)
	return collect(rows, err, scanPayment)
}

func (t *txStore) RefundByIdempotencyKey(ctx context.Context, tenantID, key string) (payment.Refund, error) {
	return scanRefund(t.tx.QueryRow(ctx,
		`SELECT `+refundColumns+` FROM refunds r WHERE r.tenant_id = $1 AND r.idempotency_key = $2`, tenantID, key))
}

func (t *txStore) CreateRefund(ctx context.Context, r payment.Refund) error {
	err := expectOneRow(t.tx.Exec(ctx, `
		INSERT INTO refunds (id, payment_id, tenant_id, idempotency_key, request_hash, amount_minor, status, next_check_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		r.ID, r.PaymentID, r.TenantID, r.IdempotencyKey, r.RequestHash, r.Amount, r.Status, nullTime(r.NextCheckAt)))
	if errors.Is(err, payment.ErrNotFound) {
		return payment.ErrIdempotencyMismatch
	}
	return err
}

func (t *txStore) FindRefundForEvent(ctx context.Context, provider, refundID, refundRef string) (payment.Refund, error) {
	return scanRefund(t.tx.QueryRow(ctx, `
		SELECT `+refundColumns+` FROM refunds r JOIN payments p ON p.id = r.payment_id
		WHERE p.provider = $1 AND (r.id = $2 OR r.provider_ref = $3)
		ORDER BY r.id = $2 DESC
		LIMIT 1`, provider, refundID, refundRef))
}

func (t *txStore) LockRefund(ctx context.Context, id string) (payment.Refund, error) {
	return scanRefund(t.tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds r WHERE r.id = $1 FOR UPDATE`, id))
}

func (t *txStore) UpdateRefund(ctx context.Context, r payment.Refund) error {
	return expectOneRow(t.tx.Exec(ctx, `
		UPDATE refunds SET
			status = $2, provider_ref = NULLIF($3, ''), failure_reason = $4,
			attempts = $5, flagged = $6, flag_reason = $7, next_check_at = $8, updated_at = now()
		WHERE id = $1`,
		r.ID, r.Status, r.ProviderRef, r.FailureReason, r.Attempts, r.Flagged, r.FlagReason, nullTime(r.NextCheckAt)))
}

func (t *txStore) ReservedRefundAmount(ctx context.Context, paymentID string) (int64, error) {
	var sum int64
	err := t.tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_minor), 0) FROM refunds
		WHERE payment_id = $1 AND status <> 'failed'`, paymentID).Scan(&sum)
	return sum, err
}

func (t *txStore) DueRefunds(ctx context.Context, now time.Time, limit int) ([]payment.Refund, error) {
	rows, err := t.tx.Query(ctx, `
		SELECT `+refundColumns+` FROM refunds r
		WHERE r.status IN ('unknown', 'pending') AND NOT r.flagged AND r.next_check_at <= $1
		  AND (r.claimed_until IS NULL OR r.claimed_until < now())
		ORDER BY r.next_check_at
		LIMIT $2`, now, limit)
	return collect(rows, err, scanRefund)
}

var claimTables = map[payment.Resource]string{
	payment.ResourcePayment: "payments",
	payment.ResourceRefund:  "refunds",
}

func claimTable(r payment.Resource) (string, error) {
	table, ok := claimTables[r]
	if !ok {
		return "", fmt.Errorf("unknown resource %q", r)
	}
	return table, nil
}

func (t *txStore) Claim(ctx context.Context, r payment.Resource, id string, ttl time.Duration) (string, bool, error) {
	table, err := claimTable(r)
	if err != nil {
		return "", false, err
	}
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	tag, err := t.tx.Exec(ctx, `
		UPDATE `+table+` SET claimed_by = $2, claimed_until = now() + $3 * interval '1 millisecond'
		WHERE id = $1 AND (claimed_until IS NULL OR claimed_until < now())`, id, token, ttl.Milliseconds())
	if err != nil || tag.RowsAffected() != 1 {
		return "", false, err
	}
	return token, true, nil
}

func (t *txStore) Release(ctx context.Context, r payment.Resource, id, token string) error {
	table, err := claimTable(r)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx,
		`UPDATE `+table+` SET claimed_by = NULL, claimed_until = NULL WHERE id = $1 AND claimed_by = $2`, id, token)
	return err
}

func (t *txStore) RecordEvent(ctx context.Context, provider string, ev gateway.Event) (bool, error) {
	tag, err := t.tx.Exec(ctx, `
		INSERT INTO webhook_events (provider, event_id, event_type) VALUES ($1, $2, $3)
		ON CONFLICT (provider, event_id) DO NOTHING`, provider, ev.ID, ev.Type)
	return err == nil && tag.RowsAffected() == 1, err
}

func (t *txStore) SetEventOutcome(ctx context.Context, provider, eventID, outcome string) error {
	return expectOneRow(t.tx.Exec(ctx,
		`UPDATE webhook_events SET outcome = $3 WHERE provider = $1 AND event_id = $2`, provider, eventID, outcome))
}

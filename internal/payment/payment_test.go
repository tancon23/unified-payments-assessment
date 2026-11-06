package payment_test

import (
	"testing"

	"github.com/tancon23/unified-payments-assessment/internal/payment"
)

func TestStatusTransitions(t *testing.T) {
	all := []payment.Status{
		payment.StatusPending, payment.StatusUnknown, payment.StatusAuthorized, payment.StatusSucceeded,
		payment.StatusFailed, payment.StatusExpired, payment.StatusPartiallyRefunded, payment.StatusRefunded,
	}
	allowed := map[[2]payment.Status]bool{
		{payment.StatusPending, payment.StatusUnknown}:             true,
		{payment.StatusPending, payment.StatusAuthorized}:          true,
		{payment.StatusPending, payment.StatusSucceeded}:           true,
		{payment.StatusPending, payment.StatusFailed}:              true,
		{payment.StatusPending, payment.StatusExpired}:             true,
		{payment.StatusUnknown, payment.StatusPending}:             true,
		{payment.StatusUnknown, payment.StatusAuthorized}:          true,
		{payment.StatusUnknown, payment.StatusSucceeded}:           true,
		{payment.StatusUnknown, payment.StatusFailed}:              true,
		{payment.StatusUnknown, payment.StatusExpired}:             true,
		{payment.StatusAuthorized, payment.StatusSucceeded}:        true,
		{payment.StatusAuthorized, payment.StatusFailed}:           true,
		{payment.StatusSucceeded, payment.StatusPartiallyRefunded}: true,
		{payment.StatusSucceeded, payment.StatusRefunded}:          true,
		{payment.StatusPartiallyRefunded, payment.StatusRefunded}:  true,
	}
	for _, from := range all {
		for _, to := range all {
			if got := from.CanBecome(to); got != allowed[[2]payment.Status{from, to}] {
				t.Errorf("%s -> %s: CanBecome = %v", from, to, got)
			}
		}
	}
}

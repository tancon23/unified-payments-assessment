package paddle_test

import (
	"testing"

	"github.com/tancon23/unified-payments-assessment/internal/provider/paddle"
	"github.com/tancon23/unified-payments-assessment/internal/provider/providertest"
)

func TestContract(t *testing.T) {
	providertest.Run(t, providertest.Harness{Provider: paddle.New(), Placeholder: true})
}

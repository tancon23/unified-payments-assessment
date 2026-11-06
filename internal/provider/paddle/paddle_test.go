package paddle_test

import (
	"testing"

	"github.com/tanweerdev/unified-payments-assessment/internal/provider/paddle"
	"github.com/tanweerdev/unified-payments-assessment/internal/provider/providertest"
)

func TestContract(t *testing.T) {
	providertest.Run(t, providertest.Harness{Provider: paddle.New(), Placeholder: true})
}

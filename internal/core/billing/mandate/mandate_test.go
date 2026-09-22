package mandate_test

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/billing/mandate"
)

// The webhook never reads through the entitlement service, so a nil one would
// mount, take deliveries, and panic only once a buyer had paid. Failing here
// moves that to wiring.
func TestNewServiceRejectsANilEntitlementService(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewService accepted a nil entitlement service")
		}
	}()
	mandate.NewService(nil, nil, nil, nil)
}

package mandate_test

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/billing/mandate"
)

// A nil entitlement service fails nothing at wiring on its own: the webhook would
// mount and store deliveries, then panic applying one, as would the dashboard's
// first billing call. Failing here moves that to wiring.
func TestNewServiceRejectsANilEntitlementService(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewService accepted a nil entitlement service")
		}
	}()
	mandate.NewService(nil, nil, nil, nil)
}

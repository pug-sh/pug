package server

import (
	"testing"

	pogrpc "github.com/pug-sh/pug/internal/app/server/rpc"
)

// TestAssertServedServicesMatch exercises the runtime AUTHZ CONTRACT check used
// by start(): the mounted RPC-service set must equal the authz permission
// registry. It runs in short mode (no infra) — it only exercises the pure
// comparison against pogrpc.ServedServiceNames().
func TestAssertServedServicesMatch(t *testing.T) {
	names := pogrpc.ServedServiceNames()
	if len(names) == 0 {
		t.Fatal("ServedServiceNames returned empty; expected the served RPC services")
	}

	full := make(map[string]bool, len(names))
	for _, n := range names {
		full[n] = true
	}

	// The exact served set satisfies the contract.
	if err := assertServedServicesMatch(full); err != nil {
		t.Fatalf("full served set must satisfy the contract: %v", err)
	}

	// A service mounted without an authz decision must fail (the case the
	// contract exists to catch: a new service shipped without a registry entry).
	withExtra := map[string]bool{"bogus.unregistered.v1.Service": true}
	for n := range full {
		withExtra[n] = true
	}
	if err := assertServedServicesMatch(withExtra); err == nil {
		t.Error("expected error for a mounted service with no authz decision")
	}

	// An authorized service that isn't mounted must also fail (stale registry).
	missing := make(map[string]bool, len(full))
	for n := range full {
		missing[n] = true
	}
	delete(missing, names[0])
	if err := assertServedServicesMatch(missing); err == nil {
		t.Error("expected error for an authz decision with no mounted service")
	}
}

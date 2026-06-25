package server

import (
	"fmt"

	pogrpc "github.com/pug-sh/pug/internal/app/server/rpc"
)

// assertServedServicesMatch verifies the set of RPC services mounted by start()
// exactly matches the authz permission registry (pogrpc.ServedServiceNames). A
// mismatch is a wiring bug — a service mounted without an authz decision, or an
// authz decision with no mounted service — so start() fails fast rather than
// serve an unauthorized (or dead) route.
//
// This is the RUNTIME half of the "no RPC ships without an authz decision"
// contract; the compile/test half lives in
// rpc.TestPermissionRegistryCoversAllProcedures, which pins the registry to the
// generated handler interfaces. Together they close the loop:
// mounted ⊆⊇ registry (here) and registry ⊆⊇ generated procedures (there).
func assertServedServicesMatch(mounted map[string]bool) error {
	authorized := make(map[string]bool, len(mounted))
	for _, name := range pogrpc.ServedServiceNames() {
		authorized[name] = true
	}
	for name := range mounted {
		if !authorized[name] {
			return fmt.Errorf("server: RPC service %q is mounted but has no authz decision in the permission registry", name)
		}
	}
	for name := range authorized {
		if !mounted[name] {
			return fmt.Errorf("server: authz registry lists %q but it is not mounted", name)
		}
	}
	return nil
}

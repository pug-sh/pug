package rpc

import (
	"testing"

	"github.com/pug-sh/pug/internal/app/server/rpc/authzspec"
)

// servedServices, servedProcedures, and AssertRegistryMatchesServedProcedures live
// in authz_served.go (non-test): the same procedure-level contract runs at startup
// (server.start) and here in CI, so the two cannot drift.

// TestPermissionRegistryCoversAllProcedures is the "no RPC ships without an
// authz decision" contract: every served procedure must have a registry entry,
// and every registry entry must correspond to a served procedure. It reports every
// mismatch (vs. the first-error startup assertion) for better CI diagnostics.
func TestPermissionRegistryCoversAllProcedures(t *testing.T) {
	served := servedProcedures()

	for proc := range served {
		if _, ok := permissionRegistry[proc]; !ok {
			t.Errorf("served RPC %q has no permissionRegistry entry — add an explicit authz decision", proc)
		}
	}
	for proc := range permissionRegistry {
		if !served[proc] {
			t.Errorf("permissionRegistry has stale entry %q (no such served RPC)", proc)
		}
	}
}

// TestAssertRegistryMatchesServedProcedures exercises the startup assertion itself
// (the fail-fast guard server.start runs) against the real registry, so a
// regression in it is caught in CI.
func TestAssertRegistryMatchesServedProcedures(t *testing.T) {
	if err := AssertRegistryMatchesServedProcedures(); err != nil {
		t.Error(err)
	}
}

// TestAssertRegistryCoversProcedures pins both rejection paths of the startup
// guard with synthetic inputs: a served procedure missing from the registry (the
// "new method on a mounted service" case this whole change targets) and a stale
// registry entry. Proves the guard actually fails closed, not just that the real
// sets happen to match.
func TestAssertRegistryCoversProcedures(t *testing.T) {
	reg := map[string]authzspec.Spec{"/svc/A": authzspec.Public()}

	if err := assertRegistryCoversProcedures(map[string]bool{"/svc/A": true}, reg); err != nil {
		t.Errorf("matching sets returned error: %v", err)
	}
	if err := assertRegistryCoversProcedures(map[string]bool{"/svc/A": true, "/svc/B": true}, reg); err == nil {
		t.Error("served procedure with no registry entry was not rejected")
	}
	if err := assertRegistryCoversProcedures(map[string]bool{}, reg); err == nil {
		t.Error("stale registry entry (no served procedure) was not rejected")
	}
}

// TestPermissionRegistryRoleEntriesAreComplete asserts every entry is built by an
// authzspec constructor (not a bare Spec{}) and that every role-gated entry
// carries the full triple the interceptor enforces (resource + action +
// orgSource). The reverse — a non-role-gated entry carrying a stray triple — is
// now impossible to express (authzspec.Spec's fields are unexported and only the
// constructors set them), so it needs no check.
func TestPermissionRegistryRoleEntriesAreComplete(t *testing.T) {
	for proc, spec := range permissionRegistry {
		if !spec.Defined() {
			t.Errorf("registry entry %q is an undefined (zero) Spec — build it with an authzspec constructor", proc)
		}
		if !spec.IsRoleGated() {
			continue
		}
		if spec.Resource() == "" || spec.Action() == "" {
			t.Errorf("role-gated entry %q must set resource and action", proc)
		}
		if spec.OrgSource() != authzspec.OrgFromMessage && spec.OrgSource() != authzspec.OrgFromProject {
			t.Errorf("role-gated entry %q must set a valid orgSource", proc)
		}
	}
}

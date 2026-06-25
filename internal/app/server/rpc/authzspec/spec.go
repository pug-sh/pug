// Package authzspec defines the per-RPC authorization decision (Spec) that the
// permission registry maps every served procedure to and that AuthzInterceptor
// enforces.
//
// Its reason to be a separate package is to make the
// "role-gated ⟺ resource+action+orgSource" invariant correct BY CONSTRUCTION.
// Spec's fields are unexported, so the registry (in another package) can build a
// Spec only through the constructors here:
//
//   - A role-gated Spec cannot be written as a bare struct literal that forgets
//     orgSource — OrgGated/ProjGated always set the full triple.
//   - A non-role-gated literal cannot carry a stray (resource, action) that would
//     read as gated but be silently ignored — the fields can't be set from
//     outside the package.
//   - The lone literal still expressible from outside, authzspec.Spec{}, is
//     kindUnset and is rejected by the registry-completeness test (Defined()).
//
// What was previously an invariant enforced only by a test is now structural.
package authzspec

import "github.com/pug-sh/pug/internal/core/authz"

// kind classifies how a procedure is authorized — documentation for the reader,
// plus the single runtime bit the interceptor needs (IsRoleGated). The
// distinctions among the non-gated kinds are carried by which constructor built
// the Spec; only the role-gated kind is enforced.
type kind int

const (
	// kindUnset is the zero value — a bare Spec{}. It is neither gated nor a valid
	// decision; the registry-completeness test rejects it via Defined().
	kindUnset kind = iota
	// kindPublic: no authentication (the auth service; public share links).
	kindPublic
	// kindSelf: authenticated customer operating on their OWN data / orgs; no
	// org-role gate (e.g. GetMe, List/Create/Leave orgs).
	kindSelf
	// kindRoleGated: an org-role gate applies — the interceptor resolves the
	// caller's role and checks (resource, action). The only enforced kind.
	kindRoleGated
	// kindProject: project-scoped via x-project-id, with NO org-role gate —
	// project access is fully established at auth time (Principal.Project is set
	// only for org members) and the RPC merely echoes it (projects.Get). A role
	// check would be redundant: the only read it allows is in every role's floor.
	kindProject
	// kindSDKKey: API-key project scope (SDK, write-only).
	kindSDKKey
)

// OrgSource says how AuthzInterceptor resolves the org a role-gated RPC is
// authorized in — the one thing that varies per role-gated RPC, and what makes a
// single generic interceptor viable across both planes. The zero value is "no org
// source"; a non-role-gated Spec has it by construction.
type OrgSource int

const (
	// OrgFromMessage: the request message carries org_id, read via the generated
	// interface{ GetOrgId() string } (orgs.*, projects BatchGet/Create,
	// orgemailproviders.*). The org control plane.
	OrgFromMessage OrgSource = iota + 1
	// OrgFromProject: the org is the x-project-id project's org
	// (principal.Project.OrgID) — dashboards / insights / activity / profiles and
	// the project-lifecycle writes. The project data plane.
	OrgFromProject
)

// String renders an OrgSource for test output and diagnostics.
func (o OrgSource) String() string {
	switch o {
	case OrgFromMessage:
		return "message"
	case OrgFromProject:
		return "project"
	default:
		return "none"
	}
}

// Spec is the authorization decision for one RPC. It is immutable and can only be
// built via the constructors below — which is what makes the role-gated invariant
// hold by construction. For a role-gated Spec, AuthzInterceptor reads
// (Resource, Action, OrgSource) and enforces them, so the registry is the single
// ENFORCED source of truth: a drifted pair changes real behavior and is caught by
// the interceptor tests.
type Spec struct {
	kind      kind
	resource  authz.Resource
	action    authz.Action
	orgSource OrgSource
	note      string
}

func firstNote(notes []string) string {
	if len(notes) > 0 {
		return notes[0]
	}
	return ""
}

// Public builds a no-auth Spec.
func Public(notes ...string) Spec { return Spec{kind: kindPublic, note: firstNote(notes)} }

// Self builds a Spec for an authenticated customer acting on their OWN data — no
// org-role gate.
func Self(notes ...string) Spec { return Spec{kind: kindSelf, note: firstNote(notes)} }

// Project builds a project-scoped, no-role-gate Spec: project access is already
// established at auth time and the RPC merely echoes it (projects.Get).
func Project(notes ...string) Spec { return Spec{kind: kindProject, note: firstNote(notes)} }

// SDKKey builds an API-key (SDK, write-only) Spec.
func SDKKey(notes ...string) Spec { return Spec{kind: kindSDKKey, note: firstNote(notes)} }

// OrgGated builds a role-gated Spec whose org_id comes from the request message
// (org control plane). resource+action are the enforced permission.
func OrgGated(resource authz.Resource, action authz.Action, notes ...string) Spec {
	return Spec{kind: kindRoleGated, resource: resource, action: action, orgSource: OrgFromMessage, note: firstNote(notes)}
}

// ProjGated builds a role-gated Spec whose org is the x-project-id project's org
// (project data plane / project-lifecycle writes).
func ProjGated(resource authz.Resource, action authz.Action, notes ...string) Spec {
	return Spec{kind: kindRoleGated, resource: resource, action: action, orgSource: OrgFromProject, note: firstNote(notes)}
}

// Defined reports whether the Spec was built by a constructor (i.e. is not a bare
// zero Spec{}). The registry-completeness test asserts every entry is Defined.
func (s Spec) Defined() bool { return s.kind != kindUnset }

// IsRoleGated reports whether AuthzInterceptor must enforce this Spec. It is the
// only runtime distinction; the non-gated kinds differ only as documentation.
func (s Spec) IsRoleGated() bool { return s.kind == kindRoleGated }

// Resource is the enforced resource for a role-gated Spec (empty otherwise).
func (s Spec) Resource() authz.Resource { return s.resource }

// Action is the enforced action for a role-gated Spec (empty otherwise).
func (s Spec) Action() authz.Action { return s.action }

// OrgSource is how the interceptor resolves the org for a role-gated Spec (zero
// otherwise).
func (s Spec) OrgSource() OrgSource { return s.orgSource }

// Note returns the optional documentation note attached at construction.
func (s Spec) Note() string { return s.note }

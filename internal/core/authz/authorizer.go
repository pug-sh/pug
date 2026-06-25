package authz

import (
	"fmt"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
)

// Authorizer answers role -> permission questions against the static policy.
//
// It is immutable after construction: the policy is loaded once and we never
// call casbin's mutating policy-management APIs, so we need no lock of our own.
// Concurrent Authorize is safe because casbin guards its own shared state — the
// CachedEnforcer serializes decision-cache access under a mutex (the cache IS
// written on every miss) and the role manager guards link lookups with a mutex.
// Verified against casbin/v2 v2.135.0 for our Enforce-only, immutable-policy use;
// re-check on a casbin upgrade.
type Authorizer struct {
	enforcer *casbin.CachedEnforcer
}

// NewAuthorizer builds an Authorizer from the in-code model + policy. It returns
// an error only if the static model/policy is malformed — a programming error
// caught by TestNewAuthorizer and at startup via NewAuthorizer in newDeps.
func NewAuthorizer() (*Authorizer, error) {
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, fmt.Errorf("authz: load model: %w", err)
	}

	enforcer, err := casbin.NewCachedEnforcer(m)
	if err != nil {
		return nil, fmt.Errorf("authz: new enforcer: %w", err)
	}
	// The policy is in-memory and never persisted; disable auto-save so adding
	// rules never tries to reach a (non-existent) adapter.
	enforcer.EnableAutoSave(false)

	if _, err := enforcer.AddPolicies(policyRules); err != nil {
		return nil, fmt.Errorf("authz: add policies: %w", err)
	}
	if _, err := enforcer.AddGroupingPolicies(groupingRules); err != nil {
		return nil, fmt.Errorf("authz: add grouping policies: %w", err)
	}

	return &Authorizer{enforcer: enforcer}, nil
}

// Authorize reports whether role permits action on resource.
//
// role is the stored role string (e.g. "ORG_ROLE_ADMIN"). Callers pass an
// already-validated role (the rpc layer resolves it via orgs.GetMemberRole); an
// unknown or empty role matches no policy rule and fails closed (deny). A non-nil
// error indicates an enforcement failure (malformed input or a broken policy),
// never an ordinary denial — callers map it to an internal error.
//
// A principal holds exactly one org role today, so this takes a single role. If
// multi-role assignment (or API-key scopes) ever lands, reintroduce an OR-any
// match over a role set here — it is intentionally not built ahead of need.
func (a *Authorizer) Authorize(role string, resource Resource, action Action) (bool, error) {
	ok, err := a.enforcer.Enforce(role, string(resource), string(action))
	if err != nil {
		return false, fmt.Errorf("authz: enforce: %w", err)
	}
	return ok, nil
}

package email

import "context"

// ResolvedFrom carries sender identity overrides chosen per send.
// Empty fields mean "use the operator default."
type ResolvedFrom struct {
	From    string
	ReplyTo string
}

// ProviderResolver picks the right Provider for an email send. tenantID is
// nil for platform-scoped emails (signup, password reset, verification);
// non-nil for org-scoped emails (today only org_member_invite).
type ProviderResolver interface {
	Resolve(ctx context.Context, tenantID *string) (Provider, ResolvedFrom, error)
}

// OperatorOnlyResolver returns the operator-configured provider for every
// resolve call. Used when PUG_EMAIL_PROVIDER_SECRET_KEY is unset, preserving
// the pre-BYOP behaviour. Used internally by NewService to wrap a single
// Provider so existing call sites keep working.
type OperatorOnlyResolver struct {
	Provider Provider
	From     string
	ReplyTo  string
}

func (r *OperatorOnlyResolver) Resolve(_ context.Context, _ *string) (Provider, ResolvedFrom, error) {
	return r.Provider, ResolvedFrom{From: r.From, ReplyTo: r.ReplyTo}, nil
}

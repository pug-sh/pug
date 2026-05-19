package email

import "context"

// ResolvedFrom carries sender identity overrides chosen per send.
// Empty fields mean "use the operator default."
type ResolvedFrom struct {
	From    string
	ReplyTo string
}

// ProviderResolver picks the Provider for an email send. tenantID is nil for
// platform-scoped emails and non-nil for org-scoped emails.
type ProviderResolver interface {
	Resolve(ctx context.Context, tenantID *string) (Provider, ResolvedFrom, error)
}

// OperatorOnlyResolver always returns the operator-configured provider. Used
// when PUG_EMAIL_PROVIDER_SECRET_KEY is unset.
type OperatorOnlyResolver struct {
	Provider Provider
	From     string
	ReplyTo  string
}

func (r *OperatorOnlyResolver) Resolve(_ context.Context, _ *string) (Provider, ResolvedFrom, error) {
	return r.Provider, ResolvedFrom{From: r.From, ReplyTo: r.ReplyTo}, nil
}

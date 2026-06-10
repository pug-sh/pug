package oauth

import authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"

// ProviderFromProto maps a public auth proto enum to a configured provider name.
func ProviderFromProto(p authv1.OAuthProvider) (ProviderName, error) {
	switch p {
	case authv1.OAuthProvider_O_AUTH_PROVIDER_GOOGLE:
		return ProviderGoogle, nil
	default:
		return "", ErrOAuthProviderDisabled
	}
}

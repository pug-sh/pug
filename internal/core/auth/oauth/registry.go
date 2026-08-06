package oauth

import (
	"context"
)

// Registry maps provider names to configured Provider implementations.
type Registry struct {
	providers map[ProviderName]Provider
}

func NewRegistry(providers ...Provider) *Registry {
	m := make(map[ProviderName]Provider, len(providers))
	for _, p := range providers {
		m[p.Name()] = p
	}
	return &Registry{providers: m}
}

func (r *Registry) Get(name ProviderName) (Provider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, ErrOAuthProviderDisabled
	}
	return p, nil
}

// NewRegistryFromConfig builds providers for all configured IdPs.
func NewRegistryFromConfig(ctx context.Context, cfg Config) (*Registry, error) {
	providers := make([]Provider, 0, len(cfg.Providers))
	for _, providerCfg := range cfg.Providers {
		if providerCfg.Type != ProviderTypeOIDC {
			return nil, ErrOAuthProviderDisabled
		}
		provider, err := newOIDCProvider(ctx, providerCfg)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return NewRegistry(providers...), nil
}

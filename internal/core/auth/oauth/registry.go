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
	var providers []Provider
	if cfg.IsProviderEnabled(ProviderGoogle) {
		g, err := newGoogleProvider(ctx, cfg.GoogleClientID, cfg.GoogleClientSecret)
		if err != nil {
			return nil, err
		}
		providers = append(providers, g)
	}
	return NewRegistry(providers...), nil
}

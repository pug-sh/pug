package oauth

import (
	"fmt"
	"net/http"
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

// NewRegistryFromConfig builds providers for all configured IdPs. It performs no
// I/O; each provider discovers its issuer on first use.
func NewRegistryFromConfig(cfg Config, httpClient *http.Client) (*Registry, error) {
	providers := make([]Provider, 0, len(cfg.Providers))
	for _, providerCfg := range cfg.Providers {
		if providerCfg.Type != ProviderTypeOIDC {
			return nil, fmt.Errorf("oauth: provider %q has unsupported type %q", providerCfg.ID, providerCfg.Type)
		}
		providers = append(providers, newOIDCProvider(providerCfg, httpClient))
	}
	return NewRegistry(providers...), nil
}

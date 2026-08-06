package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/sethvargo/go-envconfig"
)

const Version = 1

type ProviderType string

const (
	ProviderTypeGoogle ProviderType = "google"
	ProviderTypeOIDC   ProviderType = "oidc"
)

// Config is the versioned, file-backed Pug configuration envelope. Operational
// settings continue to use environment variables; this file is for structured
// settings whose cardinality cannot be represented cleanly as flat variables.
type Config struct {
	Schema  string     `json:"$schema,omitempty"`
	Version int        `json:"version"`
	Auth    AuthConfig `json:"auth"`
}

type AuthConfig struct {
	Providers []AuthProvider `json:"providers,omitempty"`
}

type AuthProvider struct {
	ID          string       `json:"id"`
	Type        ProviderType `json:"type"`
	DisplayName string       `json:"displayName"`
	ClientID    string       `json:"clientId"`
	IssuerURL   string       `json:"issuerUrl,omitempty"`
	Scopes      []string     `json:"scopes,omitempty"`
}

type envConfig struct {
	ConfigFile     string `env:"PUG_CONFIG_FILE"`
	GoogleClientID string `env:"PUG_OAUTH_GOOGLE_CLIENT_ID"`
}

var providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// Load reads the optional PUG_CONFIG_FILE. The legacy Google client-id variable
// remains supported when no file is configured so existing installations keep
// working without changes.
func Load(ctx context.Context) (Config, error) {
	var env envConfig
	if err := envconfig.Process(ctx, &env); err != nil {
		return Config{}, err
	}

	if env.ConfigFile == "" {
		cfg := Config{Version: Version}
		if env.GoogleClientID != "" {
			cfg.Auth.Providers = []AuthProvider{{
				ID:          "google",
				Type:        ProviderTypeGoogle,
				DisplayName: "Google",
				ClientID:    env.GoogleClientID,
			}}
		}
		return cfg, nil
	}
	if env.GoogleClientID != "" {
		return Config{}, errors.New("PUG_CONFIG_FILE and PUG_OAUTH_GOOGLE_CLIENT_ID cannot be used together")
	}

	f, err := os.Open(env.ConfigFile)
	if err != nil {
		return Config{}, fmt.Errorf("open %s: %w", env.ConfigFile, err)
	}
	defer func() { _ = f.Close() }()

	var cfg Config
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", env.ConfigFile, err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", env.ConfigFile, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate %s: %w", env.ConfigFile, err)
	}
	return cfg, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (c *Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("version must be %d", Version)
	}

	seen := make(map[string]struct{}, len(c.Auth.Providers))
	for i := range c.Auth.Providers {
		p := &c.Auth.Providers[i]
		prefix := fmt.Sprintf("auth.providers[%d]", i)
		if !providerIDPattern.MatchString(p.ID) {
			return fmt.Errorf("%s.id must match %s", prefix, providerIDPattern)
		}
		if _, ok := seen[p.ID]; ok {
			return fmt.Errorf("%s.id %q is duplicated", prefix, p.ID)
		}
		seen[p.ID] = struct{}{}

		p.DisplayName = strings.TrimSpace(p.DisplayName)
		p.ClientID = strings.TrimSpace(p.ClientID)
		if p.DisplayName == "" || len([]rune(p.DisplayName)) > 64 {
			return fmt.Errorf("%s.displayName must contain 1 to 64 characters", prefix)
		}
		if p.ClientID == "" {
			return fmt.Errorf("%s.clientId is required", prefix)
		}

		switch p.Type {
		case ProviderTypeGoogle:
			if p.IssuerURL != "" || len(p.Scopes) != 0 {
				return fmt.Errorf("%s: issuerUrl and scopes are only valid for type oidc", prefix)
			}
		case ProviderTypeOIDC:
			issuer, err := validateIssuer(p.IssuerURL)
			if err != nil {
				return fmt.Errorf("%s.issuerUrl: %w", prefix, err)
			}
			p.IssuerURL = issuer
			if len(p.Scopes) == 0 {
				p.Scopes = []string{"openid", "profile", "email"}
			}
			if err := validateScopes(p.Scopes); err != nil {
				return fmt.Errorf("%s.scopes: %w", prefix, err)
			}
		default:
			return fmt.Errorf("%s.type must be %q or %q", prefix, ProviderTypeGoogle, ProviderTypeOIDC)
		}
	}
	return nil
}

func validateIssuer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must be an absolute issuer URL without credentials, query, or fragment")
	}
	localhost := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && (u.Scheme != "http" || !localhost) {
		return "", errors.New("must use HTTPS (HTTP is allowed only for localhost)")
	}
	return raw, nil
}

func validateScopes(scopes []string) error {
	seen := make(map[string]struct{}, len(scopes))
	for i, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
			return fmt.Errorf("entry %d must be one non-empty scope", i)
		}
		scopes[i] = scope
		seen[scope] = struct{}{}
	}
	if _, ok := seen["openid"]; !ok {
		return errors.New("must include openid")
	}
	return nil
}

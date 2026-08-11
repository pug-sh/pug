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

const ProviderTypeOIDC ProviderType = "oidc"

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
	// Stored as customer_identities.provider, so renaming it orphans those links.
	ID           string       `json:"id"`
	Type         ProviderType `json:"type"`
	DisplayName  string       `json:"displayName"`
	ClientID     string       `json:"clientId"`
	ClientSecret string       `json:"clientSecret,omitempty"`
	IssuerURL    string       `json:"issuerUrl,omitempty"`
	Scopes       []string     `json:"scopes,omitempty"`
}

type envConfig struct {
	ConfigFile string `env:"PUG_CONFIG_FILE"`
}

var providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// Load reads the optional PUG_CONFIG_FILE.
func Load(ctx context.Context) (Config, error) {
	var env envConfig
	if err := envconfig.Process(ctx, &env); err != nil {
		return Config{}, err
	}

	if env.ConfigFile == "" {
		return Config{Version: Version}, nil
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
		// A file-mounted k8s secret arrives with a trailing newline the IdP rejects.
		p.ClientSecret = strings.TrimSpace(p.ClientSecret)
		if p.DisplayName == "" || len([]rune(p.DisplayName)) > 64 {
			return fmt.Errorf("%s.displayName must contain 1 to 64 characters", prefix)
		}
		if p.ClientID == "" {
			return fmt.Errorf("%s.clientId is required", prefix)
		}

		switch p.Type {
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
			return fmt.Errorf("%s.type must be %q", prefix, ProviderTypeOIDC)
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
	// Without it the server boots clean and then fails every sign-in on the email claim.
	if _, ok := seen["email"]; !ok {
		return errors.New("must include email")
	}
	if _, ok := seen["openid"]; !ok {
		return errors.New("must include openid")
	}
	return nil
}

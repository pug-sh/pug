package cimd

import (
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxClientNameRunes = 200

// Document is the part of a client's metadata document pug reads.
type Document struct {
	ClientID                          string   `json:"client_id"`
	ClientName                        string   `json:"client_name"`
	RedirectURIs                      []string `json:"redirect_uris"`
	TokenEndpointAuthMethod           string   `json:"token_endpoint_auth_method"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	GrantTypes                        []string `json:"grant_types"`
	ResponseTypes                     []string `json:"response_types"`
}

// Lists reports whether the document lists redirect. On a loopback host the port is
// ignored, because a native client such as Claude Code picks a new one each session.
func (d Document) Lists(redirect string) bool {
	if slices.Contains(d.RedirectURIs, redirect) {
		return true
	}
	r, ok := parseRedirect(redirect)
	if !ok || !isLoopback(r) {
		return false
	}
	return slices.ContainsFunc(d.RedirectURIs, func(listed string) bool {
		l, ok := parseRedirect(listed)
		return ok && withoutPort(l) == withoutPort(r)
	})
}

func parse(body []byte, clientID, host string) (Document, error) {
	var d Document
	if err := json.Unmarshal(body, &d); err != nil {
		return Document{}, err
	}
	// Grok lists a console.x.ai redirect beside its grok.com one, so a redirect pug
	// won't send codes to is dropped rather than refusing the client.
	d.RedirectURIs = slices.DeleteFunc(d.RedirectURIs, func(r string) bool { return !redirectAllowed(r, host) })
	if err := d.check(clientID); err != nil {
		return Document{}, err
	}
	return d, nil
}

func (d Document) check(clientID string) error {
	switch {
	case d.ClientID != clientID:
		return errors.New("client_id differs from the document's URL")
	// Control characters include NUL, which Postgres can't store.
	case d.ClientName == "", utf8.RuneCountInString(d.ClientName) > maxClientNameRunes,
		strings.ContainsFunc(d.ClientName, unicode.IsControl):
		return errors.New("client_name is missing, too long or has control characters")
	case len(d.RedirectURIs) == 0:
		return errors.New("no redirect_uris that are https on the client's host or on a loopback host")
	case !d.canUseNone():
		return errors.New("the client can't authenticate with none")
	case !absentOrHas(d.GrantTypes, "authorization_code"):
		return errors.New("grant_types lacks authorization_code")
	case !absentOrHas(d.ResponseTypes, "code"):
		return errors.New("response_types lacks code")
	}
	return nil
}

// canUseNone: ChatGPT names private_key_jwt as its method but lists none as supported.
func (d Document) canUseNone() bool {
	return d.TokenEndpointAuthMethod == "" || d.TokenEndpointAuthMethod == "none" ||
		slices.Contains(d.TokenEndpointAuthMethodsSupported, "none")
}

// absentOrHas: an absent list takes RFC 7591's default, which includes v.
func absentOrHas(list []string, v string) bool {
	return list == nil || slices.Contains(list, v)
}

// redirectAllowed is https on the client's own host, so a document can't send
// codes to another host, or http or https on a loopback host.
func redirectAllowed(redirect, host string) bool {
	u, ok := parseRedirect(redirect)
	switch {
	case !ok:
		return false
	case isLoopback(u):
		return u.Scheme == "http" || u.Scheme == "https"
	default:
		return u.Scheme == "https" && strings.EqualFold(u.Hostname(), host)
	}
}

func parseRedirect(redirect string) (*url.URL, bool) {
	u, ok := parseExact(redirect)
	return u, ok && u.User == nil && u.Fragment == ""
}

// IsLoopback reports whether redirect is on a loopback host, so the client runs on
// the person's computer, where any program can claim the port.
func IsLoopback(redirect string) bool {
	u, ok := parseRedirect(redirect)
	return ok && isLoopback(u)
}

func isLoopback(u *url.URL) bool {
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

func withoutPort(u *url.URL) string {
	c := *u
	c.Host = strings.TrimSuffix(c.Host, ":"+c.Port())
	return c.String()
}

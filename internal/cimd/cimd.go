// Package cimd fetches OAuth client ID metadata documents: a client whose
// client_id is an https URL publishes its metadata there. Fetch returns a
// document only for a client pug can serve.
package cimd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/pug-sh/pug/internal/httpx"
	"github.com/pug-sh/pug/internal/slogx"
)

const (
	maxDocumentBytes = 5 << 10
	fetchTimeout     = 10 * time.Second
)

// Every failed fetch is ErrUnavailable: a refused address, no answer, a status
// other than 200, or a body over 5 KiB.
var (
	ErrClientID    = errors.New("cimd: invalid client_id")
	ErrUnavailable = errors.New("cimd: fetch failed")
	ErrDocument    = errors.New("cimd: document refused")
)

type Client struct {
	http *http.Client
}

// New fetches over httpx.NewGuardedClient, which refuses private, loopback and
// metadata addresses and ignores proxies.
func New() *Client {
	return NewClient(httpx.NewGuardedClient(fetchTimeout, false))
}

// NewClient fetches over c, such as a test server's client, where New would refuse
// the address. It never follows a redirect: the 3xx comes back and fails as a non-200.
func NewClient(c *http.Client) *Client {
	out := *c
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &out}
}

// Fetch returns the document clientID names, if pug can serve the client with it.
// Nothing is cached.
func (c *Client) Fetch(ctx context.Context, clientID string) (Document, error) {
	id, ok := parseClientID(clientID)
	if !ok {
		return Document{}, ErrClientID
	}
	body, err := c.get(ctx, clientID)
	if err != nil {
		slog.WarnContext(ctx, "client metadata fetch failed", slogx.Error(err), slog.String("client_id", clientID))
		return Document{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	doc, err := parse(body, clientID, id.Hostname())
	if err != nil {
		slog.WarnContext(ctx, "client metadata refused", slogx.Error(err), slog.String("client_id", clientID))
		return Document{}, fmt.Errorf("%w: %w", ErrDocument, err)
	}
	return doc, nil
}

func (c *Client) get(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Cloudflare sets cf-mitigated when its bot protection blocked the request.
		return nil, fmt.Errorf("status %d, cf-mitigated %q", resp.StatusCode, resp.Header.Get("Cf-Mitigated"))
	}
	return io.ReadAll(http.MaxBytesReader(nil, resp.Body, maxDocumentBytes))
}

// parseClientID allows an https URL with a host and a path, and no userinfo, query,
// fragment or dot segment, spelled so that the id compared is the URL fetched.
func parseClientID(clientID string) (*url.URL, bool) {
	u, ok := parseExact(clientID)
	return u, ok && u.Scheme == "https" && u.Hostname() != "" && u.User == nil &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" &&
		u.Path != "" && u.Path != "/" && !hasDotSegment(u.Path)
}

// parseExact refuses a spelling that Go writes back differently, such as an upper-case
// scheme, a bare '#', a space or raw non-ASCII, so the string checked is the URL used.
// It also refuses a percent-encoded non-ASCII host, which would show as a lookalike.
func parseExact(s string) (*url.URL, bool) {
	u, err := url.Parse(s)
	return u, err == nil && u.String() == s &&
		!strings.ContainsFunc(u.Host, func(r rune) bool { return r > unicode.MaxASCII })
}

func hasDotSegment(path string) bool {
	return slices.ContainsFunc(strings.Split(path, "/"), func(s string) bool { return s == "." || s == ".." })
}

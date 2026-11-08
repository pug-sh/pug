package cimd

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code.dny.dev/ssrf"
)

const testID = "https://example.com/client.json"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// serve answers each URL from respond, which returns a status, headers and a body.
func serve(respond func(url string) (int, http.Header, string)) *Client {
	return NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, header, body := respond(r.URL.String())
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
}

func ok(body string) func(string) (int, http.Header, string) {
	return func(string) (int, http.Header, string) { return http.StatusOK, http.Header{}, body }
}

func TestFetch(t *testing.T) {
	valid := `{"client_id":"` + testID + `","client_name":"Example","redirect_uris":["https://example.com/callback"]}`
	// Padded with spaces, which JSON allows, to an exact size.
	sized := func(n int) string { return valid + strings.Repeat(" ", n-len(valid)) }
	for _, tc := range []struct {
		name    string
		respond func(string) (int, http.Header, string)
		want    error
	}{
		{name: "a document", respond: ok(valid)},
		{name: "a document of exactly 5 KiB", respond: ok(sized(maxDocumentBytes))},
		{name: "a document over 5 KiB", respond: ok(sized(maxDocumentBytes + 1)), want: ErrUnavailable},
		{
			name: "a document behind a redirect",
			respond: func(u string) (int, http.Header, string) {
				if u == testID {
					return http.StatusFound, http.Header{"Location": {"https://example.com/moved.json"}}, ""
				}
				return http.StatusOK, http.Header{}, valid
			},
			want: ErrUnavailable,
		},
		{
			name: "a status other than 200",
			respond: func(string) (int, http.Header, string) {
				return http.StatusForbidden, http.Header{"Cf-Mitigated": {"challenge"}}, valid
			},
			want: ErrUnavailable,
		},
		{name: "not JSON", respond: ok("<html></html>"), want: ErrDocument},
		{
			name:    "a document naming another client",
			respond: ok(`{"client_id":"https://example.com/other.json","client_name":"Example","redirect_uris":["https://example.com/callback"]}`),
			want:    ErrDocument,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := serve(tc.respond).Fetch(t.Context(), testID)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if err == nil && doc.ClientName != "Example" {
				t.Fatalf("client_name = %q, want Example", doc.ClientName)
			}
		})
	}
}

func TestClientIDRules(t *testing.T) {
	c := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("fetched %s, which the client_id rules should refuse first", r.URL)
		return nil, errors.New("unreachable")
	})})
	for _, id := range []string{
		"",
		"client",
		"http://example.com/client.json",
		"HTTPS://example.com/client.json",
		"https://example.com",
		"https://example.com/",
		"https://user:pass@example.com/client.json",
		"https://example.com/a/../client.json",
		"https://example.com/a/./client.json",
		"https://example.com/a/%2e%2e/client.json",
		"https://example.com/client.json?v=1",
		"https://example.com/client.json?",
		"https://example.com/client.json#top",
		"https://example.com/client\x00.json",
		"https://example.com/client\xff.json",
		"https://example.com/clienté.json",
		"https://example.com/client .json",
		"https://:443/client.json",
		"https://%D0%B0%D1%80%D1%80%D3%8F%D0%B5.com/client.json",
	} {
		if _, err := c.Fetch(t.Context(), id); !errors.Is(err, ErrClientID) {
			t.Errorf("client_id %q: err = %v, want ErrClientID", id, err)
		}
	}
}

func TestFetchRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the guarded client reached a loopback server")
	}))
	defer srv.Close()

	_, err := New().Fetch(t.Context(), srv.URL+"/client.json")
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, ssrf.ErrProhibitedIP) {
		t.Fatalf("err = %v, want ErrUnavailable from ssrf.ErrProhibitedIP", err)
	}
}

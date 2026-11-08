package httpx

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGuard(t *testing.T) {
	for _, tc := range []struct {
		network, addr string
		allowed       bool
	}{
		{"tcp4", "93.184.215.14:443", true},
		{"tcp4", "93.184.215.14:8443", true},
		{"tcp4", "127.0.0.1:443", false},
		{"tcp4", "10.1.2.3:443", false},
		{"tcp4", "192.168.1.1:443", false},
		{"tcp4", "169.254.169.254:80", false},
		{"tcp6", "[::1]:443", false},
		{"tcp6", "[fd00::1]:443", false},
	} {
		err := guard.Safe(tc.network, tc.addr, nil)
		if (err == nil) != tc.allowed {
			t.Errorf("Safe(%s) = %v, want allowed = %v", tc.addr, err, tc.allowed)
		}
	}
}

// Behind a proxy the dial guard would check the proxy's address, not the server's.
func TestGuardedTransportSkipsProxies(t *testing.T) {
	if guardedTransport(time.Second, false).Proxy != nil {
		t.Fatal("the guarded transport must not use a proxy")
	}
	if guardedTransport(time.Second, true).Proxy == nil {
		t.Fatal("with private addresses allowed, the transport keeps the environment's proxy")
	}
}

func TestGuardedClientCapsResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/headers" {
			w.Header().Set("X-Big", strings.Repeat("x", maxGuardedHeaderBytes))
		}
		_, _ = w.Write(make([]byte, maxGuardedBodyBytes+1))
	}))
	defer srv.Close()
	client := NewGuardedClient(10*time.Second, true)

	resp, err := client.Get(srv.URL + "/body")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if _, ok := errors.AsType[*http.MaxBytesError](err); !ok {
		t.Fatalf("body: err = %v, want *http.MaxBytesError", err)
	}
	if resp, err := client.Get(srv.URL + "/headers"); err == nil {
		resp.Body.Close()
		t.Fatal("headers over the cap: want an error")
	}
}

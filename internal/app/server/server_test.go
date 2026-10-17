package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestServeReturnsServeError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()

	err = serve(t.Context(), new(http.Server), ln, func() { t.Error("drain ran without a shutdown") }, 0)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve = %v, want the listener's error", err)
	}
}

// TestServeDrains pins the shutdown order: drain runs first, requests are still
// served during the drain delay, and serve waits for an in-flight request (up to
// its 10s limit), since its caller closes NATS and the databases next.
func TestServeDrains(t *testing.T) {
	const drainDelay = 500 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	releaseSlow := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseSlow)
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	mux.HandleFunc("/fast", func(http.ResponseWriter, *http.Request) {})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	draining := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}, ln, func() { close(draining) }, drainDelay)
	}()

	client := &http.Client{Transport: &http.Transport{}}
	get := func(path string) error {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+path, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	slow := make(chan error, 1)
	go func() { slow <- get("/slow") }()
	recv(t, started, "the slow request to start")
	cancel()
	recv(t, draining, "serve to drain")
	if err := get("/fast"); err != nil {
		t.Fatalf("request during the drain delay failed: %v", err)
	}

	select {
	case err := <-served:
		t.Fatalf("serve returned (%v) with a request still in flight", err)
	case <-time.After(drainDelay + 200*time.Millisecond):
	}

	releaseSlow()
	if err := recv(t, slow, "the slow response"); err != nil {
		t.Fatalf("in-flight request failed: %v", err)
	}
	if err := recv(t, served, "serve to return"); err != nil {
		t.Fatalf("serve = %v, want nil", err)
	}
}

// recv fails the test after 5s instead of hanging until the package timeout.
func recv[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

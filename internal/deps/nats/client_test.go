package nats

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestIsClosed pins that a reconnecting connection is not closed, and that IsClosed
// answers while a reconnect attempt holds nats.go's connection lock.
func TestIsClosed(t *testing.T) {
	if !(&NATSClient{}).IsClosed() {
		t.Error("a client with no connection reported open")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The first connection is dropped so Connect fails fast and starts reconnecting; the
	// next one never speaks, so that attempt holds the lock until its handshake times out.
	stalled := make(chan net.Conn, 4)
	go func() {
		for first := true; ; first = false {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if first {
				_ = c.Close()
				continue
			}
			stalled <- c
		}
	}()

	conn, err := nats.Connect("nats://"+ln.Addr().String(), nats.RetryOnFailedConnect(true), nats.ReconnectWait(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)

	var c net.Conn
	select {
	case c = <-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a reconnect attempt")
	}
	// Runs before conn.Close, which would otherwise wait out the stalled handshake.
	t.Cleanup(func() {
		_ = ln.Close()
		_ = c.Close()
	})

	client := &NATSClient{conn: conn}
	closed := make(chan bool, 1)
	go func() { closed <- client.IsClosed() }()
	select {
	case got := <-closed:
		if got {
			t.Error("a reconnecting connection reported closed")
		}
	case <-time.After(250 * time.Millisecond):
		t.Error("IsClosed waited on the lock a reconnect attempt holds")
	}
}

// TestNewMarksClosed pins that a connection from New reports closed once nats.go
// closes it for good, since /healthz reads only that flag.
func TestNewMarksClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("INFO {\"server_id\":\"fake\",\"max_payload\":1048576}\r\n"))
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "PING") {
				_, _ = c.Write([]byte("PONG\r\n"))
			}
		}
	}()

	t.Setenv("NATS_URL", "nats://"+ln.Addr().String())
	client, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if client.IsClosed() {
		t.Fatal("a connected client reported closed")
	}

	client.conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !client.IsClosed() {
		if time.Now().After(deadline) {
			t.Fatal("a closed connection never reported closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

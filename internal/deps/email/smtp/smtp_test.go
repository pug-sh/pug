package smtp_test

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	emailsmtp "github.com/pug-sh/pug/internal/deps/email/smtp"
)

// fakeSMTPServer answers a minimal SMTP conversation and captures the bytes
// after DATA. Does NOT implement STARTTLS — Provider tests pass UseTLS:false.
type fakeSMTPServer struct {
	listener net.Listener
	mu       sync.Mutex
	body     bytes.Buffer
	wg       sync.WaitGroup
}

func newFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeSMTPServer{listener: ln}
	srv.wg.Add(1)
	go srv.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		srv.wg.Wait()
	})
	return srv
}

func (s *fakeSMTPServer) serve() {
	defer s.wg.Done()
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	buf := make([]byte, 4096)
	send := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	send("220 fake.localhost ESMTP")
	dataMode := false
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		chunk := string(buf[:n])
		if dataMode {
			s.mu.Lock()
			s.body.WriteString(chunk)
			s.mu.Unlock()
			if strings.Contains(chunk, "\r\n.\r\n") {
				send("250 OK queued")
				dataMode = false
			}
			continue
		}
		switch {
		case strings.HasPrefix(chunk, "EHLO"), strings.HasPrefix(chunk, "HELO"):
			send("250-fake.localhost")
			send("250-AUTH PLAIN LOGIN")
			send("250 OK")
		case strings.HasPrefix(chunk, "AUTH"):
			send("235 OK authenticated")
		case strings.HasPrefix(chunk, "MAIL FROM"), strings.HasPrefix(chunk, "RCPT TO"):
			send("250 OK")
		case strings.HasPrefix(chunk, "DATA"):
			send("354 send body")
			dataMode = true
		case strings.HasPrefix(chunk, "QUIT"):
			send("221 bye")
			return
		default:
			send("250 OK")
		}
	}
}

func (s *fakeSMTPServer) addr() string { return s.listener.Addr().String() }
func (s *fakeSMTPServer) bodyString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String()
}

func TestSMTPProviderSend(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := splitHostPort(t, srv.addr())

	prov, err := emailsmtp.New(emailsmtp.Config{
		Host:     host,
		Port:     port,
		Username: "user",
		Password: "pass",
		UseTLS:   false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = prov.Send(context.Background(), coreemail.Message{
		From:     "from@example.com",
		To:       "to@example.com",
		Subject:  "hello",
		HTMLBody: "<p>hi</p>",
		TextBody: "hi",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(srv.bodyString(), "Subject: hello") {
		t.Fatalf("expected subject in body, got %s", srv.bodyString())
	}
	if !strings.Contains(srv.bodyString(), "<p>hi</p>") {
		t.Fatalf("expected html body, got %s", srv.bodyString())
	}
}

func TestSMTPProviderConnectError(t *testing.T) {
	prov, err := emailsmtp.New(emailsmtp.Config{
		Host:     "127.0.0.1",
		Port:     1, // closed port
		Username: "u", Password: "p", UseTLS: false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = prov.Send(context.Background(), coreemail.Message{
		From: "from@example.com", To: "to@example.com",
		Subject: "x", TextBody: "y", HTMLBody: "<p>y</p>",
	})
	if err == nil {
		t.Fatal("expected connect error")
	}
	if coreemail.IsPermanentError(err) {
		t.Fatalf("connect error should be transient, got permanent: %v", err)
	}
}

// TestSMTPProviderSanitizesHeaders pins the header-injection defense: a
// message Subject containing CRLF must not produce two header lines. This is
// the regression test for the org_display_name → Subject injection vector.
func TestSMTPProviderSanitizesHeaders(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := splitHostPort(t, srv.addr())

	prov, err := emailsmtp.New(emailsmtp.Config{
		Host: host, Port: port, Username: "user", Password: "pass", UseTLS: false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = prov.Send(context.Background(), coreemail.Message{
		From:    "from@example.com",
		To:      "to@example.com",
		Subject: "Hello\r\nBcc: leak@evil.example",
		TextBody: "x", HTMLBody: "<p>x</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	body := srv.bodyString()
	// The dangerous outcome is a standalone Bcc: header line. The CRLF must
	// have been stripped so the malicious payload is collapsed into the
	// Subject value (where it's harmless content, not a header).
	for _, line := range strings.Split(body, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("Bcc header injected on its own line: %q", line)
		}
	}
	if !strings.Contains(body, "Subject: HelloBcc: leak@evil.example") {
		t.Fatalf("expected sanitized subject on one line, got: %s", body)
	}
}

func TestSMTPProviderUseTLSWithoutSTARTTLSFailsPermanent(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := splitHostPort(t, srv.addr())

	prov, err := emailsmtp.New(emailsmtp.Config{
		Host: host, Port: port, Username: "u", Password: "p",
		UseTLS: true, // server fake does NOT advertise STARTTLS
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = prov.Send(context.Background(), coreemail.Message{
		From: "f@example.com", To: "t@example.com",
		Subject: "x", TextBody: "y", HTMLBody: "<p>y</p>",
	})
	if err == nil {
		t.Fatal("expected error when UseTLS=true and server doesn't advertise STARTTLS")
	}
	if !coreemail.IsPermanentError(err) {
		t.Fatalf("expected permanent error, got %v", err)
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %s: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %s: %v", portStr, err)
	}
	return host, port
}

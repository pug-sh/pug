package smtp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	netsmtp "net/smtp"
	"net/textproto"
	"strconv"
	"strings"

	emailspec "github.com/pug-sh/pug/internal/core/email/spec"
	"github.com/pug-sh/pug/internal/slogx"
)

type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	UseTLS   bool
}

type Provider struct {
	cfg Config
}

func New(cfg Config) (*Provider, error) {
	if cfg.Host == "" || cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, errors.New("smtp: host required and port must be in 1..65535")
	}
	// Probe crypto/rand once at construction so a broken entropy source
	// surfaces here rather than panicking inside the first Send (which would
	// take down a worker goroutine).
	if _, err := rand.Read(make([]byte, 1)); err != nil {
		return nil, fmt.Errorf("smtp: crypto/rand unavailable: %w", err)
	}
	return &Provider{cfg: cfg}, nil
}

func (p *Provider) Send(ctx context.Context, msg emailspec.Message) error {
	addr := net.JoinHostPort(p.cfg.Host, strconv.Itoa(p.cfg.Port))

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}

	// Start the ctx-cancel watchdog BEFORE NewClient so a server that
	// accepts the TCP connection but never sends the 220 greeting can't
	// pin a worker goroutine. netsmtp.NewClient blocks reading the
	// greeting; closing the underlying conn here unblocks that read.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	c, err := netsmtp.NewClient(conn, p.cfg.Host)
	if err != nil {
		if cerr := conn.Close(); cerr != nil {
			slog.WarnContext(ctx, "smtp: conn close after NewClient failure errored", slogx.Error(cerr))
		}
		return fmt.Errorf("smtp new client: %w", err)
	}
	defer func() {
		if cerr := c.Close(); cerr != nil {
			slog.WarnContext(ctx, "smtp: client close errored", slogx.Error(cerr))
		}
	}()

	if err := c.Hello("localhost"); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}

	if p.cfg.UseTLS {
		ok, _ := c.Extension("STARTTLS")
		if !ok {
			return emailspec.NewPermanentError(errors.New("smtp: server does not advertise STARTTLS but UseTLS was requested"))
		}
		if err := c.StartTLS(&tls.Config{ServerName: p.cfg.Host}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}

	if p.cfg.Username != "" {
		auth := netsmtp.PlainAuth("", p.cfg.Username, p.cfg.Password, p.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return classify(fmt.Errorf("smtp auth: %w", err))
		}
	}

	if err := c.Mail(msg.From); err != nil {
		return classify(fmt.Errorf("smtp mail from: %w", err))
	}
	if err := c.Rcpt(msg.To); err != nil {
		return classify(fmt.Errorf("smtp rcpt to: %w", err))
	}

	w, err := c.Data()
	if err != nil {
		return classify(fmt.Errorf("smtp data: %w", err))
	}
	if _, err := w.Write([]byte(buildMIME(msg))); err != nil {
		return fmt.Errorf("smtp write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return classify(fmt.Errorf("smtp close data: %w", err))
	}

	// After w.Close() succeeded the server has queued the message (250 OK).
	// A Quit() error here is a connection teardown issue — the email IS sent.
	// Returning it would cause NATS to retry and the recipient to get a duplicate.
	// Log at Warn so a teardown spike is at least visible to ops.
	if qerr := c.Quit(); qerr != nil {
		slog.WarnContext(ctx, "smtp: Quit after successful DATA errored (message was queued)", slogx.Error(qerr))
	}
	return nil
}

// randomBoundary returns a random multipart MIME boundary. Using a random
// boundary per message prevents collisions with literal boundary strings that
// might appear in user-supplied body content. crypto/rand failure means the
// system entropy source is broken; panicking surfaces the problem rather
// than emailing with a predictable boundary. New() probes crypto/rand at
// construction so this panic is normally unreachable in production.
func randomBoundary() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("smtp: crypto/rand failed: %v", err))
	}
	return "pug-" + hex.EncodeToString(buf[:])
}

// sanitizeHeader strips CR and LF from a header value. SMTP headers terminate
// on \r\n, so any CR/LF in user-supplied content (e.g. an org display_name
// reaching Subject through an invite email) would split the field and let an
// attacker inject arbitrary headers like Bcc.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

func buildMIME(msg emailspec.Message) string {
	var sb strings.Builder
	sb.WriteString("From: " + sanitizeHeader(msg.From) + "\r\n")
	sb.WriteString("To: " + sanitizeHeader(msg.To) + "\r\n")
	if msg.ReplyTo != "" {
		sb.WriteString("Reply-To: " + sanitizeHeader(msg.ReplyTo) + "\r\n")
	}
	sb.WriteString("Subject: " + sanitizeHeader(msg.Subject) + "\r\n")
	if msg.IdempotencyKey != "" {
		sb.WriteString("X-Idempotency-Key: " + sanitizeHeader(msg.IdempotencyKey) + "\r\n")
	}
	sb.WriteString("MIME-Version: 1.0\r\n")
	boundary := randomBoundary()
	sb.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")

	sb.WriteString("--" + boundary + "\r\n")
	sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	sb.WriteString(msg.TextBody + "\r\n\r\n")

	sb.WriteString("--" + boundary + "\r\n")
	sb.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	sb.WriteString(msg.HTMLBody + "\r\n\r\n")

	sb.WriteString("--" + boundary + "--\r\n")
	return sb.String()
}

// classify turns 5xx SMTP replies (permanent failures) into permanent errors.
// Anything else (4xx, connection issues) stays transient so NATS retries.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var smtpErr *textproto.Error
	if errors.As(err, &smtpErr) && smtpErr.Code >= 500 && smtpErr.Code < 600 {
		return emailspec.NewPermanentError(err)
	}
	return err
}

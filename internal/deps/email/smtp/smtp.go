package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	netsmtp "net/smtp"
	"net/textproto"
	"strconv"
	"strings"

	coreemail "github.com/pug-sh/pug/internal/core/email"
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
	if cfg.Host == "" || cfg.Port == 0 {
		return nil, errors.New("smtp: host and port are required")
	}
	return &Provider{cfg: cfg}, nil
}

func (p *Provider) Send(_ context.Context, msg coreemail.Message) error {
	addr := net.JoinHostPort(p.cfg.Host, strconv.Itoa(p.cfg.Port))

	c, err := netsmtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	defer c.Close()

	if err := c.Hello("localhost"); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}

	if p.cfg.UseTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: p.cfg.Host}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
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

	if err := c.Quit(); err != nil {
		return fmt.Errorf("smtp quit: %w", err)
	}
	return nil
}

// buildMIME returns a multipart/alternative message with text and html parts.
func buildMIME(msg coreemail.Message) string {
	var sb strings.Builder
	sb.WriteString("From: " + msg.From + "\r\n")
	sb.WriteString("To: " + msg.To + "\r\n")
	if msg.ReplyTo != "" {
		sb.WriteString("Reply-To: " + msg.ReplyTo + "\r\n")
	}
	sb.WriteString("Subject: " + msg.Subject + "\r\n")
	if msg.IdempotencyKey != "" {
		sb.WriteString("X-Idempotency-Key: " + msg.IdempotencyKey + "\r\n")
	}
	sb.WriteString("MIME-Version: 1.0\r\n")
	boundary := "pug-mime-boundary"
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
		return coreemail.NewPermanentError(err)
	}
	return err
}

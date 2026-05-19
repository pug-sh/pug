// Package spec holds the leaf types shared between core/email and the
// deps/email/* provider packages (resend, smtp, ses). Living in a separate
// package lets the deps providers describe their Send signature without
// importing core/email — that import direction would be a cycle once
// core/email needs to construct providers from per-tenant config
// (see internal/core/email/tenant_resolver.go).
package spec

import (
	"context"
	"errors"
)

type Message struct {
	IdempotencyKey string
	From           string
	ReplyTo        string
	Subject        string
	To             string
	HTMLBody       string
	TextBody       string
}

type Provider interface {
	Send(ctx context.Context, msg Message) error
}

// PermanentError wraps an error to signal the failure must not be retried.
type PermanentError struct{ err error }

func NewPermanentError(err error) *PermanentError {
	if err == nil {
		panic("email: nil permanent error")
	}
	return &PermanentError{err: err}
}

func (e *PermanentError) Error() string { return e.err.Error() }
func (e *PermanentError) Unwrap() error { return e.err }

func IsPermanentError(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}

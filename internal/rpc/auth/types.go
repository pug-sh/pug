package auth

import "errors"

var (
	ErrMissingEmail    = errors.New("email is required")
	ErrMissingPassword = errors.New("password is required")
)

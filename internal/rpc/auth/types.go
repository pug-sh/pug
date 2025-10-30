package auth

import "errors"

var (
	ErrUserAlreadyExists  = errors.New("user with this email already exists")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrCustomerCreation   = errors.New("failed to create customer")
	ErrMissingEmail       = errors.New("email is required")
	ErrMissingPassword    = errors.New("password is required")
)

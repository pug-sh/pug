package apperr

import "connectrpc.com/connect"

func NotFound(reason Reason, msg string, opts ...Option) error {
	return Err(connect.CodeNotFound, reason, msg, opts...)
}
func Invalid(reason Reason, msg string, opts ...Option) error {
	return Err(connect.CodeInvalidArgument, reason, msg, opts...)
}
func AlreadyExists(reason Reason, msg string, opts ...Option) error {
	return Err(connect.CodeAlreadyExists, reason, msg, opts...)
}
func PermissionDenied(reason Reason, msg string, opts ...Option) error {
	return Err(connect.CodePermissionDenied, reason, msg, opts...)
}
func FailedPrecondition(reason Reason, msg string, opts ...Option) error {
	return Err(connect.CodeFailedPrecondition, reason, msg, opts...)
}
func Unauthenticated(reason Reason, msg string, opts ...Option) error {
	return Err(connect.CodeUnauthenticated, reason, msg, opts...)
}

package auth

import (
	"context"

	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
)

// PublishEmailJobForTest exposes the unexported publishEmailJob to the
// auth_test package so the "unknown payload kind" counter branch can be
// driven directly. Without this, the unknown branch is unreachable via the
// public API since all callers construct typed jobs.
func (s *Service) PublishEmailJobForTest(ctx context.Context, job *emailworkerv1.EmailJob) {
	s.publishEmailJob(ctx, job)
}

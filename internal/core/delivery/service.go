package delivery

import (
	"context"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

// Service defines the interface for sending notifications through different channels
type Service interface {
	SendNotification(ctx context.Context, campaign dbread.Campaign, device dbread.ProfileDevice) error
}

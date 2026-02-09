package delivery

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Router struct {
	pgRO        *pgxpool.Pool
	pgW         *pgxpool.Pool
	projectsSvc *projects.Service
	fcmService  *FCMService
	// apnService   *APNService  // Placeholder for future APN service
	// emailService *EmailService // Placeholder for future email service
}

func NewRouter(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, projectsSvc *projects.Service) *Router {
	return &Router{
		pgRO:        pgRO,
		pgW:         pgW,
		projectsSvc: projectsSvc,
		fcmService:  NewFCMService(pgRO, pgW, projectsSvc),
		// apnService:   NewAPNService(pgRO, pgW, projectsSvc),  // To be implemented later
		// emailService: NewEmailService(pgRO, pgW, projectsSvc), // To be implemented later
	}
}

// SendNotification routes the notification to the appropriate delivery service based on platform
func (r *Router) SendNotification(ctx context.Context, campaign dbread.Campaign, subscription dbread.Subscription) error {
	switch subscription.Platform {
	case "fcm", "android", "firebase":
		return r.fcmService.SendNotification(ctx, campaign, subscription)
	case "apn", "ios", "apple":
		// TODO: Implement APN delivery
		slog.WarnContext(ctx,"APN delivery not implemented yet",
			slog.String("subscription_id", subscription.ID),
			slog.String("platform", subscription.Platform))
		return fmt.Errorf("APN delivery not implemented yet")
	case "email":
		// TODO: Implement email delivery
		slog.WarnContext(ctx,"Email delivery not implemented yet",
			slog.String("subscription_id", subscription.ID),
			slog.String("platform", subscription.Platform))
		return fmt.Errorf("email delivery not implemented yet")
	default:
		slog.WarnContext(ctx,"Unknown platform for delivery",
			slog.String("subscription_id", subscription.ID),
			slog.String("platform", subscription.Platform))
		return fmt.Errorf("unknown platform: %s", subscription.Platform)
	}
}

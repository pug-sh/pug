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
func (r *Router) SendNotification(ctx context.Context, campaign dbread.Campaign, device dbread.ProfileDevice) error {
	switch device.Platform {
	case "android":
		return r.fcmService.SendNotification(ctx, campaign, device)
	case "ios":
		// TODO: Implement APN delivery
		slog.WarnContext(ctx, "APN delivery not implemented yet",
			slog.String("device_id", device.ID),
			slog.String("platform", device.Platform))
		return fmt.Errorf("APN delivery not implemented yet")
	case "web":
		// TODO: Implement web push delivery
		slog.WarnContext(ctx, "web push delivery not implemented yet",
			slog.String("device_id", device.ID),
			slog.String("platform", device.Platform))
		return fmt.Errorf("web push delivery not implemented yet")
	default:
		slog.WarnContext(ctx, "Unknown platform for delivery",
			slog.String("device_id", device.ID),
			slog.String("platform", device.Platform))
		return fmt.Errorf("unknown platform: %s", device.Platform)
	}
}

package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
	"google.golang.org/api/option"
)

type FCMService struct {
	pgRO        *pgxpool.Pool
	pgW         *pgxpool.Pool
	projectsSvc *projects.Service
	mu          sync.RWMutex
	clients     map[string]*messaging.Client
	sf          singleflight.Group
}

func NewFCMService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, projectsSvc *projects.Service) *FCMService {
	return &FCMService{
		pgRO:        pgRO,
		pgW:         pgW,
		projectsSvc: projectsSvc,
		clients:     make(map[string]*messaging.Client),
	}
}

func (f *FCMService) getMessagingClient(ctx context.Context, projectID, fcmServiceJSON string) (*messaging.Client, error) {
	f.mu.RLock()
	if client, ok := f.clients[projectID]; ok {
		f.mu.RUnlock()
		return client, nil
	}
	f.mu.RUnlock()

	// singleflight deduplicates concurrent initialization for the same project ID
	// so the lock is only held briefly for map access, not during network I/O.
	v, err, _ := f.sf.Do(projectID, func() (any, error) {
		// Double-check after winning the singleflight race.
		f.mu.RLock()
		if client, ok := f.clients[projectID]; ok {
			f.mu.RUnlock()
			return client, nil
		}
		f.mu.RUnlock()

		opt := option.WithCredentialsJSON([]byte(fcmServiceJSON))
		app, err := firebase.NewApp(ctx, nil, opt)
		if err != nil {
			return nil, fmt.Errorf("error initializing Firebase app: %w", err)
		}

		client, err := app.Messaging(ctx)
		if err != nil {
			return nil, fmt.Errorf("error getting Firebase Messaging client: %w", err)
		}

		f.mu.Lock()
		f.clients[projectID] = client
		f.mu.Unlock()

		slog.InfoContext(ctx, "Firebase messaging client cached", slog.String("project_id", projectID))
		return client, nil
	})
	if err != nil {
		return nil, err
	}

	return v.(*messaging.Client), nil
}

// SendNotification sends a push notification to a device token via FCM
func (f *FCMService) SendNotification(ctx context.Context, campaign dbread.Campaign, device dbread.ProfileDevice) error {
	// Get project details to access FCM service JSON
	project, err := f.projectsSvc.GetProjectByID(ctx, campaign.ProjectID)
	if err != nil {
		return fmt.Errorf("failed to get project: %w", err)
	}

	// Check if FCM service JSON is available for the project
	if project.FcmServiceJson.String == "" {
		slog.WarnContext(ctx, "No FCM service JSON configured for project",
			slog.String("project_id", campaign.ProjectID))
		return fmt.Errorf("no FCM service configuration for project: %s", campaign.ProjectID)
	}

	// Extract title and body from notification data
	title, ok := campaign.NotificationData["title"].(string)
	if !ok {
		slog.WarnContext(ctx, "campaign missing notification title, using fallback",
			slog.String("campaign_id", campaign.ID))
		title = "Notification"
	}
	body, ok := campaign.NotificationData["body"].(string)
	if !ok {
		slog.WarnContext(ctx, "campaign missing notification body, using fallback",
			slog.String("campaign_id", campaign.ID))
		body = "You have a new notification"
	}

	// Get or create cached messaging client for this project
	client, err := f.getMessagingClient(ctx, campaign.ProjectID, project.FcmServiceJson.String)
	if err != nil {
		return err
	}

	// Create the FCM message
	fcmMsg := &messaging.Message{
		Notification: &messaging.Notification{
			Title: title,
			Body:  body,
		},
		Data: map[string]string{
			"campaign_id": campaign.ID,
			"project_id":  campaign.ProjectID,
		},
		Token: device.Token.String,
	}

	// Send the message
	response, err := client.Send(ctx, fcmMsg)
	if err != nil {
		return fmt.Errorf("error sending FCM message: %w", err)
	}

	slog.InfoContext(ctx, "FCM notification sent successfully",
		slog.String("response_id", response),
		slog.String("device_id", device.ID),
		slog.String("campaign_id", campaign.ID))

	return nil
}

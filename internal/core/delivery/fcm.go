package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/api/option"
)

type FCMService struct {
	pgRO           *pgxpool.Pool
	pgW            *pgxpool.Pool
	projectsSvc    *projects.Service
	messagingCache sync.Map
}

func NewFCMService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, projectsSvc *projects.Service) *FCMService {
	return &FCMService{
		pgRO:        pgRO,
		pgW:         pgW,
		projectsSvc: projectsSvc,
	}
}

func (f *FCMService) getMessagingClient(ctx context.Context, projectID, fcmServiceJSON string) (*messaging.Client, error) {
	if cached, ok := f.messagingCache.Load(projectID); ok {
		return cached.(*messaging.Client), nil
	}

	opt := option.WithCredentialsJSON([]byte(fcmServiceJSON))
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		return nil, fmt.Errorf("error initializing Firebase app: %w", err)
	}

	client, err := app.Messaging(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting Firebase Messaging client: %w", err)
	}

	f.messagingCache.Store(projectID, client)
	slog.Info("Firebase messaging client cached", slog.String("project_id", projectID))

	return client, nil
}

// SendNotification sends a push notification to a device token via FCM
func (f *FCMService) SendNotification(ctx context.Context, campaign dbread.Campaign, subscription dbread.Subscription) error {
	// Get project details to access FCM service JSON
	project, err := f.projectsSvc.GetProjectByID(ctx, campaign.ProjectID)
	if err != nil {
		return fmt.Errorf("failed to get project: %w", err)
	}

	// Check if FCM service JSON is available for the project
	if project.FcmServiceJson.String == "" {
		slog.Warn("No FCM service JSON configured for project",
			slog.String("project_id", campaign.ProjectID))
		return fmt.Errorf("no FCM service configuration for project: %s", campaign.ProjectID)
	}

	// Parse the notification data from the campaign
	var notificationData map[string]interface{}
	if err := json.Unmarshal(campaign.NotificationData, &notificationData); err != nil {
		return fmt.Errorf("failed to unmarshal notification data: %w", err)
	}

	// Extract title and body from notification data
	title, ok := notificationData["title"].(string)
	if !ok {
		title = "Notification"
	}
	body, ok := notificationData["body"].(string)
	if !ok {
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
		Token: subscription.Token, // The device token is stored in the Token field
	}

	// Send the message
	response, err := client.Send(ctx, fcmMsg)
	if err != nil {
		return fmt.Errorf("error sending FCM message: %w", err)
	}

	slog.Info("FCM notification sent successfully",
		slog.String("response_id", response),
		slog.String("subscription_id", subscription.ID),
		slog.String("campaign_id", campaign.ID))

	return nil
}

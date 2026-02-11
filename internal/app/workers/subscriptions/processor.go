package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/subscriptions"
	subscriptionsv1 "github.com/fivebitsio/cotton/internal/gen/proto/subscriptions/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

type Worker struct {
	subscriptionService *subscriptions.Service
	profilesRead        *dbread.Queries
	profilesWrite       *dbwrite.Queries
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	return &Worker{
		subscriptionService: subscriptions.NewService(pgRO, pgW),
		profilesRead:        dbread.New(pgRO),
		profilesWrite:       dbwrite.New(pgW),
	}
}

// protoMapToAny converts a protobuf map to map[string]any via JSON round-trip
func protoMapToAny(m any) (map[string]any, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	msg := &subscriptionsv1.SubscriptionOperationMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal subscription operation message", slogx.Error(err))
		return err
	}

	switch msg.OperationType {
	case subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPSERT:
		return c.handleUpsert(ctx, msg)
	case subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPDATE_HEARTBEAT:
		return c.handleUpdateHeartbeat(ctx, msg)
	case subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPDATE_METADATA:
		return c.handleUpdateMetadata(ctx, msg)
	case subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPDATE_STATUS:
		return c.handleUpdateStatus(ctx, msg)
	case subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPDATE_TOKEN:
		return c.handleUpdateToken(ctx, msg)
	case subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_PROFILE_LINK:
		return c.handleProfileLink(ctx, msg)
	default:
		slog.WarnContext(ctx, "unknown subscription operation type", slog.Int("type", int(msg.OperationType)))
		return fmt.Errorf("unknown operation type: %v", msg.OperationType)
	}
}

func (c *Worker) handleUpsert(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	_, err := c.subscriptionService.GetSubscription(ctx, msg.GetId(), msg.GetProjectId())
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "failed to get subscription", slogx.Error(err))
			return err
		}
		// Subscription not found, create it
		metadata, marshalErr := protoMapToAny(msg.GetMetadata())
		if marshalErr != nil {
			slog.ErrorContext(ctx, "failed to convert metadata", slogx.Error(marshalErr))
			return marshalErr
		}
		if _, createErr := c.subscriptionService.CreateSubscription(ctx, msg.GetId(), msg.GetProjectId(), msg.GetToken(), msg.GetPlatform(), metadata, msg.GetStatus(), "worker"); createErr != nil {
			slog.ErrorContext(ctx, "failed to create subscription", slogx.Error(createErr))
			return createErr
		}
		return nil
	}

	// Subscription exists, update fields that are provided
	if msg.GetToken() != "" {
		if _, err := c.subscriptionService.UpdateSubscriptionToken(ctx, msg.GetId(), msg.GetProjectId(), msg.GetToken()); err != nil {
			slog.ErrorContext(ctx, "failed to update subscription token", slogx.Error(err))
			return err
		}
	}
	if msg.GetPlatform() != "" {
		if _, err := c.subscriptionService.UpdateSubscriptionPlatform(ctx, msg.GetId(), msg.GetProjectId(), msg.GetPlatform()); err != nil {
			slog.ErrorContext(ctx, "failed to update subscription platform", slogx.Error(err))
			return err
		}
	}
	if msg.GetStatus() != "" {
		if _, err := c.subscriptionService.UpdateSubscriptionStatus(ctx, msg.GetId(), msg.GetProjectId(), msg.GetStatus()); err != nil {
			slog.ErrorContext(ctx, "failed to update subscription status", slogx.Error(err))
			return err
		}
	}
	if len(msg.GetMetadata()) > 0 {
		metadata, marshalErr := protoMapToAny(msg.GetMetadata())
		if marshalErr != nil {
			slog.ErrorContext(ctx, "failed to convert metadata", slogx.Error(marshalErr))
			return marshalErr
		}
		if _, updateErr := c.subscriptionService.UpdateSubscriptionMetadata(ctx, msg.GetId(), msg.GetProjectId(), metadata); updateErr != nil {
			slog.ErrorContext(ctx, "failed to update subscription metadata", slogx.Error(updateErr))
			return updateErr
		}
	}
	return nil
}

func (c *Worker) handleUpdateHeartbeat(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	if _, err := c.subscriptionService.UpdateSubscriptionHeartbeat(ctx, msg.GetId(), msg.GetProjectId()); err != nil {
		slog.ErrorContext(ctx, "failed to update heartbeat", slogx.Error(err))
		return err
	}
	return nil
}

func (c *Worker) handleUpdateMetadata(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	metadata, err := protoMapToAny(msg.GetMetadata())
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert metadata", slogx.Error(err))
		return err
	}
	if _, err = c.subscriptionService.UpdateSubscriptionMetadata(ctx, msg.GetId(), msg.GetProjectId(), metadata); err != nil {
		slog.ErrorContext(ctx, "failed to update metadata", slogx.Error(err))
		return err
	}
	return nil
}

func (c *Worker) handleUpdateStatus(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	if _, err := c.subscriptionService.UpdateSubscriptionStatus(ctx, msg.GetId(), msg.GetProjectId(), msg.GetStatus()); err != nil {
		slog.ErrorContext(ctx, "failed to update status", slogx.Error(err))
		return err
	}
	return nil
}

func (c *Worker) handleUpdateToken(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	if _, err := c.subscriptionService.UpdateSubscriptionToken(ctx, msg.GetId(), msg.GetProjectId(), msg.GetToken()); err != nil {
		slog.ErrorContext(ctx, "failed to update token", slogx.Error(err))
		return err
	}
	return nil
}

func (c *Worker) handleProfileLink(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	projectID := msg.GetProjectId()
	externalID := msg.GetExternalId()
	subscriptionID := msg.GetSubscriptionId()

	profileMetadata, err := protoMapToAny(msg.GetProfileMetadata())
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert profile metadata", slogx.Error(err))
		return err
	}

	profile, err := c.profilesWrite.SaveProfile(ctx, dbwrite.SaveProfileParams{
		AutoProperties:   profileMetadata,
		CustomProperties: map[string]any{},
		ExternalID:       externalID,
		ID:               xid.New().String(),
		ProjectID:        projectID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to save profile", slogx.Error(err))
		return err
	}

	if _, err := c.subscriptionService.LinkSubscriptionToProfile(ctx, subscriptionID, projectID, profile.ID); err != nil {
		slog.ErrorContext(ctx, "failed to link subscription to profile", slogx.Error(err))
		return err
	}
	return nil
}

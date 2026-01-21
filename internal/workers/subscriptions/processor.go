package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/subscriptions"
	subscriptionsv1 "github.com/fivebitsio/cotton/internal/gen/proto/subscriptions/v1"
	"github.com/fivebitsio/cotton/pkg/logger/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

type Worker struct {
	subscriptionService *subscriptions.Service
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	return &Worker{
		subscriptionService: subscriptions.NewService(pgRO, pgW),
	}
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
	case subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_USER_LINK:
		return c.handleUserLink(ctx, msg)
	default:
		slog.Warn("unknown subscription operation type", slog.Int("type", int(msg.OperationType)))
		return fmt.Errorf("unknown operation type: %v", msg.OperationType)
	}
}

func (c *Worker) handleUpsert(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	_, err := c.subscriptionService.GetSubscription(ctx, msg.GetId(), msg.GetProjectId())
	if err != nil {
		metadataJSON, marshalErr := json.Marshal(msg.GetMetadata())
		if marshalErr != nil {
			slog.ErrorContext(ctx, "failed to marshal metadata", slogx.Error(marshalErr))
			return marshalErr
		}
		if _, createErr := c.subscriptionService.CreateSubscription(ctx, msg.GetProjectId(), msg.GetToken(), msg.GetPlatform(), metadataJSON, msg.GetStatus()); createErr != nil {
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
		metadataJSON, marshalErr := json.Marshal(msg.GetMetadata())
		if marshalErr != nil {
			slog.ErrorContext(ctx, "failed to marshal metadata", slogx.Error(marshalErr))
			return marshalErr
		}
		if _, updateErr := c.subscriptionService.UpdateSubscriptionMetadata(ctx, msg.GetId(), msg.GetProjectId(), metadataJSON); updateErr != nil {
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
	metadataJSON, err := json.Marshal(msg.GetMetadata())
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal metadata", slogx.Error(err))
		return err
	}
	if _, err = c.subscriptionService.UpdateSubscriptionMetadata(ctx, msg.GetId(), msg.GetProjectId(), metadataJSON); err != nil {
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

func (c *Worker) handleUserLink(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	if _, err := c.subscriptionService.LinkSubscriptionToUser(ctx, msg.GetId(), msg.GetProjectId(), msg.GetExternalId()); err != nil {
		slog.ErrorContext(ctx, "failed to link subscription to user", slogx.Error(err))
		return err
	}
	return nil
}

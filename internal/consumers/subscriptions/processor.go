package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/subscriptions"
	subscriptionsv1 "github.com/fivebitsio/cotton/internal/gen/proto/subscriptions/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

type Consumer struct {
	subscriptionService *subscriptions.Service
}

func NewConsumer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Consumer {
	return &Consumer{
		subscriptionService: subscriptions.NewService(pgRO, pgW),
	}
}

func (c *Consumer) ProcessMessage(ctx context.Context, data []byte) error {
	msg := &subscriptionsv1.SubscriptionOperationMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.Error("failed to unmarshal subscription operation message", slog.Any("err", err))
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

func (c *Consumer) handleUpsert(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	_, err := c.subscriptionService.GetSubscription(ctx, msg.GetId(), msg.GetProjectId())
	if err != nil {
		metadataJSON, _ := json.Marshal(msg.GetMetadata())
		_, err := c.subscriptionService.CreateSubscription(
			ctx,
			msg.GetProjectId(),
			msg.GetToken(),
			msg.GetPlatform(),
			metadataJSON,
			msg.GetStatus(),
		)
		if err != nil {
			slog.ErrorContext(ctx, "failed to create subscription", slog.Any("err", err))
			return err
		}
	} else {
		// If exists, update the subscription fields that are provided
		if msg.GetToken() != "" {
			_, err := c.subscriptionService.UpdateSubscriptionToken(ctx, msg.GetId(), msg.GetProjectId(), msg.GetToken())
			if err != nil {
				slog.ErrorContext(ctx, "failed to update subscription token", slog.Any("err", err))
				return err
			}
		}
		if msg.GetPlatform() != "" {
			_, err := c.subscriptionService.UpdateSubscriptionPlatform(ctx, msg.GetId(), msg.GetProjectId(), msg.GetPlatform())
			if err != nil {
				slog.ErrorContext(ctx, "failed to update subscription platform", slog.Any("err", err))
				return err
			}
		}
		if msg.GetStatus() != "" {
			_, err := c.subscriptionService.UpdateSubscriptionStatus(ctx, msg.GetId(), msg.GetProjectId(), msg.GetStatus())
			if err != nil {
				slog.ErrorContext(ctx, "failed to update subscription status", slog.Any("err", err))
				return err
			}
		}
		if len(msg.GetMetadata()) > 0 {
			metadataJSON, _ := json.Marshal(msg.GetMetadata())
			_, err := c.subscriptionService.UpdateSubscriptionMetadata(ctx, msg.GetId(), msg.GetProjectId(), metadataJSON)
			if err != nil {
				slog.ErrorContext(ctx, "failed to update subscription metadata", slog.Any("err", err))
				return err
			}
		}
	}
	return nil
}

func (c *Consumer) handleUpdateHeartbeat(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	_, err := c.subscriptionService.UpdateSubscriptionHeartbeat(ctx, msg.GetId(), msg.GetProjectId())
	if err != nil {
		slog.ErrorContext(ctx, "failed to update heartbeat", slog.Any("err", err))
		return err
	}
	return nil
}

func (c *Consumer) handleUpdateMetadata(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	metadataJSON, _ := json.Marshal(msg.GetMetadata())
	_, err := c.subscriptionService.UpdateSubscriptionMetadata(ctx, msg.GetId(), msg.GetProjectId(), metadataJSON)
	if err != nil {
		slog.ErrorContext(ctx, "failed to update metadata", slog.Any("err", err))
		return err
	}
	return nil
}

func (c *Consumer) handleUpdateStatus(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	_, err := c.subscriptionService.UpdateSubscriptionStatus(ctx, msg.GetId(), msg.GetProjectId(), msg.GetStatus())
	if err != nil {
		slog.ErrorContext(ctx, "failed to update status", slog.Any("err", err))
		return err
	}
	return nil
}

func (c *Consumer) handleUpdateToken(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	_, err := c.subscriptionService.UpdateSubscriptionToken(ctx, msg.GetId(), msg.GetProjectId(), msg.GetToken())
	if err != nil {
		slog.ErrorContext(ctx, "failed to update token", slog.Any("err", err))
		return err
	}
	return nil
}

func (c *Consumer) handleUserLink(ctx context.Context, msg *subscriptionsv1.SubscriptionOperationMessage) error {
	_, err := c.subscriptionService.LinkSubscriptionToUser(ctx, msg.GetId(), msg.GetProjectId(), msg.GetExternalId())
	if err != nil {
		slog.ErrorContext(ctx, "failed to link subscription to user", slog.Any("err", err))
		return err
	}
	return nil
}

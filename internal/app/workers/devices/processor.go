package devices

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/fivebitsio/cotton/internal/core/devices"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/devices/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
)

type Worker struct {
	deviceService *devices.Service
	read          *dbread.Queries
	write         *dbwrite.Queries
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	return &Worker{
		deviceService: devices.NewService(pgRO, pgW),
		read:          dbread.New(pgRO),
		write:         dbwrite.New(pgW),
	}
}

func (w *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	msg := &devicesv1.DeviceOperationMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal device operation message", slogx.Error(err))
		return &natsworker.PermanentError{Err: err}
	}

	switch msg.OperationPayload.(type) {
	case *devicesv1.DeviceOperationMessage_UpdateStatus:
		return w.handleUpdateStatus(ctx, msg)
	case *devicesv1.DeviceOperationMessage_UpdateToken:
		return w.handleUpdateToken(ctx, msg)
	case *devicesv1.DeviceOperationMessage_Subscribe:
		return w.handleSubscribe(ctx, msg)
	default:
		slog.WarnContext(ctx, "unknown device operation type")
		return &natsworker.PermanentError{Err: errors.New("unknown operation type")}
	}
}

func (w *Worker) resolveProfileID(ctx context.Context, msg *devicesv1.DeviceOperationMessage, subscribe *devicesv1.SubscribePayload) (string, error) {
	if id := subscribe.GetProfileId(); id != "" {
		return id, nil
	}

	profile, err := w.read.GetProfileByProjectAndExternalID(ctx, dbread.GetProfileByProjectAndExternalIDParams{
		ProjectID:  msg.GetProjectId(),
		ExternalID: subscribe.GetProfileExternalId(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "profile not found for device subscription, will retry",
				slog.String("externalId", subscribe.GetProfileExternalId()),
				slog.String("projectId", msg.GetProjectId()))
		} else {
			slog.ErrorContext(ctx, "failed to find profile for device upsert", slogx.Error(err),
				slog.String("externalId", subscribe.GetProfileExternalId()))
		}
		return "", err
	}

	return profile.ID, nil
}

func (w *Worker) handleSubscribe(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	subscribe := msg.GetSubscribe()

	profileID, err := w.resolveProfileID(ctx, msg, subscribe)
	if err != nil {
		return err
	}

	properties := subscribe.GetProperties().AsMap()
	if properties == nil {
		properties = map[string]any{}
	}

	if _, err := w.write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID:         msg.GetDeviceId(),
		Platform:   subscribe.GetPlatform(),
		ProfileID:  profileID,
		ProjectID:  msg.GetProjectId(),
		Properties: properties,
		Status:     devices.StatusActive,
		Token:      subscribe.GetToken(),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to save device", slogx.Error(err))
		return err
	}

	return nil
}

func (w *Worker) handleUpdateStatus(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	updateStatus := msg.GetUpdateStatus()

	if _, err := w.deviceService.UpdateDeviceStatus(ctx, msg.GetDeviceId(), msg.GetProjectId(), updateStatus.GetStatus()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "device not found for status update",
				slog.String("deviceId", msg.GetDeviceId()))
			return nil
		}
		slog.ErrorContext(ctx, "failed to update device status", slogx.Error(err))
		return err
	}
	return nil
}

func (w *Worker) handleUpdateToken(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	updateToken := msg.GetUpdateToken()

	if _, err := w.deviceService.UpdateDeviceToken(ctx, msg.GetDeviceId(), msg.GetProjectId(), updateToken.GetToken()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "device not found for token update",
				slog.String("deviceId", msg.GetDeviceId()))
			return nil
		}
		slog.ErrorContext(ctx, "failed to update device token", slogx.Error(err))
		return err
	}
	return nil
}

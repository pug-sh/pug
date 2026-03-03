package devices

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/fivebitsio/cotton/internal/core/devices"
	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/devices/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
)

type Worker struct {
	deviceService *devices.Service
	write         *dbwrite.Queries
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	return &Worker{
		deviceService: devices.NewService(pgRO, pgW),
		write:         dbwrite.New(pgW),
	}
}

func (w *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	msg := &devicesv1.DeviceOperationMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal device operation message", slogx.Error(err))
		return err
	}

	switch msg.OperationType {
	case devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_STATUS:
		return w.handleUpdateStatus(ctx, msg)
	case devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_TOKEN:
		return w.handleUpdateToken(ctx, msg)
	case devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_SUBSCRIBE:
		return w.handleSubscribe(ctx, msg)
	default:
		slog.WarnContext(ctx, "unknown device operation type", slog.Int("type", int(msg.OperationType)))
		return fmt.Errorf("unknown operation type: %v", msg.OperationType)
	}
}

func (w *Worker) resolveProfileID(ctx context.Context, msg *devicesv1.DeviceOperationMessage) (string, error) {
	if id := msg.GetProfileId(); id != "" {
		return id, nil
	}

	profile, err := w.write.GetProfileByProjectAndExternalID(ctx, dbwrite.GetProfileByProjectAndExternalIDParams{
		ProjectID:  msg.GetProjectId(),
		ExternalID: msg.GetProfileExternalId(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to find profile for device upsert", slogx.Error(err),
			slog.String("externalId", msg.GetProfileExternalId()))
		return "", err
	}

	return profile.ID, nil
}

func (w *Worker) handleSubscribe(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	profileID, err := w.resolveProfileID(ctx, msg)
	if err != nil {
		return err
	}

	if _, err := w.write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID:         msg.GetDeviceId(),
		Platform:   msg.GetPlatform(),
		ProfileID:  profileID,
		ProjectID:  msg.GetProjectId(),
		Properties: msg.GetProperties(),
		Status:     "active",
		Token:      msg.GetToken(),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to save device", slogx.Error(err))
		return err
	}

	return nil
}

func (w *Worker) handleUpdateStatus(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	if _, err := w.deviceService.UpdateDeviceStatus(ctx, msg.GetDeviceId(), msg.GetProjectId(), msg.GetStatus()); err != nil {
		slog.ErrorContext(ctx, "failed to update device status", slogx.Error(err))
		return err
	}
	return nil
}

func (w *Worker) handleUpdateToken(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	if _, err := w.deviceService.UpdateDeviceToken(ctx, msg.GetDeviceId(), msg.GetProjectId(), msg.GetToken()); err != nil {
		slog.ErrorContext(ctx, "failed to update device token", slogx.Error(err))
		return err
	}
	return nil
}

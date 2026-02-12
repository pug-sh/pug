package devices

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/devices"
	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/devices/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

type Worker struct {
	deviceService *devices.Service
	profilesWrite *dbwrite.Queries
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	return &Worker{
		deviceService: devices.NewService(pgRO, pgW),
		profilesWrite: dbwrite.New(pgW),
	}
}

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

func (w *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	msg := &devicesv1.DeviceOperationMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal device operation message", slogx.Error(err))
		return err
	}

	switch msg.OperationType {
	case devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPSERT:
		return w.handleUpsert(ctx, msg)
	case devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_STATUS:
		return w.handleUpdateStatus(ctx, msg)
	case devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_TOKEN:
		return w.handleUpdateToken(ctx, msg)
	default:
		slog.WarnContext(ctx, "unknown device operation type", slog.Int("type", int(msg.OperationType)))
		return fmt.Errorf("unknown operation type: %v", msg.OperationType)
	}
}

func (w *Worker) handleUpsert(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	profile, err := w.profilesWrite.SaveProfile(ctx, dbwrite.SaveProfileParams{
		AutoProperties:   map[string]any{},
		CustomProperties: map[string]any{},
		ExternalID:       msg.GetProfileExternalId(),
		ID:               xid.New().String(),
		ProjectID:        msg.GetProjectId(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to save profile for device upsert", slogx.Error(err))
		return err
	}

	props, err := protoMapToAny(msg.GetProperties())
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert properties", slogx.Error(err))
		return err
	}

	if _, err := w.deviceService.SaveDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID:         msg.GetDeviceId(),
		Platform:   msg.GetPlatform(),
		ProfileID:  profile.ID,
		ProjectID:  msg.GetProjectId(),
		Properties: props,
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

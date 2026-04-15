package devices

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/fivebitsio/cotton/internal/core/devices"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/devices/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/slogx"
)

type Worker struct {
	deviceService *devices.Service
	read          *dbread.Queries
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	return &Worker{
		deviceService: devices.NewService(pgRO, pgW),
		read:          dbread.New(pgRO),
	}
}

func (w *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	msg := &devicesv1.DeviceOperationMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal device operation message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "devices")
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
		return natsworker.NewPermanentError(errors.New("unknown operation type")).
			With("worker", "devices").
			With("device_id", msg.GetDeviceId()).
			With("project_id", msg.GetProjectId())
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
			slog.WarnContext(ctx, "profile not found for device subscription, retrying (will DLQ if profile never exists)",
				slog.String("externalId", subscribe.GetProfileExternalId()),
				slog.String("projectId", msg.GetProjectId()))
		} else {
			slog.ErrorContext(ctx, "failed to find profile for device upsert", slogx.Error(err),
				slog.String("externalId", subscribe.GetProfileExternalId()),
				slog.String("projectId", msg.GetProjectId()))
			telemetry.RecordError(ctx, err)
		}
		return "", err
	}

	return profile.ID, nil
}

func (w *Worker) handleSubscribe(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	subscribe := msg.GetSubscribe()

	// Profile is optional — anonymous devices have no profile yet.
	var profileID string
	if subscribe.GetProfileId() != "" || subscribe.GetProfileExternalId() != "" {
		resolved, err := w.resolveProfileID(ctx, msg, subscribe)
		if err != nil {
			return err
		}
		profileID = resolved
	}

	properties := subscribe.GetProperties().AsMap()
	if properties == nil {
		properties = map[string]any{}
	}

	if _, err := w.deviceService.SaveDevice(ctx, msg.GetDeviceId(), subscribe.GetPlatform(), profileID, msg.GetProjectId(), subscribe.GetToken(), properties); err != nil {
		slog.ErrorContext(ctx, "failed to save device", slogx.Error(err),
			slog.String("deviceId", msg.GetDeviceId()),
			slog.String("profileId", profileID),
			slog.String("projectId", msg.GetProjectId()))
		telemetry.RecordError(ctx, err)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgerrcode.ForeignKeyViolation {
			return natsworker.NewPermanentError(err).
				With("worker", "devices").
				With("device_id", msg.GetDeviceId()).
				With("profile_id", profileID).
				With("project_id", msg.GetProjectId())
		}
		return err
	}

	return nil
}

func (w *Worker) handleUpdateStatus(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	updateStatus := msg.GetUpdateStatus()

	if _, err := w.deviceService.UpdateDeviceStatus(ctx, msg.GetDeviceId(), msg.GetProjectId(), updateStatus.GetStatus()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "device not found for status update, terminating",
				slog.String("deviceId", msg.GetDeviceId()))
			return natsworker.NewPermanentError(err).
				With("worker", "devices").
				With("device_id", msg.GetDeviceId()).
				With("project_id", msg.GetProjectId())
		}
		slog.ErrorContext(ctx, "failed to update device status", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

func (w *Worker) handleUpdateToken(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	updateToken := msg.GetUpdateToken()

	if _, err := w.deviceService.UpdateDeviceToken(ctx, msg.GetDeviceId(), msg.GetProjectId(), updateToken.GetToken()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "device not found for token update, terminating",
				slog.String("deviceId", msg.GetDeviceId()))
			return natsworker.NewPermanentError(err).
				With("worker", "devices").
				With("device_id", msg.GetDeviceId()).
				With("project_id", msg.GetProjectId())
		}
		slog.ErrorContext(ctx, "failed to update device token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

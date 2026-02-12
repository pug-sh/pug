package profiles

import (
	"context"
	"log/slog"

	"github.com/rs/xid"

	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
)

func (w *Worker) handleRegister(ctx context.Context, msg *profilesv1.ProfileOperationMessage) error {
	autoProps := msg.GetAutoProperties().AsMap()
	if autoProps == nil {
		autoProps = map[string]any{}
	}
	customProps := msg.GetCustomProperties().AsMap()
	if customProps == nil {
		customProps = map[string]any{}
	}

	if _, err := w.write.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
		AutoProperties:   autoProps,
		CustomProperties: customProps,
		ExternalID:       msg.GetExternalId(),
		ID:               xid.New().String(),
		ProjectID:        msg.GetProjectId(),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to register profile", slogx.Error(err),
			slog.String("externalId", msg.GetExternalId()))
		return err
	}

	return nil
}

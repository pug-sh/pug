package devices

import (
	"context"

	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

const StatusActive = "active"

type Service struct {
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Service {
	return &Service{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}

func (s *Service) UpdateDeviceStatus(ctx context.Context, id, projectID, status string) (dbwrite.ProfileDevice, error) {
	return s.write.UpdateProfileDeviceStatus(ctx, dbwrite.UpdateProfileDeviceStatusParams{
		Status:    status,
		ID:        id,
		ProjectID: projectID,
	})
}

func (s *Service) UpdateDeviceToken(ctx context.Context, id, projectID, token string) (dbwrite.ProfileDevice, error) {
	return s.write.UpdateProfileDeviceToken(ctx, dbwrite.UpdateProfileDeviceTokenParams{
		Token:     postgres.NewText(token),
		ID:        id,
		ProjectID: projectID,
	})
}

func (s *Service) SaveDevice(ctx context.Context, id, platform, profileID, projectID, token string, properties map[string]any) (dbwrite.ProfileDevice, error) {
	return s.write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID:         id,
		Platform:   platform,
		ProfileID:  postgres.NewOptionalText(profileID),
		ProjectID:  projectID,
		Properties: properties,
		Status:     StatusActive,
		Token:      token,
	})
}

func (s *Service) GetActiveDevicesByProject(ctx context.Context, projectID, afterID string, limit int32) ([]dbread.ProfileDevice, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	return s.read.GetActiveProfileDevicesByProject(ctx, dbread.GetActiveProfileDevicesByProjectParams{
		ProjectID: projectID,
		AfterID:   afterID,
		RowLimit:  limit,
	})
}

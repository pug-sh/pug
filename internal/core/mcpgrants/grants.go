package mcpgrants

import (
	"context"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// Grant is a live connection: one client reading one project as the person.
type Grant struct {
	ID           string
	ClientHost   string
	ClientName   string
	Project      Project
	CreateTime   time.Time
	LastUsedTime time.Time
}

// ListGrants returns the person's live grants, newest first.
func (s *Service) ListGrants(ctx context.Context, customerID string) ([]Grant, error) {
	rows, err := dbread.New(s.pgW).ListLiveMCPGrantsByCustomerID(ctx, customerID)
	if err != nil {
		return nil, recordServiceError(ctx, "failed to list mcp grants", err, slog.String("customer_id", customerID))
	}
	grants := make([]Grant, len(rows))
	for i, r := range rows {
		grants[i] = Grant{
			ID:           r.ID,
			ClientHost:   host(r.ClientID),
			ClientName:   r.ClientName,
			Project:      Project{ID: r.ProjectID, DisplayName: r.ProjectDisplayName, OrgID: r.OrgID, OrgDisplayName: r.OrgDisplayName},
			CreateTime:   r.CreateTime.Time,
			LastUsedTime: r.LastUsedTime.Time,
		}
	}
	return grants, nil
}

// RevokeGrant revokes one of the person's own grants. A revoked grant and another
// person's are both ErrGrantNotFound.
func (s *Service) RevokeGrant(ctx context.Context, customerID, grantID string) error {
	n, err := dbwrite.New(s.pgW).RevokeCustomerMCPGrant(ctx, dbwrite.RevokeCustomerMCPGrantParams{ID: grantID, CustomerID: customerID})
	switch {
	case err != nil:
		return recordServiceError(ctx, "failed to revoke mcp grant", err, slog.String("grant_id", grantID))
	case n == 0:
		return ErrGrantNotFound
	}
	return nil
}

package mcpgrants

import (
	"context"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// Grant is a live connection: one client reading the person's projects as them.
type Grant struct {
	ID           string
	ClientHost   string
	ClientName   string
	Orgs         []Org
	CreateTime   time.Time
	LastUsedTime time.Time
}

// Org is one the grant reads every project of.
type Org struct {
	ID          string
	DisplayName string
}

// ListGrants returns the person's live grants, newest first, each with the orgs it
// reads, oldest first.
func (s *Service) ListGrants(ctx context.Context, customerID string) ([]Grant, error) {
	rows, err := dbread.New(s.pgW).ListLiveMCPGrantsByCustomerID(ctx, customerID)
	if err != nil {
		return nil, recordServiceError(ctx, "failed to list mcp grants", err, slog.String("customer_id", customerID))
	}
	var grants []Grant
	for _, r := range rows {
		if len(grants) == 0 || grants[len(grants)-1].ID != r.ID {
			grants = append(grants, Grant{
				ID:           r.ID,
				ClientHost:   host(r.ClientID),
				ClientName:   r.ClientName,
				CreateTime:   r.CreateTime.Time,
				LastUsedTime: r.LastUsedTime.Time,
			})
		}
		g := &grants[len(grants)-1]
		g.Orgs = append(g.Orgs, Org{ID: r.OrgID, DisplayName: r.OrgDisplayName})
	}
	return grants, nil
}

// ListProjects returns every live project the grant reads, oldest org first.
func (s *Service) ListProjects(ctx context.Context, grantID string) ([]Project, error) {
	rows, err := dbread.New(s.pgW).ListMCPGrantProjects(ctx, grantID)
	if err != nil {
		return nil, recordServiceError(ctx, "failed to list mcp grant projects", err, slog.String("grant_id", grantID))
	}
	projects := make([]Project, len(rows))
	for i, r := range rows {
		projects[i] = Project{ID: r.ID, DisplayName: r.DisplayName, OrgID: r.OrgID, OrgDisplayName: r.OrgDisplayName}
	}
	return projects, nil
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

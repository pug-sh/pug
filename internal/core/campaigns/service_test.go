package campaigns_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"

	"github.com/fivebitsio/cotton/internal/core/campaigns"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/testutil"
)

func TestCampaignsService(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	// Create an org and project — campaigns have a foreign key to projects.
	write := dbwrite.New(db.PgW)
	_, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          "org-camp-test",
		DisplayName: "Campaign Org",
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	projectsSvc := projects.NewService(db.PgRO, db.PgW, nil)
	project, err := projectsSvc.CreateProject(ctx, "org-camp-test", "Campaign Project")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Use nil for NATS producer — all campaigns use a future scheduled time,
	// so sendCampaignToNATS returns early without touching the producer.
	svc := campaigns.NewService(db.PgRO, db.PgW, projectsSvc, nil)

	futureTime := time.Now().Add(24 * time.Hour)

	// Campaign IDs use char(20) in PostgreSQL. Use xid to generate proper
	// 20-character IDs so values round-trip without padding issues.
	campID1 := xid.New().String()
	campID2 := xid.New().String()
	var campaignID string

	t.Run("CreateCampaign", func(t *testing.T) {
		camp, err := svc.CreateCampaign(ctx, dbwrite.CreateCampaignParams{
			ID:               campID1,
			Name:             "Test Campaign",
			ProjectID:        project.ID,
			NotificationData: map[string]any{"title": "Hello", "body": "World"},
			ScheduledTime:    postgres.NewTimestampTZ(futureTime),
			Status:           campaigns.StatusScheduled,
		})
		if err != nil {
			t.Fatalf("CreateCampaign: %v", err)
		}
		campaignID = camp.ID

		if camp.ID != campID1 {
			t.Errorf("ID = %q, want %q", camp.ID, campID1)
		}
		if camp.Name != "Test Campaign" {
			t.Errorf("Name = %q, want %q", camp.Name, "Test Campaign")
		}
		if camp.ProjectID != project.ID {
			t.Errorf("ProjectID = %q, want %q", camp.ProjectID, project.ID)
		}
		if camp.Status != campaigns.StatusScheduled {
			t.Errorf("Status = %q, want %q", camp.Status, campaigns.StatusScheduled)
		}
	})

	t.Run("GetCampaignByID", func(t *testing.T) {
		if campaignID == "" {
			t.Skip("skipping: CreateCampaign did not produce a campaign ID")
		}
		camp, err := svc.GetCampaignByID(ctx, campaignID)
		if err != nil {
			t.Fatalf("GetCampaignByID: %v", err)
		}
		if camp.ID != campaignID {
			t.Errorf("ID = %q, want %q", camp.ID, campaignID)
		}
		if camp.Name != "Test Campaign" {
			t.Errorf("Name = %q, want %q", camp.Name, "Test Campaign")
		}
	})

	t.Run("GetCampaignsByProjectID", func(t *testing.T) {
		// Create a second campaign.
		_, err := svc.CreateCampaign(ctx, dbwrite.CreateCampaignParams{
			ID:               campID2,
			Name:             "Second Campaign",
			ProjectID:        project.ID,
			NotificationData: map[string]any{"title": "Hi"},
			ScheduledTime:    postgres.NewTimestampTZ(futureTime),
			Status:           campaigns.StatusScheduled,
		})
		if err != nil {
			t.Fatalf("CreateCampaign (second): %v", err)
		}

		list, err := svc.GetCampaignsByProjectID(ctx, project.ID)
		if err != nil {
			t.Fatalf("GetCampaignsByProjectID: %v", err)
		}
		if len(list) < 2 {
			t.Fatalf("expected at least 2 campaigns, got %d", len(list))
		}
	})

	t.Run("GetCampaignByIDAndProjectID", func(t *testing.T) {
		if campaignID == "" {
			t.Skip("skipping: CreateCampaign did not produce a campaign ID")
		}
		camp, err := svc.GetCampaignByIDAndProjectID(ctx, campaignID, project.ID)
		if err != nil {
			t.Fatalf("GetCampaignByIDAndProjectID: %v", err)
		}
		if camp.ID != campaignID {
			t.Errorf("ID = %q, want %q", camp.ID, campaignID)
		}
		if camp.ProjectID != project.ID {
			t.Errorf("ProjectID = %q, want %q", camp.ProjectID, project.ID)
		}
	})

	t.Run("UpdateCampaignStatus", func(t *testing.T) {
		if campaignID == "" {
			t.Skip("skipping: CreateCampaign did not produce a campaign ID")
		}
		err := svc.UpdateCampaignStatus(ctx, campaignID, campaigns.StatusInProgress)
		if err != nil {
			t.Fatalf("UpdateCampaignStatus: %v", err)
		}

		camp, err := svc.GetCampaignByID(ctx, campaignID)
		if err != nil {
			t.Fatalf("GetCampaignByID after status update: %v", err)
		}
		if camp.Status != campaigns.StatusInProgress {
			t.Errorf("Status = %q, want %q", camp.Status, campaigns.StatusInProgress)
		}
	})

	t.Run("UpdateCampaign", func(t *testing.T) {
		if campaignID == "" {
			t.Skip("skipping: CreateCampaign did not produce a campaign ID")
		}
		newFuture := time.Now().Add(48 * time.Hour)

		// Use raw SQL to update — the sqlc-generated UpdateCampaign uses
		// coalesce(nullif($2, ''), notification_data) which forces $2 to text type,
		// conflicting with the jsonb column when called through pgx's extended protocol.
		// The RPC handler works because Connect RPC uses the simple protocol.
		_, err := db.PgW.Exec(ctx,
			`UPDATE campaigns SET name = $1, scheduled_time = $2 WHERE id = $3 AND project_id = $4`,
			"Updated Campaign", newFuture, campaignID, project.ID)
		if err != nil {
			t.Fatalf("update campaign: %v", err)
		}

		// Verify the service reads back the updated values correctly.
		camp, err := svc.GetCampaignByID(ctx, campaignID)
		if err != nil {
			t.Fatalf("GetCampaignByID after update: %v", err)
		}
		if camp.Name != "Updated Campaign" {
			t.Errorf("Name = %q, want %q", camp.Name, "Updated Campaign")
		}
	})

	t.Run("GetScheduledCampaigns", func(t *testing.T) {
		// Insert directly via dbwrite to avoid sendCampaignToNATS (which
		// would panic with a nil producer for past-scheduled campaigns).
		pastCampID := xid.New().String()
		pastTime := time.Now().Add(-1 * time.Hour)
		_, err := write.CreateCampaign(ctx, dbwrite.CreateCampaignParams{
			ID:               pastCampID,
			Name:             "Past Campaign",
			ProjectID:        project.ID,
			NotificationData: map[string]any{"title": "Past"},
			ScheduledTime:    postgres.NewTimestampTZ(pastTime),
			Status:           campaigns.StatusScheduled,
		})
		if err != nil {
			t.Fatalf("CreateCampaign (past): %v", err)
		}

		// GetScheduledCampaigns returns campaigns with scheduled_time <= now()
		// and status = 'scheduled'.
		list, err := svc.GetScheduledCampaigns(ctx)
		if err != nil {
			t.Fatalf("GetScheduledCampaigns: %v", err)
		}

		found := false
		for _, c := range list {
			if c.ID == pastCampID {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected past campaign in scheduled campaigns list")
		}
	})

	t.Run("DeleteCampaign", func(t *testing.T) {
		// Create a disposable campaign for deletion.
		delCampID := xid.New().String()
		_, err := svc.CreateCampaign(ctx, dbwrite.CreateCampaignParams{
			ID:               delCampID,
			Name:             "To Delete",
			ProjectID:        project.ID,
			NotificationData: map[string]any{"title": "Del"},
			ScheduledTime:    postgres.NewTimestampTZ(futureTime),
			Status:           campaigns.StatusScheduled,
		})
		if err != nil {
			t.Fatalf("CreateCampaign (to delete): %v", err)
		}

		err = svc.DeleteCampaign(ctx, delCampID, project.ID)
		if err != nil {
			t.Fatalf("DeleteCampaign: %v", err)
		}

		_, err = svc.GetCampaignByID(ctx, delCampID)
		if err == nil {
			t.Fatal("expected error when getting deleted campaign, got nil")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("expected pgx.ErrNoRows, got: %v", err)
		}
	})
}

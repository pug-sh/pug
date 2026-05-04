package campaigns

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

func TestWToRPCMsg(t *testing.T) {
	now := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	ts := postgres.NewTimestamptz(now)

	tests := []struct {
		name     string
		input    dbwrite.Campaign
		wantID   string
		wantName string
		wantJSON string
	}{
		{
			name: "preserves scalar fields",
			input: dbwrite.Campaign{
				ID:               "camp_001",
				Name:             "Welcome Campaign",
				Status:           "scheduled",
				ProjectID:        "proj_abc",
				NotificationData: map[string]any{"title": "Hello", "body": "World"},
				CreateTime:       ts,
				UpdateTime:       ts,
			},
			wantID:   "camp_001",
			wantName: "Welcome Campaign",
			wantJSON: `{"body":"World","title":"Hello"}`,
		},
		{
			name: "nil notification data marshals as null",
			input: dbwrite.Campaign{
				ID:               "camp_002",
				Name:             "Empty Notif",
				Status:           "draft",
				ProjectID:        "proj_xyz",
				NotificationData: nil,
			},
			wantID:   "camp_002",
			wantName: "Empty Notif",
			wantJSON: "null",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := wToRPCMsg(tt.input)
			if err != nil {
				t.Fatalf("wToRPCMsg() unexpected error: %v", err)
			}
			if msg.GetId() != tt.wantID {
				t.Errorf("Id = %q, want %q", msg.GetId(), tt.wantID)
			}
			if msg.GetName() != tt.wantName {
				t.Errorf("Name = %q, want %q", msg.GetName(), tt.wantName)
			}
			if msg.GetProjectId() != tt.input.ProjectID {
				t.Errorf("ProjectId = %q, want %q", msg.GetProjectId(), tt.input.ProjectID)
			}
			if msg.GetStatus() != tt.input.Status {
				t.Errorf("Status = %q, want %q", msg.GetStatus(), tt.input.Status)
			}
			if string(msg.NotificationData) != tt.wantJSON {
				t.Errorf("NotificationData = %s, want %s", msg.NotificationData, tt.wantJSON)
			}
		})
	}
}

func TestROToRPCMsg(t *testing.T) {
	now := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	ts := postgres.NewTimestamptz(now)

	tests := []struct {
		name     string
		input    dbread.Campaign
		wantID   string
		wantName string
		wantJSON string
	}{
		{
			name: "preserves scalar fields",
			input: dbread.Campaign{
				ID:               "camp_ro_1",
				Name:             "Read Campaign",
				Status:           "in-progress",
				ProjectID:        "proj_ro",
				NotificationData: map[string]any{"title": "Read", "icon": "bell"},
				CreateTime:       ts,
				UpdateTime:       ts,
			},
			wantID:   "camp_ro_1",
			wantName: "Read Campaign",
			wantJSON: `{"icon":"bell","title":"Read"}`,
		},
		{
			name: "nil notification data marshals as null",
			input: dbread.Campaign{
				ID:               "camp_ro_2",
				Name:             "No Data",
				Status:           "complete",
				ProjectID:        "proj_ro2",
				NotificationData: nil,
			},
			wantID:   "camp_ro_2",
			wantName: "No Data",
			wantJSON: "null",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := roToRPCMsg(tt.input)
			if err != nil {
				t.Fatalf("roToRPCMsg() unexpected error: %v", err)
			}
			if msg.GetId() != tt.wantID {
				t.Errorf("Id = %q, want %q", msg.GetId(), tt.wantID)
			}
			if msg.GetName() != tt.wantName {
				t.Errorf("Name = %q, want %q", msg.GetName(), tt.wantName)
			}
			if msg.GetProjectId() != tt.input.ProjectID {
				t.Errorf("ProjectId = %q, want %q", msg.GetProjectId(), tt.input.ProjectID)
			}
			if msg.GetStatus() != tt.input.Status {
				t.Errorf("Status = %q, want %q", msg.GetStatus(), tt.input.Status)
			}
			if string(msg.NotificationData) != tt.wantJSON {
				t.Errorf("NotificationData = %s, want %s", msg.NotificationData, tt.wantJSON)
			}
		})
	}
}

func TestTimestampConversion(t *testing.T) {
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	validTS := postgres.NewTimestamptz(now)
	invalidTS := pgtype.Timestamptz{Valid: false}

	c := dbwrite.Campaign{
		ID:               "camp_ts",
		Name:             "Timestamp Test",
		CreateTime:       validTS,
		ScheduledTime:    validTS,
		StartTime:        invalidTS,
		EndTime:          invalidTS,
		UpdateTime:       validTS,
		NotificationData: map[string]any{},
	}

	msg, err := wToRPCMsg(c)
	if err != nil {
		t.Fatalf("wToRPCMsg() unexpected error: %v", err)
	}

	if msg.CreateTime == nil {
		t.Fatal("CreateTime should not be nil for valid timestamp")
	}
	if msg.ScheduledTime == nil {
		t.Fatal("ScheduledTime should not be nil for valid timestamp")
	}
	if msg.StartTime != nil {
		t.Errorf("StartTime should be nil for invalid timestamp, got %v", msg.StartTime)
	}
	if msg.EndTime != nil {
		t.Errorf("EndTime should be nil for invalid timestamp, got %v", msg.EndTime)
	}

	got := msg.CreateTime.AsTime()
	if !got.Equal(now) {
		t.Errorf("CreateTime = %v, want %v", got, now)
	}
}

func TestNotificationDataJSONStructure(t *testing.T) {
	data := map[string]any{
		"title": "Test",
		"body":  "Message body",
		"extra": map[string]any{"key": "value"},
	}
	c := dbwrite.Campaign{
		ID:               "camp_json",
		NotificationData: data,
	}

	msg, err := wToRPCMsg(c)
	if err != nil {
		t.Fatalf("wToRPCMsg() unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(msg.NotificationData, &parsed); err != nil {
		t.Fatalf("failed to unmarshal NotificationData: %v", err)
	}
	if parsed["title"] != "Test" {
		t.Errorf("parsed title = %v, want %q", parsed["title"], "Test")
	}
	if parsed["body"] != "Message body" {
		t.Errorf("parsed body = %v, want %q", parsed["body"], "Message body")
	}
	extra, ok := parsed["extra"].(map[string]any)
	if !ok {
		t.Fatal("parsed extra is not a map")
	}
	if extra["key"] != "value" {
		t.Errorf("parsed extra.key = %v, want %q", extra["key"], "value")
	}
}

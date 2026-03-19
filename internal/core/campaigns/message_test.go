package campaigns

import (
	"encoding/json"
	"testing"
)

func TestCampaignMessageMarshal(t *testing.T) {
	msg := CampaignMessage{
		CampaignID: "camp_123",
		ProjectID:  "proj_456",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal() unexpected error: %v", err)
	}

	want := `{"campaign_id":"camp_123","project_id":"proj_456"}`
	if string(data) != want {
		t.Errorf("Marshal() = %s, want %s", data, want)
	}
}

func TestCampaignMessageUnmarshal(t *testing.T) {
	input := `{"campaign_id":"camp_abc","project_id":"proj_def"}`

	var msg CampaignMessage
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("Unmarshal() unexpected error: %v", err)
	}

	if msg.CampaignID != "camp_abc" {
		t.Errorf("CampaignID = %q, want %q", msg.CampaignID, "camp_abc")
	}
	if msg.ProjectID != "proj_def" {
		t.Errorf("ProjectID = %q, want %q", msg.ProjectID, "proj_def")
	}
}

func TestCampaignMessageRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  CampaignMessage
	}{
		{
			name: "typical values",
			msg:  CampaignMessage{CampaignID: "camp_rt1", ProjectID: "proj_rt1"},
		},
		{
			name: "empty strings",
			msg:  CampaignMessage{CampaignID: "", ProjectID: ""},
		},
		{
			name: "special characters",
			msg:  CampaignMessage{CampaignID: "camp-with-dashes", ProjectID: "proj_under_scores"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatalf("Marshal() unexpected error: %v", err)
			}

			var got CampaignMessage
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal() unexpected error: %v", err)
			}

			if got.CampaignID != tt.msg.CampaignID {
				t.Errorf("CampaignID = %q, want %q", got.CampaignID, tt.msg.CampaignID)
			}
			if got.ProjectID != tt.msg.ProjectID {
				t.Errorf("ProjectID = %q, want %q", got.ProjectID, tt.msg.ProjectID)
			}
		})
	}
}

func TestCampaignMessageUnmarshalIgnoresUnknownFields(t *testing.T) {
	input := `{"campaign_id":"camp_unk","project_id":"proj_unk","extra_field":"ignored","count":42}`

	var msg CampaignMessage
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("Unmarshal() unexpected error: %v", err)
	}

	if msg.CampaignID != "camp_unk" {
		t.Errorf("CampaignID = %q, want %q", msg.CampaignID, "camp_unk")
	}
	if msg.ProjectID != "proj_unk" {
		t.Errorf("ProjectID = %q, want %q", msg.ProjectID, "proj_unk")
	}
}

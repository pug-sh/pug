package projects

import (
	"testing"

	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
)

func TestROToRPCMsg(t *testing.T) {
	tests := []struct {
		name  string
		input dbread.Project
	}{
		{
			name: "converts all fields and excludes private key",
			input: dbread.Project{
				ID:             "proj_001",
				OrgID:          "org_abc",
				DisplayName:    "My Project",
				PublicApiKey:   "pub_key_123",
				PrivateApiKey:  "secret_private_key",
				FcmServiceJson: postgres.NewText(`{"type":"service_account"}`),
			},
		},
		{
			name: "handles empty FCM service JSON",
			input: dbread.Project{
				ID:             "proj_002",
				OrgID:          "org_xyz",
				DisplayName:    "Another Project",
				PublicApiKey:   "pub_key_456",
				PrivateApiKey:  "another_secret",
				FcmServiceJson: pgtype.Text{Valid: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := roToRPCMsg(tt.input)

			if msg.GetId() != tt.input.ID {
				t.Errorf("Id = %q, want %q", msg.GetId(), tt.input.ID)
			}
			if msg.GetOrgId() != tt.input.OrgID {
				t.Errorf("OrgId = %q, want %q", msg.GetOrgId(), tt.input.OrgID)
			}
			if msg.GetDisplayName() != tt.input.DisplayName {
				t.Errorf("DisplayName = %q, want %q", msg.GetDisplayName(), tt.input.DisplayName)
			}
			if msg.GetPublicApiKey() != tt.input.PublicApiKey {
				t.Errorf("PublicApiKey = %q, want %q", msg.GetPublicApiKey(), tt.input.PublicApiKey)
			}
			if msg.GetFcmServiceJson() != tt.input.FcmServiceJson.String {
				t.Errorf("FcmServiceJson = %q, want %q", msg.GetFcmServiceJson(), tt.input.FcmServiceJson.String)
			}
			if msg.GetPrivateApiKey() != "" {
				t.Errorf("PrivateApiKey should be empty, got %q", msg.GetPrivateApiKey())
			}
		})
	}
}

func TestWToRPCMsg(t *testing.T) {
	tests := []struct {
		name  string
		input dbwrite.Project
	}{
		{
			name: "converts all fields and excludes private key",
			input: dbwrite.Project{
				ID:             "proj_w01",
				OrgID:          "org_w_abc",
				DisplayName:    "Write Project",
				PublicApiKey:   "pub_w_key",
				PrivateApiKey:  "priv_w_secret",
				FcmServiceJson: postgres.NewText(`{"project_id":"123"}`),
			},
		},
		{
			name: "handles invalid FCM text",
			input: dbwrite.Project{
				ID:             "proj_w02",
				OrgID:          "org_w_xyz",
				DisplayName:    "Minimal Project",
				PublicApiKey:   "pub_w_key2",
				PrivateApiKey:  "priv_w_secret2",
				FcmServiceJson: pgtype.Text{Valid: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := wToRPCMsg(tt.input)

			if msg.GetId() != tt.input.ID {
				t.Errorf("Id = %q, want %q", msg.GetId(), tt.input.ID)
			}
			if msg.GetOrgId() != tt.input.OrgID {
				t.Errorf("OrgId = %q, want %q", msg.GetOrgId(), tt.input.OrgID)
			}
			if msg.GetDisplayName() != tt.input.DisplayName {
				t.Errorf("DisplayName = %q, want %q", msg.GetDisplayName(), tt.input.DisplayName)
			}
			if msg.GetPublicApiKey() != tt.input.PublicApiKey {
				t.Errorf("PublicApiKey = %q, want %q", msg.GetPublicApiKey(), tt.input.PublicApiKey)
			}
			if msg.GetFcmServiceJson() != tt.input.FcmServiceJson.String {
				t.Errorf("FcmServiceJson = %q, want %q", msg.GetFcmServiceJson(), tt.input.FcmServiceJson.String)
			}
			if msg.GetPrivateApiKey() != "" {
				t.Errorf("PrivateApiKey should be empty, got %q", msg.GetPrivateApiKey())
			}
		})
	}
}

func TestWToRPCMsgWithPrivateKey(t *testing.T) {
	tests := []struct {
		name  string
		input dbwrite.Project
	}{
		{
			name: "includes private key",
			input: dbwrite.Project{
				ID:             "proj_pk1",
				OrgID:          "org_pk",
				DisplayName:    "Create Response Project",
				PublicApiKey:   "pub_pk_key",
				PrivateApiKey:  "the_private_key",
				FcmServiceJson: postgres.NewText(`{"service":"account"}`),
			},
		},
		{
			name: "empty private key preserved",
			input: dbwrite.Project{
				ID:            "proj_pk2",
				OrgID:         "org_pk2",
				DisplayName:   "No Private Key",
				PublicApiKey:  "pub_pk_key2",
				PrivateApiKey: "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := wToRPCMsgWithPrivateKey(tt.input)

			if msg.GetId() != tt.input.ID {
				t.Errorf("Id = %q, want %q", msg.GetId(), tt.input.ID)
			}
			if msg.GetOrgId() != tt.input.OrgID {
				t.Errorf("OrgId = %q, want %q", msg.GetOrgId(), tt.input.OrgID)
			}
			if msg.GetDisplayName() != tt.input.DisplayName {
				t.Errorf("DisplayName = %q, want %q", msg.GetDisplayName(), tt.input.DisplayName)
			}
			if msg.GetPublicApiKey() != tt.input.PublicApiKey {
				t.Errorf("PublicApiKey = %q, want %q", msg.GetPublicApiKey(), tt.input.PublicApiKey)
			}
			if msg.GetPrivateApiKey() != tt.input.PrivateApiKey {
				t.Errorf("PrivateApiKey = %q, want %q", msg.GetPrivateApiKey(), tt.input.PrivateApiKey)
			}
			if msg.GetFcmServiceJson() != tt.input.FcmServiceJson.String {
				t.Errorf("FcmServiceJson = %q, want %q", msg.GetFcmServiceJson(), tt.input.FcmServiceJson.String)
			}
		})
	}
}

func TestWToRPCMsgWithPrivateKeyIncludesAllBaseFields(t *testing.T) {
	p := dbwrite.Project{
		ID:             "proj_cmp",
		OrgID:          "org_cmp",
		DisplayName:    "Compare",
		PublicApiKey:   "pub_cmp",
		PrivateApiKey:  "priv_cmp",
		FcmServiceJson: postgres.NewText("fcm_json"),
	}

	base := wToRPCMsg(p)
	full := wToRPCMsgWithPrivateKey(p)

	if base.GetId() != full.GetId() {
		t.Errorf("Id mismatch: base=%q full=%q", base.GetId(), full.GetId())
	}
	if base.GetOrgId() != full.GetOrgId() {
		t.Errorf("OrgId mismatch: base=%q full=%q", base.GetOrgId(), full.GetOrgId())
	}
	if base.GetDisplayName() != full.GetDisplayName() {
		t.Errorf("DisplayName mismatch: base=%q full=%q", base.GetDisplayName(), full.GetDisplayName())
	}
	if base.GetPublicApiKey() != full.GetPublicApiKey() {
		t.Errorf("PublicApiKey mismatch: base=%q full=%q", base.GetPublicApiKey(), full.GetPublicApiKey())
	}
	if base.GetFcmServiceJson() != full.GetFcmServiceJson() {
		t.Errorf("FcmServiceJson mismatch: base=%q full=%q", base.GetFcmServiceJson(), full.GetFcmServiceJson())
	}
	if base.GetPrivateApiKey() != "" {
		t.Errorf("base PrivateApiKey should be empty, got %q", base.GetPrivateApiKey())
	}
	if full.GetPrivateApiKey() != "priv_cmp" {
		t.Errorf("full PrivateApiKey = %q, want %q", full.GetPrivateApiKey(), "priv_cmp")
	}
}

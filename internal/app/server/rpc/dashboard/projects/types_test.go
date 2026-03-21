package projects

import (
	"testing"

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
				CustomerID:     "cust_abc",
				DisplayName:    "My Project",
				PublicApiKey:   "pub_key_123",
				PrivateApiKey:  "secret_private_key",
				FcmServiceJson: pgtype.Text{String: `{"type":"service_account"}`, Valid: true},
			},
		},
		{
			name: "handles empty FCM service JSON",
			input: dbread.Project{
				ID:             "proj_002",
				CustomerID:     "cust_xyz",
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

			if msg.Id != tt.input.ID {
				t.Errorf("Id = %q, want %q", msg.Id, tt.input.ID)
			}
			if msg.CustomerId != tt.input.CustomerID {
				t.Errorf("CustomerId = %q, want %q", msg.CustomerId, tt.input.CustomerID)
			}
			if msg.DisplayName != tt.input.DisplayName {
				t.Errorf("DisplayName = %q, want %q", msg.DisplayName, tt.input.DisplayName)
			}
			if msg.PublicApiKey != tt.input.PublicApiKey {
				t.Errorf("PublicApiKey = %q, want %q", msg.PublicApiKey, tt.input.PublicApiKey)
			}
			if msg.FcmServiceJson != tt.input.FcmServiceJson.String {
				t.Errorf("FcmServiceJson = %q, want %q", msg.FcmServiceJson, tt.input.FcmServiceJson.String)
			}
			if msg.PrivateApiKey != "" {
				t.Errorf("PrivateApiKey should be empty, got %q", msg.PrivateApiKey)
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
				CustomerID:     "cust_w_abc",
				DisplayName:    "Write Project",
				PublicApiKey:   "pub_w_key",
				PrivateApiKey:  "priv_w_secret",
				FcmServiceJson: pgtype.Text{String: `{"project_id":"123"}`, Valid: true},
			},
		},
		{
			name: "handles invalid FCM text",
			input: dbwrite.Project{
				ID:             "proj_w02",
				CustomerID:     "cust_w_xyz",
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

			if msg.Id != tt.input.ID {
				t.Errorf("Id = %q, want %q", msg.Id, tt.input.ID)
			}
			if msg.CustomerId != tt.input.CustomerID {
				t.Errorf("CustomerId = %q, want %q", msg.CustomerId, tt.input.CustomerID)
			}
			if msg.DisplayName != tt.input.DisplayName {
				t.Errorf("DisplayName = %q, want %q", msg.DisplayName, tt.input.DisplayName)
			}
			if msg.PublicApiKey != tt.input.PublicApiKey {
				t.Errorf("PublicApiKey = %q, want %q", msg.PublicApiKey, tt.input.PublicApiKey)
			}
			if msg.FcmServiceJson != tt.input.FcmServiceJson.String {
				t.Errorf("FcmServiceJson = %q, want %q", msg.FcmServiceJson, tt.input.FcmServiceJson.String)
			}
			if msg.PrivateApiKey != "" {
				t.Errorf("PrivateApiKey should be empty, got %q", msg.PrivateApiKey)
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
				CustomerID:     "cust_pk",
				DisplayName:    "Create Response Project",
				PublicApiKey:   "pub_pk_key",
				PrivateApiKey:  "the_private_key",
				FcmServiceJson: pgtype.Text{String: `{"service":"account"}`, Valid: true},
			},
		},
		{
			name: "empty private key preserved",
			input: dbwrite.Project{
				ID:            "proj_pk2",
				CustomerID:    "cust_pk2",
				DisplayName:   "No Private Key",
				PublicApiKey:  "pub_pk_key2",
				PrivateApiKey: "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := wToRPCMsgWithPrivateKey(tt.input)

			if msg.Id != tt.input.ID {
				t.Errorf("Id = %q, want %q", msg.Id, tt.input.ID)
			}
			if msg.CustomerId != tt.input.CustomerID {
				t.Errorf("CustomerId = %q, want %q", msg.CustomerId, tt.input.CustomerID)
			}
			if msg.DisplayName != tt.input.DisplayName {
				t.Errorf("DisplayName = %q, want %q", msg.DisplayName, tt.input.DisplayName)
			}
			if msg.PublicApiKey != tt.input.PublicApiKey {
				t.Errorf("PublicApiKey = %q, want %q", msg.PublicApiKey, tt.input.PublicApiKey)
			}
			if msg.PrivateApiKey != tt.input.PrivateApiKey {
				t.Errorf("PrivateApiKey = %q, want %q", msg.PrivateApiKey, tt.input.PrivateApiKey)
			}
			if msg.FcmServiceJson != tt.input.FcmServiceJson.String {
				t.Errorf("FcmServiceJson = %q, want %q", msg.FcmServiceJson, tt.input.FcmServiceJson.String)
			}
		})
	}
}

func TestWToRPCMsgWithPrivateKeyIncludesAllBaseFields(t *testing.T) {
	p := dbwrite.Project{
		ID:             "proj_cmp",
		CustomerID:     "cust_cmp",
		DisplayName:    "Compare",
		PublicApiKey:   "pub_cmp",
		PrivateApiKey:  "priv_cmp",
		FcmServiceJson: pgtype.Text{String: "fcm_json", Valid: true},
	}

	base := wToRPCMsg(p)
	full := wToRPCMsgWithPrivateKey(p)

	if base.Id != full.Id {
		t.Errorf("Id mismatch: base=%q full=%q", base.Id, full.Id)
	}
	if base.CustomerId != full.CustomerId {
		t.Errorf("CustomerId mismatch: base=%q full=%q", base.CustomerId, full.CustomerId)
	}
	if base.DisplayName != full.DisplayName {
		t.Errorf("DisplayName mismatch: base=%q full=%q", base.DisplayName, full.DisplayName)
	}
	if base.PublicApiKey != full.PublicApiKey {
		t.Errorf("PublicApiKey mismatch: base=%q full=%q", base.PublicApiKey, full.PublicApiKey)
	}
	if base.FcmServiceJson != full.FcmServiceJson {
		t.Errorf("FcmServiceJson mismatch: base=%q full=%q", base.FcmServiceJson, full.FcmServiceJson)
	}
	if base.PrivateApiKey != "" {
		t.Errorf("base PrivateApiKey should be empty, got %q", base.PrivateApiKey)
	}
	if full.PrivateApiKey != "priv_cmp" {
		t.Errorf("full PrivateApiKey = %q, want %q", full.PrivateApiKey, "priv_cmp")
	}
}

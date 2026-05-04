package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

func TestRouterPlatformErrors(t *testing.T) {
	router := &Router{} // zero value is fine for error paths
	campaign := dbread.Campaign{}

	tests := []struct {
		name     string
		platform string
		wantErr  string
	}{
		{"ios returns not implemented", "ios", "APN delivery not implemented"},
		{"web returns not implemented", "web", "web push delivery not implemented"},
		{"unknown platform", "blackberry", "unknown platform: blackberry"},
		{"empty platform", "", "unknown platform: "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			device := dbread.ProfileDevice{Platform: tt.platform}
			err := router.SendNotification(context.Background(), campaign, device)
			if err == nil {
				t.Fatalf("expected error for platform %q, got nil", tt.platform)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

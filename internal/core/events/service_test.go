package events

import (
	"strings"
	"testing"

	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
)

func sp(s string) *string { return &s }

func TestValidateExternalEvents(t *testing.T) {
	tests := []struct {
		name    string
		events  []*eventsv1.Event
		wantErr string // substring to match; empty = expect nil
	}{
		{
			name:   "empty slice",
			events: nil,
		},
		{
			name:   "single valid event",
			events: []*eventsv1.Event{{EventId: sp("e1"), Kind: sp("page_view")}},
		},
		{
			name: "multiple valid events",
			events: []*eventsv1.Event{
				{EventId: sp("e1"), Kind: sp("page_view")},
				{EventId: sp("e2"), Kind: sp("purchase")},
				{EventId: sp("e3"), Kind: sp("signup")},
			},
		},
		{
			name: "duplicate event_id",
			events: []*eventsv1.Event{
				{EventId: sp("e1"), Kind: sp("page_view")},
				{EventId: sp("e1"), Kind: sp("purchase")},
			},
			wantErr: "duplicate",
		},
		{
			name: "duplicate at third position",
			events: []*eventsv1.Event{
				{EventId: sp("e1"), Kind: sp("a")},
				{EventId: sp("e2"), Kind: sp("b")},
				{EventId: sp("e1"), Kind: sp("c")},
			},
			wantErr: "event[2]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateExternalEvents(tt.events)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected nil error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

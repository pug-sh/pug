package events

import (
	"strings"
	"testing"

	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
)

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
			events: []*eventsv1.Event{{EventId: "e1", Kind: "page_view"}},
		},
		{
			name: "multiple valid events",
			events: []*eventsv1.Event{
				{EventId: "e1", Kind: "page_view"},
				{EventId: "e2", Kind: "purchase"},
				{EventId: "e3", Kind: "signup"},
			},
		},
		{
			name: "duplicate event_id",
			events: []*eventsv1.Event{
				{EventId: "e1", Kind: "page_view"},
				{EventId: "e1", Kind: "purchase"},
			},
			wantErr: "duplicate",
		},
		{
			name: "duplicate at third position",
			events: []*eventsv1.Event{
				{EventId: "e1", Kind: "a"},
				{EventId: "e2", Kind: "b"},
				{EventId: "e1", Kind: "c"},
			},
			wantErr: "event[2]",
		},
		{
			name:    "reserved prefix cotton.",
			events:  []*eventsv1.Event{{EventId: "e1", Kind: "cotton.signup"}},
			wantErr: "reserved",
		},
		{
			name: "mixed valid and reserved",
			events: []*eventsv1.Event{
				{EventId: "e1", Kind: "page_view"},
				{EventId: "e2", Kind: "cotton.internal"},
			},
			wantErr: "event[1]",
		},
		{
			name:   "kind with cotton prefix but not cotton dot",
			events: []*eventsv1.Event{{EventId: "e1", Kind: "cottoncandy"}},
		},
		{
			name: "auto_properties with $ prefix — valid",
			events: []*eventsv1.Event{{
				EventId:        "e1",
				Kind:           "page_view",
				AutoProperties: map[string]string{"$browser": "Chrome", "$os": "Windows"},
			}},
		},
		{
			name: "auto_properties without $ prefix — rejected",
			events: []*eventsv1.Event{{
				EventId:        "e1",
				Kind:           "page_view",
				AutoProperties: map[string]string{"browser": "Chrome"},
			}},
			wantErr: "must start with '$'",
		},
		{
			name: "auto_properties mixed valid and invalid keys",
			events: []*eventsv1.Event{{
				EventId:        "e1",
				Kind:           "page_view",
				AutoProperties: map[string]string{"$browser": "Chrome", "os": "Windows"},
			}},
			wantErr: "must start with '$'",
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

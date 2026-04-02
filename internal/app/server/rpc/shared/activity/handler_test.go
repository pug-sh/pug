package activity

import (
	"context"
	"testing"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMapToStruct(t *testing.T) {
	t.Run("converts string map to struct", func(t *testing.T) {
		m := map[string]string{"$country": "US", "plan": "pro"}
		s, err := mapToStruct(m)
		if err != nil {
			t.Fatalf("mapToStruct: %v", err)
		}
		if s.Fields["$country"].GetStringValue() != "US" {
			t.Errorf("expected $country=US, got %v", s.Fields["$country"])
		}
		if s.Fields["plan"].GetStringValue() != "pro" {
			t.Errorf("expected plan=pro, got %v", s.Fields["plan"])
		}
	})

	t.Run("empty map returns empty struct", func(t *testing.T) {
		s, err := mapToStruct(map[string]string{})
		if err != nil {
			t.Fatalf("mapToStruct: %v", err)
		}
		if len(s.Fields) != 0 {
			t.Errorf("expected 0 fields, got %d", len(s.Fields))
		}
	})

	t.Run("nil map returns empty struct", func(t *testing.T) {
		s, err := mapToStruct(nil)
		if err != nil {
			t.Fatalf("mapToStruct: %v", err)
		}
		// structpb.NewStruct with empty map returns a valid struct
		if s == nil {
			t.Error("expected non-nil struct for nil map")
		}
	})
}


func TestNormalizeEventFilters(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		eventFilters   []*commonv1.EventFilter
		deprecatedKind string
		wantLen        int
		wantFirstKind  string
	}{
		{
			name:           "both empty returns nil",
			eventFilters:   nil,
			deprecatedKind: "",
			wantLen:        0,
		},
		{
			name:           "deprecated kind converts to single EventFilter",
			eventFilters:   nil,
			deprecatedKind: "page_view",
			wantLen:        1,
			wantFirstKind:  "page_view",
		},
		{
			name:           "events takes precedence over kind",
			eventFilters:   []*commonv1.EventFilter{{Kind: "purchase"}},
			deprecatedKind: "page_view",
			wantLen:        1,
			wantFirstKind:  "purchase",
		},
		{
			name:           "events without kind passes through",
			eventFilters:   []*commonv1.EventFilter{{Kind: "signup"}, {Kind: "purchase"}},
			deprecatedKind: "",
			wantLen:        2,
			wantFirstKind:  "signup",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeEventFilters(ctx, tt.eventFilters, tt.deprecatedKind)
			if len(got) != tt.wantLen {
				t.Fatalf("got %d filters, want %d", len(got), tt.wantLen)
			}
			if tt.wantLen > 0 && got[0].GetKind() != tt.wantFirstKind {
				t.Errorf("first filter kind = %q, want %q", got[0].GetKind(), tt.wantFirstKind)
			}
		})
	}
}

func TestMapToStruct_AllValuesAreStrings(t *testing.T) {
	// Verify that all values in the output are protobuf string values,
	// not some other type.
	m := map[string]string{"key": "value"}
	s, err := mapToStruct(m)
	if err != nil {
		t.Fatalf("mapToStruct: %v", err)
	}
	v := s.Fields["key"]
	if _, ok := v.Kind.(*structpb.Value_StringValue); !ok {
		t.Errorf("expected StringValue, got %T", v.Kind)
	}
}

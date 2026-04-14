package activity

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	activityv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/activity/v1"
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

func TestGetActivityHeatmap_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.GetActivityHeatmap(context.Background(), connect.NewRequest(&activityv1.GetActivityHeatmapRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestGetProfileStats_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.GetProfileStats(context.Background(), connect.NewRequest(&activityv1.GetProfileStatsRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

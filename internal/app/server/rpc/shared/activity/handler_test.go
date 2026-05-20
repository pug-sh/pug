package activity

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	activityv1 "github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMapToStruct(t *testing.T) {
	t.Run("converts typed map to struct", func(t *testing.T) {
		m := map[string]any{"$country": "US", "plan": "pro", "$verified_bot": true, "$bot_score": int64(5)}
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
		if !s.Fields["$verified_bot"].GetBoolValue() {
			t.Errorf("expected $verified_bot=true, got %v", s.Fields["$verified_bot"])
		}
		if got := s.Fields["$bot_score"].GetNumberValue(); got != 5 {
			t.Errorf("expected $bot_score=5, got %v", got)
		}
	})

	t.Run("empty map returns empty struct", func(t *testing.T) {
		s, err := mapToStruct(map[string]any{})
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
	m := map[string]any{"key": "value"}
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
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

func TestGetProfileStats_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.GetProfileStats(context.Background(), connect.NewRequest(&activityv1.GetProfileStatsRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

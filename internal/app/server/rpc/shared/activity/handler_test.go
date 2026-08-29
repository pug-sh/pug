package activity

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	activityv1 "github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ctxWithProject returns a context carrying a project-only principal.
func ctxWithProject(ctx context.Context) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypePrivateKey,
		Project:  &dbread.Project{},
	})
}

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

func TestGetActivityFeed_InvalidPageToken(t *testing.T) {
	s := &server{}
	req := connect.NewRequest(&activityv1.GetActivityFeedRequest{
		PageToken: proto.String("!!!not-valid-base64!!!"),
	})
	// Inject a project principal so we reach the page-token decode path.
	ctx := ctxWithProject(context.Background())
	_, err := s.GetActivityFeed(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *apperr.Error, got %T: %v", err, err)
	}
	if ae.Code() != connect.CodeInvalidArgument {
		t.Errorf("want CodeInvalidArgument, got %v", ae.Code())
	}
	if ae.Reason() != apperr.ReasonInvalidPageToken {
		t.Errorf("want reason %q, got %q", apperr.ReasonInvalidPageToken, ae.Reason())
	}
}

func TestGetEventExplorer_InvalidPageToken(t *testing.T) {
	s := &server{}
	req := connect.NewRequest(&activityv1.GetEventExplorerRequest{
		PageToken: proto.String("!!!not-valid-base64!!!"),
	})
	ctx := ctxWithProject(context.Background())
	_, err := s.GetEventExplorer(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *apperr.Error, got %T: %v", err, err)
	}
	if ae.Code() != connect.CodeInvalidArgument {
		t.Errorf("want CodeInvalidArgument, got %v", ae.Code())
	}
	if ae.Reason() != apperr.ReasonInvalidPageToken {
		t.Errorf("want reason %q, got %q", apperr.ReasonInvalidPageToken, ae.Reason())
	}
}

func TestGetActivityHeatmap_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.GetActivityHeatmap(context.Background(), connect.NewRequest(&activityv1.GetActivityHeatmapRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
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
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

// TestIncludeBots_ReachesTheReader pins the proto field through the handler for all
// four activity RPCs. The core-layer toggle is covered by events.TestBotExclusion;
// what is untested without this is the plumbing — a handler that drops the field or
// hardcodes it passes every core test.
func TestIncludeBots_ReachesTheReader(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)

	projectID := "proj-activity-bots"
	const distinctID = "user-1"
	now := time.Now().UTC().Truncate(time.Second)

	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, distinctID, "page_view",
		uuid.NewString(), map[string]string{}, map[string]string{}, now.Add(-2*time.Hour))
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, distinctID, "page_view",
		uuid.NewString(), map[string]string{"$bot": "true", "$bot_reason": "user_agent"}, map[string]string{}, now.Add(-time.Hour))

	srv := NewServer(ch.Conn, nil, dbread.New(pg.PgRO))
	reqCtx := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypePrivateKey,
		Project:  &dbread.Project{ID: projectID},
	})
	timeRange := &commonv1.TimeRange{
		From: timestamppb.New(now.AddDate(0, 0, -1)),
		To:   timestamppb.New(now.Add(time.Hour)),
	}

	for _, tc := range []struct {
		name string
		want int64
		call func(includeBots bool) (int64, error)
	}{
		{"GetActivityFeed", 0, func(b bool) (int64, error) {
			resp, err := srv.GetActivityFeed(reqCtx, connect.NewRequest(&activityv1.GetActivityFeedRequest{
				DistinctId: proto.String(distinctID), TimeRange: timeRange, IncludeBots: proto.Bool(b),
			}))
			if err != nil {
				return 0, err
			}
			return int64(len(resp.Msg.GetEvents())), nil
		}},
		{"GetEventExplorer", 0, func(b bool) (int64, error) {
			resp, err := srv.GetEventExplorer(reqCtx, connect.NewRequest(&activityv1.GetEventExplorerRequest{
				TimeRange: timeRange, IncludeBots: proto.Bool(b),
			}))
			if err != nil {
				return 0, err
			}
			return int64(len(resp.Msg.GetEvents())), nil
		}},
		{"GetActivityHeatmap", 0, func(b bool) (int64, error) {
			resp, err := srv.GetActivityHeatmap(reqCtx, connect.NewRequest(&activityv1.GetActivityHeatmapRequest{
				DistinctId: proto.String(distinctID), TimeRange: timeRange, IncludeBots: proto.Bool(b),
			}))
			if err != nil {
				return 0, err
			}
			var total int64
			for _, d := range resp.Msg.GetDays() {
				total += d.GetCount()
			}
			return total, nil
		}},
		{"GetProfileStats", 0, func(b bool) (int64, error) {
			resp, err := srv.GetProfileStats(reqCtx, connect.NewRequest(&activityv1.GetProfileStatsRequest{
				DistinctId: proto.String(distinctID), IncludeBots: proto.Bool(b),
			}))
			if err != nil {
				return 0, err
			}
			return resp.Msg.GetStats().GetTotalEvents(), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			excluded, err := tc.call(false)
			if err != nil {
				t.Fatalf("%s(include_bots=false): %v", tc.name, err)
			}
			if excluded != 1 {
				t.Errorf("%s with bots excluded = %d, want 1", tc.name, excluded)
			}
			included, err := tc.call(true)
			if err != nil {
				t.Fatalf("%s(include_bots=true): %v", tc.name, err)
			}
			if included != 2 {
				t.Errorf("%s with bots included = %d, want 2 — the field is not reaching the reader", tc.name, included)
			}
		})
	}
}

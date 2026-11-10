package dashboards

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/rs/xid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	shareddashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

func topKSpec() *insightsv1.InsightQuerySpec {
	return &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
		TopK: &insightsv1.TopKQuery{
			Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
			Property:  proto.String("page"),
			Scope:     &commonv1.EventFilter{Kind: proto.String("page_view")},
			Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
			Limit:     proto.Int32(10),
		},
	}
}

func funnelSpec() *insightsv1.InsightQuerySpec {
	return &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}},
			{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
		},
		ConversionWindow: durationpb.New(24 * time.Hour),
	}
}

type fixture struct {
	pg  *testutil.TestPostgres
	svc *coredashboards.Service
	srv *Server
}

func newFixture(t *testing.T, baseURL string) *fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	pg := testutil.SetupPostgres(t)
	svc := coredashboards.NewService(pg.PgRO, pg.PgW)
	return &fixture{pg: pg, svc: svc, srv: NewServer(svc, baseURL)}
}

// newProject is a fresh project and a private key's context for it.
func (f *fixture) newProject(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx := context.Background()
	orgID, projectID := xid.New().String(), xid.New().String()
	if _, err := f.pg.PgW.Exec(ctx, `insert into orgs (id, display_name) values ($1, 'org')`, orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := f.pg.PgW.Exec(ctx, `insert into projects (id, org_id, display_name) values ($1, $2, 'proj')`, projectID, orgID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypePrivateKey,
		Project:  &dbread.Project{ID: projectID, OrgID: orgID},
	}), projectID
}

func (f *fixture) upsert(t *testing.T, ctx context.Context, req *shareddashboardsv1.UpsertRequest) *shareddashboardsv1.UpsertResponse {
	t.Helper()
	resp, err := f.srv.Upsert(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	return resp.Msg
}

func (f *fixture) get(t *testing.T, ctx context.Context, id string) *shareddashboardsv1.Dashboard {
	t.Helper()
	resp, err := f.srv.Get(ctx, connect.NewRequest(&shareddashboardsv1.GetRequest{Id: proto.String(id)}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return resp.Msg.GetDashboard()
}

func wantRefused(t *testing.T, err error, code connect.Code, reason apperr.Reason) {
	t.Helper()
	if ae, ok := errors.AsType[*apperr.Error](err); !ok || ae.Code() != code || ae.Reason() != reason {
		t.Errorf("got %v, want %v %s", err, code, reason)
	}
}

func tile(id, name string) *shareddashboardsv1.Tile {
	return &shareddashboardsv1.Tile{Id: proto.String(id), DisplayName: proto.String(name), Description: proto.String("")}
}

func TestRPCsNeedAProject(t *testing.T) {
	s := NewServer(nil, "")
	ctx := context.Background()
	_, listErr := s.List(ctx, connect.NewRequest(&shareddashboardsv1.ListRequest{}))
	_, getErr := s.Get(ctx, connect.NewRequest(&shareddashboardsv1.GetRequest{Id: proto.String("d")}))
	_, upsertErr := s.Upsert(ctx, connect.NewRequest(&shareddashboardsv1.UpsertRequest{}))
	for _, err := range []error{listErr, getErr, upsertErr} {
		wantRefused(t, err, connect.CodeUnauthenticated, apperr.ReasonUnauthenticated)
	}
}

func TestCreateThenGet(t *testing.T) {
	f := newFixture(t, "https://app.example/")
	ctx, projectID := f.newProject(t)

	created := f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{
		DisplayName:      proto.String("Growth"),
		Description:      proto.String("Weekly numbers"),
		DefaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.Enum(),
		Tiles: []*shareddashboardsv1.Tile{
			{DisplayName: proto.String("Signups"), Spec: trendsSpec("signup"), Chart: shareddashboardsv1.Chart_CHART_BAR.Enum(), Position: pos(6, 0, 6, 22)},
			{DisplayName: proto.String("Top pages"), Spec: topKSpec()},
			{DisplayName: proto.String("Checkout"), Spec: funnelSpec()},
			markdownTile("Notes"),
		},
	})
	id, ids := created.GetId(), created.GetTileIds()
	if want := "https://app.example/dashboards/" + id + "?project=" + projectID; created.GetUrl() != want {
		t.Errorf("url = %q, want %q", created.GetUrl(), want)
	}
	if len(ids) != 4 {
		t.Fatalf("tile_ids = %v, want 4", ids)
	}

	got := f.get(t, ctx, id)
	if got.GetDisplayName() != "Growth" || got.GetDescription() != "Weekly numbers" ||
		got.GetDefaultTimeRange() != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS ||
		got.GetDefaultGranularity() != insightsv1.Granularity_GRANULARITY_DAY ||
		got.GetShared() || got.GetUrl() != created.GetUrl() ||
		!got.GetUpdateTime().AsTime().Equal(created.GetUpdateTime().AsTime()) {
		t.Errorf("dashboard = %v", got)
	}

	// The call placed Signups; the rest go below it, a row at a time.
	signups, topK, checkout, notes := tile(ids[0], "Signups"), tile(ids[1], "Top pages"), tile(ids[2], "Checkout"), tile(ids[3], "")
	signups.Spec, signups.Chart, signups.Position = trendsSpec("signup"), shareddashboardsv1.Chart_CHART_BAR.Enum(), pos(6, 0, 6, 22)
	topK.Spec, topK.Chart, topK.Position = topKSpec(), shareddashboardsv1.Chart_CHART_TABLE.Enum(), pos(0, 22, 6, 24)
	checkout.Spec, checkout.Position = funnelSpec(), pos(0, 46, 12, 26)
	notes.Markdown, notes.Position = proto.String("Notes"), pos(0, 72, 4, 16)
	want := map[string]*shareddashboardsv1.Tile{ids[0]: signups, ids[1]: topK, ids[2]: checkout, ids[3]: notes}
	if len(got.GetTiles()) != len(want) {
		t.Fatalf("got %d tiles, want %d", len(got.GetTiles()), len(want))
	}
	for _, tile := range got.GetTiles() {
		if !proto.Equal(tile, want[tile.GetId()]) {
			t.Errorf("tile %s:\n got %v\nwant %v", tile.GetId(), tile, want[tile.GetId()])
		}
	}
}

func TestEditKeepsWhatItDoesNotName(t *testing.T) {
	f := newFixture(t, "")
	ctx, _ := f.newProject(t)
	created := f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{
		DisplayName:      proto.String("Growth"),
		DefaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.Enum(),
		Tiles: []*shareddashboardsv1.Tile{
			{DisplayName: proto.String("Signups"), Spec: trendsSpec("signup"), Chart: shareddashboardsv1.Chart_CHART_AREA.Enum(), Position: pos(0, 0, 6, 22)},
			{DisplayName: proto.String("Notes"), Markdown: proto.String("Old"), Position: pos(6, 0, 4, 16)},
			{DisplayName: proto.String("Gone"), Markdown: proto.String("Bye"), Position: pos(0, 22, 4, 16)},
		},
	})
	ids := created.GetTileIds()

	edited := f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{
		Id:               proto.String(created.GetId()),
		UpdateTime:       created.GetUpdateTime(),
		Description:      proto.String("Now with notes"),
		DefaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED.Enum(),
		Tiles: []*shareddashboardsv1.Tile{
			{Id: proto.String(ids[1]), Markdown: proto.String("New")},
			{DisplayName: proto.String("Added"), Markdown: proto.String("Hi")},
		},
		Remove: []string{ids[2]},
	})
	if len(edited.GetTileIds()) != 2 || edited.GetTileIds()[0] != ids[1] {
		t.Fatalf("tile_ids = %v, want [%s, <new>]", edited.GetTileIds(), ids[1])
	}

	got := f.get(t, ctx, created.GetId())
	if got.GetDisplayName() != "Growth" || got.GetDescription() != "Now with notes" ||
		got.GetDefaultTimeRange() != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS ||
		!got.GetUpdateTime().AsTime().Equal(edited.GetUpdateTime().AsTime()) {
		t.Errorf("dashboard = %v", got)
	}
	signups, notes, added := tile(ids[0], "Signups"), tile(ids[1], "Notes"), tile(edited.GetTileIds()[1], "Added")
	signups.Spec, signups.Chart, signups.Position = trendsSpec("signup"), shareddashboardsv1.Chart_CHART_AREA.Enum(), pos(0, 0, 6, 22)
	notes.Markdown, notes.Position = proto.String("New"), pos(6, 0, 4, 16)
	added.Markdown, added.Position = proto.String("Hi"), pos(0, 22, 4, 16)
	want := map[string]*shareddashboardsv1.Tile{signups.GetId(): signups, notes.GetId(): notes, added.GetId(): added}
	if len(got.GetTiles()) != len(want) {
		t.Fatalf("got %d tiles, want %d", len(got.GetTiles()), len(want))
	}
	for _, tile := range got.GetTiles() {
		if !proto.Equal(tile, want[tile.GetId()]) {
			t.Errorf("tile %s:\n got %v\nwant %v", tile.GetId(), tile, want[tile.GetId()])
		}
	}

	// The update_time an edit returns is good for the next one.
	f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{
		Id:          proto.String(created.GetId()),
		UpdateTime:  edited.GetUpdateTime(),
		DisplayName: proto.String("Growth, renamed"),
	})
}

func TestUpsertRefusals(t *testing.T) {
	f := newFixture(t, "")
	ctx, _ := f.newProject(t)
	created := f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{
		DisplayName: proto.String("Growth"),
		Tiles: []*shareddashboardsv1.Tile{
			{DisplayName: proto.String("Signups"), Spec: trendsSpec("signup"), Position: pos(0, 0, 6, 22)},
			{DisplayName: proto.String("Checkout"), Spec: funnelSpec(), Position: pos(6, 0, 6, 26)},
			{DisplayName: proto.String("Notes"), Markdown: proto.String("Notes"), Position: pos(0, 22, 4, 16)},
		},
	})
	signups, checkout, notes := created.GetTileIds()[0], created.GetTileIds()[1], created.GetTileIds()[2]
	edit := func(change func(*shareddashboardsv1.UpsertRequest)) *shareddashboardsv1.UpsertRequest {
		req := &shareddashboardsv1.UpsertRequest{Id: proto.String(created.GetId()), UpdateTime: created.GetUpdateTime()}
		change(req)
		return req
	}
	tiles := func(tiles ...*shareddashboardsv1.Tile) func(*shareddashboardsv1.UpsertRequest) {
		return func(r *shareddashboardsv1.UpsertRequest) { r.Tiles = tiles }
	}
	bar := shareddashboardsv1.Chart_CHART_BAR.Enum()

	for _, tc := range []struct {
		name   string
		req    *shareddashboardsv1.UpsertRequest
		code   connect.Code
		reason apperr.Reason
	}{
		{"stale update_time", edit(func(r *shareddashboardsv1.UpsertRequest) {
			r.UpdateTime = timestamppb.New(created.GetUpdateTime().AsTime().Add(-time.Millisecond))
		}), connect.CodeFailedPrecondition, apperr.ReasonDashboardChanged},
		{"unknown dashboard", edit(func(r *shareddashboardsv1.UpsertRequest) { r.Id = proto.String(xid.New().String()) }),
			connect.CodeNotFound, apperr.ReasonDashboardNotFound},
		{"unknown tile", edit(tiles(&shareddashboardsv1.Tile{Id: proto.String("nope"), DisplayName: proto.String("x")})),
			connect.CodeNotFound, apperr.ReasonDashboardTileNotFound},
		{"unknown tile in remove", edit(func(r *shareddashboardsv1.UpsertRequest) { r.Remove = []string{"nope"} }),
			connect.CodeNotFound, apperr.ReasonDashboardTileNotFound},
		{"markdown on an insight tile", edit(tiles(&shareddashboardsv1.Tile{Id: proto.String(signups), Markdown: proto.String("x")})),
			connect.CodeInvalidArgument, apperr.ReasonInvalidTileContent},
		{"spec on a markdown tile", edit(tiles(&shareddashboardsv1.Tile{Id: proto.String(notes), Spec: trendsSpec("signup")})),
			connect.CodeInvalidArgument, apperr.ReasonInvalidTileContent},
		{"chart on a funnel", edit(tiles(&shareddashboardsv1.Tile{Id: proto.String(checkout), Chart: bar})),
			connect.CodeInvalidArgument, apperr.ReasonInvalidTileContent},
		{"chart on a new funnel", edit(tiles(&shareddashboardsv1.Tile{Spec: funnelSpec(), Chart: bar})),
			connect.CodeInvalidArgument, apperr.ReasonInvalidTileContent},
		{"chart on markdown", edit(tiles(&shareddashboardsv1.Tile{Id: proto.String(notes), Chart: bar})),
			connect.CodeInvalidArgument, apperr.ReasonInvalidTileContent},
		{"new tile over another", edit(tiles(&shareddashboardsv1.Tile{Markdown: proto.String("x"), Position: pos(2, 10, 4, 16)})),
			connect.CodeInvalidArgument, apperr.ReasonInvalidTilePosition},
		{"tile moved over another", edit(tiles(&shareddashboardsv1.Tile{Id: proto.String(notes), Position: pos(4, 20, 4, 16)})),
			connect.CodeInvalidArgument, apperr.ReasonInvalidTilePosition},
		{"insight tile under 15 rows", edit(tiles(&shareddashboardsv1.Tile{Id: proto.String(signups), Position: pos(0, 0, 6, 14)})),
			connect.CodeInvalidArgument, apperr.ReasonInvalidTilePosition},
		{"markdown tile under 9 rows", edit(tiles(&shareddashboardsv1.Tile{Id: proto.String(notes), Position: pos(0, 22, 4, 8)})),
			connect.CodeInvalidArgument, apperr.ReasonInvalidTilePosition},
		{"tile name taken, ignoring case", edit(tiles(&shareddashboardsv1.Tile{DisplayName: proto.String("signups"), Markdown: proto.String("x")})),
			connect.CodeAlreadyExists, apperr.ReasonDashboardTileNameConflict},
		{"window too wide for minute buckets", edit(func(r *shareddashboardsv1.UpsertRequest) {
			r.DefaultGranularity = insightsv1.Granularity_GRANULARITY_MINUTE.Enum()
		}), connect.CodeInvalidArgument, apperr.ReasonInvalidInsightQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.srv.Upsert(ctx, connect.NewRequest(tc.req))
			wantRefused(t, err, tc.code, tc.reason)
		})
	}

	_, err := f.srv.Upsert(ctx, connect.NewRequest(edit(func(r *shareddashboardsv1.UpsertRequest) { r.Remove = []string{signups, "nope"} })))
	named := &errdetails.ResourceInfo{ResourceType: "dashboard_tile", ResourceName: "nope"}
	if ae, ok := errors.AsType[*apperr.Error](err); !ok || !slices.ContainsFunc(ae.Details(), func(d proto.Message) bool { return proto.Equal(d, named) }) {
		t.Errorf("unknown tile = %v, want it named in a ResourceInfo", err)
	}

	if got := f.get(t, ctx, created.GetId()); !got.GetUpdateTime().AsTime().Equal(created.GetUpdateTime().AsTime()) || len(got.GetTiles()) != 3 {
		t.Errorf("a refused call changed the dashboard: %v", got)
	}
}

// The cap holds after the merge: a call that removes a tile can add one to a
// full dashboard.
func TestTileLimitAfterMerge(t *testing.T) {
	f := newFixture(t, "")
	ctx, _ := f.newProject(t)
	full := make([]*shareddashboardsv1.Tile, coredashboards.MaxDashboardTiles)
	for i := range full {
		full[i] = markdownTile("Note")
	}
	created := f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{DisplayName: proto.String("Full"), Tiles: full})

	_, err := f.srv.Upsert(ctx, connect.NewRequest(&shareddashboardsv1.UpsertRequest{
		Id:         proto.String(created.GetId()),
		UpdateTime: created.GetUpdateTime(),
		Tiles:      []*shareddashboardsv1.Tile{markdownTile("One more")},
	}))
	wantRefused(t, err, connect.CodeInvalidArgument, apperr.ReasonDashboardTileLimit)

	f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{
		Id:         proto.String(created.GetId()),
		UpdateTime: created.GetUpdateTime(),
		Tiles:      []*shareddashboardsv1.Tile{markdownTile("One more")},
		Remove:     []string{created.GetTileIds()[0]},
	})
}

func TestSharedDashboard(t *testing.T) {
	f := newFixture(t, "")
	ctx, projectID := f.newProject(t)
	created := f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{
		DisplayName: proto.String("Public"),
		Tiles:       []*shareddashboardsv1.Tile{markdownTile("Hello")},
	})
	shared, err := f.svc.UpdateDashboard(context.Background(), projectID, created.GetId(), coredashboards.UpdateDashboardInput{
		DisplayName: "Public",
		IsPublic:    proto.Bool(true),
	})
	if err != nil || shared.Share == nil {
		t.Fatalf("UpdateDashboard: %v, share %v", err, shared.Share)
	}

	resp, err := f.srv.Get(ctx, connect.NewRequest(&shareddashboardsv1.GetRequest{Id: proto.String(created.GetId())}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !resp.Msg.GetDashboard().GetShared() {
		t.Error("shared = false, want true")
	}
	body, err := protojson.Marshal(resp.Msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), shared.Share.ShareToken) {
		t.Errorf("Get returned the share token: %s", body)
	}

	list, err := f.srv.List(ctx, connect.NewRequest(&shareddashboardsv1.ListRequest{}))
	if err != nil || len(list.Msg.GetDashboards()) != 1 || !list.Msg.GetDashboards()[0].GetShared() {
		t.Errorf("List = %v, %v; want the dashboard, shared", list, err)
	}

	_, err = f.srv.Upsert(ctx, connect.NewRequest(&shareddashboardsv1.UpsertRequest{
		Id:          proto.String(created.GetId()),
		UpdateTime:  resp.Msg.GetDashboard().GetUpdateTime(),
		DisplayName: proto.String("Renamed"),
	}))
	wantRefused(t, err, connect.CodeFailedPrecondition, apperr.ReasonDashboardShared)
}

func TestList(t *testing.T) {
	f := newFixture(t, "")
	ctx, _ := f.newProject(t)
	created := f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{
		DisplayName: proto.String("Growth"),
		Description: proto.String("Weekly numbers"),
		Tiles:       []*shareddashboardsv1.Tile{markdownTile("One"), markdownTile("Two")},
	})

	resp, err := f.srv.List(ctx, connect.NewRequest(&shareddashboardsv1.ListRequest{}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := &shareddashboardsv1.DashboardSummary{
		Id:          proto.String(created.GetId()),
		DisplayName: proto.String("Growth"),
		Description: proto.String("Weekly numbers"),
		TileCount:   proto.Int32(2),
		Shared:      proto.Bool(false),
		UpdateTime:  created.GetUpdateTime(),
	}
	if got := resp.Msg.GetDashboards(); len(got) != 1 || !proto.Equal(got[0], want) {
		t.Errorf("List = %v, want [%v]", got, want)
	}
}

// A key reaches its own project's dashboards and no other's.
func TestOtherProjectsDashboardsAreNotFound(t *testing.T) {
	f := newFixture(t, "")
	ours, _ := f.newProject(t)
	theirs, _ := f.newProject(t)
	created := f.upsert(t, theirs, &shareddashboardsv1.UpsertRequest{DisplayName: proto.String("Theirs")})

	_, err := f.srv.Get(ours, connect.NewRequest(&shareddashboardsv1.GetRequest{Id: proto.String(created.GetId())}))
	wantRefused(t, err, connect.CodeNotFound, apperr.ReasonDashboardNotFound)
	_, err = f.srv.Upsert(ours, connect.NewRequest(&shareddashboardsv1.UpsertRequest{
		Id:          proto.String(created.GetId()),
		UpdateTime:  created.GetUpdateTime(),
		DisplayName: proto.String("Ours now"),
	}))
	wantRefused(t, err, connect.CodeNotFound, apperr.ReasonDashboardNotFound)
	if list, err := f.srv.List(ours, connect.NewRequest(&shareddashboardsv1.ListRequest{})); err != nil || len(list.Msg.GetDashboards()) != 0 {
		t.Errorf("List = %v, %v; want none", list, err)
	}
}

func TestNoLinkWithoutBaseURL(t *testing.T) {
	f := newFixture(t, "")
	ctx, _ := f.newProject(t)
	created := f.upsert(t, ctx, &shareddashboardsv1.UpsertRequest{DisplayName: proto.String("Growth")})
	if created.Url != nil {
		t.Errorf("upsert url = %q, want none", created.GetUrl())
	}
	if got := f.get(t, ctx, created.GetId()); got.Url != nil {
		t.Errorf("get url = %q, want none", got.GetUrl())
	}
}

// Positions the app stored read back as the app draws them on 12 columns, kept
// inside Position's rules.
func TestStoredPositionsReadAsTheAppShowsThem(t *testing.T) {
	f := newFixture(t, "")
	ctx, projectID := f.newProject(t)
	board, err := f.svc.CreateDashboard(context.Background(), projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	stored := map[string]*dashboardsv1.GridPosition{
		"half a column in": gridPos(3, 5, 9, 20),
		"between columns":  gridPos(40, 0, 20, 16),
		"past the edge":    gridPos(70, 0, 10, 16),
		"under its floor":  gridPos(0, 40, 24, 5),
		"far down":         gridPos(0, 250000, 24, 16),
		"near the limit":   gridPos(2147483647, 0, 72, 16),
		"none":             nil,
	}
	want := map[string]*shareddashboardsv1.Position{
		"half a column in":      pos(1, 5, 2, 20),
		"between columns":       pos(7, 0, 3, 16),
		"past the edge":         pos(10, 0, 2, 16),
		"under its floor":       pos(0, 40, 4, 9),
		"far down":              pos(0, 100000, 4, 16),
		"near the limit":        pos(10, 0, 2, 16),
		"none":                  pos(0, 0, 6, 18),
		"chart under its floor": pos(4, 40, 6, 15),
	}
	var in coredashboards.UpsertDashboardInput
	in.DisplayName = "Board"
	for name, p := range stored {
		in.Tiles = append(in.Tiles, coredashboards.UpsertTileInput{Payload: coredashboards.TilePayload{
			DisplayName: name, Content: coredashboards.MarkdownTile{Body: name}, Position: p,
		}})
	}
	in.Tiles = append(in.Tiles, coredashboards.UpsertTileInput{Payload: coredashboards.TilePayload{
		DisplayName: "chart under its floor", Content: coredashboards.InsightTile{Spec: trendsSpec("signup")}, Position: gridPos(24, 40, 36, 10),
	}})
	if _, err := f.svc.UpsertDashboard(context.Background(), projectID, board.ID, in); err != nil {
		t.Fatalf("UpsertDashboard: %v", err)
	}

	for _, tile := range f.get(t, ctx, board.ID).GetTiles() {
		if w := want[tile.GetDisplayName()]; !proto.Equal(tile.GetPosition(), w) {
			t.Errorf("%s: position %v, want %v", tile.GetDisplayName(), tile.GetPosition(), w)
		}
		if err := protovalidate.Validate(tile.GetPosition()); err != nil {
			t.Errorf("%s: position %v can't be sent back: %v", tile.GetDisplayName(), tile.GetPosition(), err)
		}
	}
}

func gridPos(x, y, w, h int32) *dashboardsv1.GridPosition {
	return &dashboardsv1.GridPosition{X: proto.Int32(x), Y: proto.Int32(y), W: proto.Int32(w), H: proto.Int32(h)}
}

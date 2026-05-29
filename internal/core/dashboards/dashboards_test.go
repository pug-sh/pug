package dashboards_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/core/dashboards"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func TestEncodeTileContent_Insight(t *testing.T) {
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
		},
	}

	enc, err := dashboards.InsightTile{Spec: spec}.Encode()
	if err != nil {
		t.Fatalf("EncodeTileContent insight: %v", err)
	}
	if enc.Kind != dashboards.TileKindInsight {
		t.Errorf("Kind = %d, want %d", enc.Kind, dashboards.TileKindInsight)
	}
	if enc.InsightQuery == nil {
		t.Error("InsightQuery = nil, want non-nil")
	}
	if enc.MarkdownBody.Valid {
		t.Error("MarkdownBody.Valid = true, want false (SQL NULL)")
	}
	// The tile stores an InsightQuerySpec directly, so insightType is top-level.
	if enc.InsightQuery["insightType"] != "INSIGHT_TYPE_TRENDS" {
		t.Errorf("InsightQuery[insightType] = %v, want INSIGHT_TYPE_TRENDS", enc.InsightQuery["insightType"])
	}
}

func TestEncodeTileContent_Markdown(t *testing.T) {
	body := "# Heading\n\nSome text with an image: ![alt](https://example.com/img.png)"

	enc, err := dashboards.MarkdownTile{Body: body}.Encode()
	if err != nil {
		t.Fatalf("EncodeTileContent markdown: %v", err)
	}
	if enc.Kind != dashboards.TileKindMarkdown {
		t.Errorf("Kind = %d, want %d", enc.Kind, dashboards.TileKindMarkdown)
	}
	if enc.InsightQuery != nil {
		t.Error("InsightQuery != nil, want nil (SQL NULL)")
	}
	if !enc.MarkdownBody.Valid {
		t.Fatal("MarkdownBody.Valid = false, want true")
	}
	if enc.MarkdownBody.String != body {
		t.Errorf("MarkdownBody.String = %q, want %q", enc.MarkdownBody.String, body)
	}
}

func TestEncodeTileContent_EmptyMarkdown(t *testing.T) {
	// Empty markdown body is unreachable via the RPC path (proto enforces min_len: 1),
	// but EncodeTileContent must encode the empty string verbatim with Valid=true.
	// MarkdownTile{Body: ""} is structurally distinct from no markdown content at all
	// (which is unrepresentable in the sealed TileContent type system).
	enc, err := dashboards.MarkdownTile{Body: ""}.Encode()
	if err != nil {
		t.Fatalf("MarkdownTile.Encode empty body: %v", err)
	}
	if enc.Kind != dashboards.TileKindMarkdown {
		t.Errorf("Kind = %d, want %d", enc.Kind, dashboards.TileKindMarkdown)
	}
	if enc.InsightQuery != nil {
		t.Errorf("InsightQuery = %v, want nil", enc.InsightQuery)
	}
	if !enc.MarkdownBody.Valid {
		t.Fatal("MarkdownBody.Valid = false, want true")
	}
	if enc.MarkdownBody.String != "" {
		t.Errorf("MarkdownBody.String = %q, want empty string", enc.MarkdownBody.String)
	}
}

package segments

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/proto/segments/v1"
)

type Repo interface {
	CreateSegment(ctx context.Context, projectID, name, description string, filter *segmentsv1.SegmentFilter) (*segmentsv1.Segment, error)
	GetSegment(ctx context.Context, segmentID string) (*segmentsv1.Segment, error)
	ListSegments(ctx context.Context, projectID string, limit, offset int32) ([]*segmentsv1.Segment, int32, error)
	UpdateSegment(ctx context.Context, segmentID, name, description string, filter *segmentsv1.SegmentFilter, isActive bool) (*segmentsv1.Segment, error)
	DeleteSegment(ctx context.Context, segmentID string) error
	GetActiveSegments(ctx context.Context, projectID string) ([]*segmentsv1.Segment, error)
	EvaluateSegment(ctx context.Context, segmentID, userExternalID string) (bool, error)
}

package segments

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/core/segments"
	segmentsv1 "github.com/fivebitsio/cotton/internal/gen/proto/segments/v1"
)

type Handler struct {
	repo segments.Repo
}

func NewHandler(repo segments.Repo) *Handler {
	return &Handler{
		repo: repo,
	}
}

func (h *Handler) CreateSegment(ctx context.Context, req *connect.Request[segmentsv1.CreateSegmentRequest]) (*connect.Response[segmentsv1.CreateSegmentResponse], error) {
	if req.Msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id is required"))
	}
	if req.Msg.Name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}

	segment, err := h.repo.CreateSegment(ctx, req.Msg.ProjectId, req.Msg.Name, req.Msg.Description, req.Msg.Filter)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to create segment: %w", err))
	}

	return connect.NewResponse(&segmentsv1.CreateSegmentResponse{
		Segment: segment,
	}), nil
}

func (h *Handler) GetSegment(ctx context.Context, req *connect.Request[segmentsv1.GetSegmentRequest]) (*connect.Response[segmentsv1.GetSegmentResponse], error) {
	if req.Msg.SegmentId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("segment_id is required"))
	}

	segment, err := h.repo.GetSegment(ctx, req.Msg.SegmentId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to get segment: %w", err))
	}

	return connect.NewResponse(&segmentsv1.GetSegmentResponse{
		Segment: segment,
	}), nil
}

func (h *Handler) ListSegments(ctx context.Context, req *connect.Request[segmentsv1.ListSegmentsRequest]) (*connect.Response[segmentsv1.ListSegmentsResponse], error) {
	if req.Msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id is required"))
	}

	segments, totalCount, err := h.repo.ListSegments(ctx, req.Msg.ProjectId, req.Msg.Limit, req.Msg.Offset)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to list segments: %w", err))
	}

	return connect.NewResponse(&segmentsv1.ListSegmentsResponse{
		Segments:   segments,
		TotalCount: totalCount,
	}), nil
}

func (h *Handler) UpdateSegment(ctx context.Context, req *connect.Request[segmentsv1.UpdateSegmentRequest]) (*connect.Response[segmentsv1.UpdateSegmentResponse], error) {
	if req.Msg.SegmentId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("segment_id is required"))
	}

	segment, err := h.repo.UpdateSegment(ctx, req.Msg.SegmentId, req.Msg.Name, req.Msg.Description, req.Msg.Filter, req.Msg.IsActive)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to update segment: %w", err))
	}

	return connect.NewResponse(&segmentsv1.UpdateSegmentResponse{
		Segment: segment,
	}), nil
}

func (h *Handler) DeleteSegment(ctx context.Context, req *connect.Request[segmentsv1.DeleteSegmentRequest]) (*connect.Response[segmentsv1.DeleteSegmentResponse], error) {
	if req.Msg.SegmentId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("segment_id is required"))
	}

	err := h.repo.DeleteSegment(ctx, req.Msg.SegmentId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to delete segment: %w", err))
	}

	return connect.NewResponse(&segmentsv1.DeleteSegmentResponse{
		Success: true,
	}), nil
}

func (h *Handler) EvaluateSegment(ctx context.Context, req *connect.Request[segmentsv1.EvaluateSegmentRequest]) (*connect.Response[segmentsv1.EvaluateSegmentResponse], error) {
	if req.Msg.SegmentId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("segment_id is required"))
	}
	if req.Msg.UserExternalId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("user_external_id is required"))
	}

	matches, err := h.repo.EvaluateSegment(ctx, req.Msg.SegmentId, req.Msg.UserExternalId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to evaluate segment: %w", err))
	}

	return connect.NewResponse(&segmentsv1.EvaluateSegmentResponse{
		Matches: matches,
	}), nil
}
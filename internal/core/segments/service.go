package segments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivebitsio/cotton/internal/gen/proto/segments/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/xid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Use pgtype to avoid "imported but not used" error (used in generated code)
var _ = pgtype.Text{}

type Service struct {
	dbWrite *dbwrite.Queries
	dbRead  *dbread.Queries
}

func NewService(dbWrite *dbwrite.Queries, dbRead *dbread.Queries) *Service {
	return &Service{
		dbWrite: dbWrite,
		dbRead:  dbRead,
	}
}

func (s *Service) CreateSegment(ctx context.Context, projectID, name, description string, filter *segmentsv1.SegmentFilter) (*segmentsv1.Segment, error) {
	if name == "" {
		return nil, errors.New("segment name is required")
	}

	id := xid.New().String()

	// Marshal the filter to JSON
	filterJSON, err := json.Marshal(filter)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal filter: %w", err)
	}

	segment, err := s.dbWrite.CreateSegment(ctx, dbwrite.CreateSegmentParams{
		ID:          id,
		ProjectID:   projectID,
		Name:        name,
		Description: postgres.NullString(description),
		Filter:      filterJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create segment: %w", err)
	}

	return &segmentsv1.Segment{
		Id:          segment.ID,
		ProjectId:   segment.ProjectID,
		Name:        segment.Name,
		Description: segment.Description.String,
		Filter:      unmarshalSegmentFilter(segment.Filter),
		IsActive:    segment.IsActive,
		CreateTime:  timestamppb.New(segment.CreateTime.Time),
		UpdateTime:  timestamppb.New(segment.UpdateTime.Time),
	}, nil
}

func (s *Service) GetSegment(ctx context.Context, segmentID string) (*segmentsv1.Segment, error) {
	segment, err := s.dbRead.GetSegment(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("failed to get segment: %w", err)
	}

	return &segmentsv1.Segment{
		Id:          segment.ID,
		ProjectId:   segment.ProjectID,
		Name:        segment.Name,
		Description: segment.Description.String,
		Filter:      unmarshalSegmentFilter(segment.Filter),
		IsActive:    segment.IsActive,
		CreateTime:  timestamppb.New(segment.CreateTime.Time),
		UpdateTime:  timestamppb.New(segment.UpdateTime.Time),
	}, nil
}

func (s *Service) ListSegments(ctx context.Context, projectID string, limit, offset int32) ([]*segmentsv1.Segment, int32, error) {
	segments, err := s.dbRead.GetSegmentsByProject(ctx, dbread.GetSegmentsByProjectParams{
		ProjectID: projectID,
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list segments: %w", err)
	}

	count, err := s.dbRead.GetSegmentCountByProject(ctx, projectID)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get segment count: %w", err)
	}

	result := make([]*segmentsv1.Segment, len(segments))
	for i, seg := range segments {
		result[i] = &segmentsv1.Segment{
			Id:          seg.ID,
			ProjectId:   seg.ProjectID,
			Name:        seg.Name,
			Description: seg.Description.String,
			Filter:      unmarshalSegmentFilter(seg.Filter),
			IsActive:    seg.IsActive,
			CreateTime:  timestamppb.New(seg.CreateTime.Time),
			UpdateTime:  timestamppb.New(seg.UpdateTime.Time),
		}
	}

	return result, int32(count), nil
}

func (s *Service) UpdateSegment(ctx context.Context, segmentID, name, description string, filter *segmentsv1.SegmentFilter, isActive bool) (*segmentsv1.Segment, error) {
	// For now, we'll use a workaround where if a field should not be updated,
	// the calling code would need to pass the existing value.
	// In a real implementation, we would ideally have more granular update methods
	// or use a different approach for partial updates.

	// Marshal the filter to JSON
	var filterJSON []byte
	if filter != nil {
		var err error
		filterJSON, err = json.Marshal(filter)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal filter: %w", err)
		}
	} else {
		// If filter is nil, we'll pass an empty slice
		filterJSON = []byte{}
	}

	// The calling code should ensure that non-updated fields contain their current values
	// The SQL COALESCE will then keep the current value if a NULL is passed
	segment, err := s.dbWrite.UpdateSegment(ctx, dbwrite.UpdateSegmentParams{
		ID:          segmentID,
		Name:        name,
		Description: postgres.NullString(description),
		Filter:      filterJSON,
		IsActive:    isActive,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update segment: %w", err)
	}

	return &segmentsv1.Segment{
		Id:          segment.ID,
		ProjectId:   segment.ProjectID,
		Name:        segment.Name,
		Description: segment.Description.String,
		Filter:      unmarshalSegmentFilter(segment.Filter),
		IsActive:    segment.IsActive,
		CreateTime:  timestamppb.New(segment.CreateTime.Time),
		UpdateTime:  timestamppb.New(segment.UpdateTime.Time),
	}, nil
}

func (s *Service) DeleteSegment(ctx context.Context, segmentID string) error {
	err := s.dbWrite.DeleteSegment(ctx, segmentID)
	if err != nil {
		return fmt.Errorf("failed to delete segment: %w", err)
	}
	return nil
}

func (s *Service) GetActiveSegments(ctx context.Context, projectID string) ([]*segmentsv1.Segment, error) {
	segments, err := s.dbRead.GetActiveSegments(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to get active segments: %w", err)
	}

	result := make([]*segmentsv1.Segment, len(segments))
	for i, seg := range segments {
		result[i] = &segmentsv1.Segment{
			Id:          seg.ID,
			ProjectId:   seg.ProjectID,
			Name:        seg.Name,
			Description: seg.Description.String,
			Filter:      unmarshalSegmentFilter(seg.Filter),
			IsActive:    seg.IsActive,
			CreateTime:  timestamppb.New(seg.CreateTime.Time),
			UpdateTime:  timestamppb.New(seg.UpdateTime.Time),
		}
	}

	return result, nil
}

func (s *Service) EvaluateSegment(ctx context.Context, segmentID, userExternalID string) (bool, error) {
	// Get the segment definition
	segment, err := s.dbRead.GetSegment(ctx, segmentID)
	if err != nil {
		return false, fmt.Errorf("failed to get segment: %w", err)
	}

	// Get the user
	user, err := s.dbRead.GetUserByProjectAndExternalID(ctx, dbread.GetUserByProjectAndExternalIDParams{
		ExternalID: userExternalID,
		ProjectID:  segment.ProjectID,
	})
	if err != nil {
		return false, fmt.Errorf("failed to get user: %w", err)
	}

	// Unmarshal user metadata
	var userMetadata map[string]interface{}
	if err := json.Unmarshal(user.Metadata, &userMetadata); err != nil {
		return false, fmt.Errorf("failed to unmarshal user metadata: %w", err)
	}

	// Unmarshal the segment filter
	segmentFilter := unmarshalSegmentFilter(segment.Filter)

	// Evaluate the segment conditions against the user's metadata
	matches := s.evaluateConditions(segmentFilter, userMetadata)
	return matches, nil
}

// evaluateConditions evaluates if a user's metadata matches the segment filter
func (s *Service) evaluateConditions(filter *segmentsv1.SegmentFilter, metadata map[string]interface{}) bool {
	if len(filter.Parts) == 0 {
		return true // No parts, matches by default
	}

	results := make([]bool, len(filter.Parts))
	for i, part := range filter.Parts {
		results[i] = s.evaluateFilterPart(part, metadata)
	}

	// Apply logical operator
	if filter.LogicalOperator == "OR" {
		for _, result := range results {
			if result {
				return true
			}
		}
		return false
	} else { // Default to AND
		for _, result := range results {
			if !result {
				return false
			}
		}
		return true
	}
}

// evaluateFilterPart evaluates a single filter part (either a sub-filter or a condition)
func (s *Service) evaluateFilterPart(filterPart *segmentsv1.FilterPart, metadata map[string]interface{}) bool {
	switch part := filterPart.Part.(type) {
	case *segmentsv1.FilterPart_SubFilter:
		return s.evaluateConditions(part.SubFilter, metadata) // Recursively evaluate nested filter
	case *segmentsv1.FilterPart_Condition:
		return s.evaluateCondition(part.Condition, metadata) // Evaluate simple condition
	default:
		return false // Should not happen
	}
}

// evaluateCondition checks if a single condition is satisfied by user metadata or events
func (s *Service) evaluateCondition(condition *segmentsv1.Condition, metadata map[string]interface{}) bool {
	switch cond := condition.ConditionType.(type) {
	case *segmentsv1.Condition_UserAttributeCondition:
		return s.evaluateUserAttributeCondition(cond.UserAttributeCondition, metadata)
	case *segmentsv1.Condition_EventCondition:
		return s.evaluateEventCondition(cond.EventCondition, metadata)
	default:
		return false // Unknown condition type
	}
}

// evaluateUserAttributeCondition checks if a user attribute condition is satisfied by user metadata
func (s *Service) evaluateUserAttributeCondition(condition *segmentsv1.UserAttributeCondition, metadata map[string]interface{}) bool {
	fieldValue, exists := metadata[condition.Field]
	if !exists {
		return false
	}

	switch condition.Operator {
	case "EQUALS":
		return fmt.Sprintf("%v", fieldValue) == condition.Value
	case "NOT_EQUALS":
		return fmt.Sprintf("%v", fieldValue) != condition.Value
	case "CONTAINS":
		// For string contains check
		fieldStr := fmt.Sprintf("%v", fieldValue)
		return containsString(fieldStr, condition.Value)
	case "NOT_CONTAINS":
		// For string does not contain check
		fieldStr := fmt.Sprintf("%v", fieldValue)
		return !containsString(fieldStr, condition.Value)
	case "GREATER_THAN":
		// For numeric comparison
		return compareNumeric(fieldValue, condition.Value, func(a, b float64) bool { return a > b })
	case "LESS_THAN":
		// For numeric comparison
		return compareNumeric(fieldValue, condition.Value, func(a, b float64) bool { return a < b })
	}

	return false
}

// evaluateEventCondition checks if a user has performed an event that meets the condition
func (s *Service) evaluateEventCondition(condition *segmentsv1.EventCondition, metadata map[string]interface{}) bool {
	// Get the user ID from the metadata (we need to extract the user ID from metadata)
	// In a real scenario, we'd likely pass the user ID separately, but for this example,
	// we'll assume it's available in the metadata

	// We need to query the events table to see if the user has performed the required event
	// within the specified timeframe and with the required properties/count

	// This is a simplified implementation - in a real system, we would:
	// 1. Query the ClickHouse events table for the user
	// 2. Filter by event_type, time window, and properties as specified in the condition
	// 3. Check if the count meets the condition requirements

	// For now, this function would need to connect to the events table in ClickHouse
	// and perform the necessary query based on the condition parameters

	return s.checkEventConditionInDatabase(condition, metadata)
}

// checkEventConditionInDatabase performs the actual database query to check if user meets event-based condition
func (s *Service) checkEventConditionInDatabase(condition *segmentsv1.EventCondition, metadata map[string]interface{}) bool {
	// This function would need access to the ClickHouse database connection
	// Since we don't have that setup in this file, this is a placeholder
	// showing the logic that would be implemented

	// In a real implementation, this would:
	// 1. Query the events table for events matching the condition
	// 2. Filter by user_id, event_type, and time window
	// 3. Check for property conditions if specified
	// 4. Count the results and compare with the required count

	// For now, we return false since we don't have access to the events DB here
	// In a real implementation, we would add a method to our repo to query events
	return false
}

// Helper function to check if a string contains a substring
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || strings.Contains(s, substr))
}

// Helper function to compare numeric values
func compareNumeric(a, b interface{}, compareFunc func(float64, float64) bool) bool {
	aFloat, aOk := toFloat64(a)
	bFloat, bOk := toFloat64(b)

	if !aOk || !bOk {
		return false
	}

	return compareFunc(aFloat, bFloat)
}

// Helper function to convert interface{} to float64
func toFloat64(val interface{}) (float64, bool) {
	switch v := val.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// Helper function to unmarshal segment filter from JSON
func unmarshalSegmentFilter(data []byte) *segmentsv1.SegmentFilter {
	var filter segmentsv1.SegmentFilter
	if err := json.Unmarshal(data, &filter); err != nil {
		// Return an empty filter if unmarshaling fails
		return &segmentsv1.SegmentFilter{}
	}
	return &filter
}

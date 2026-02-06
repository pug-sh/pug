package events

import (
	"fmt"
	"strings"

	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
)

const (
	reservedPrefix = "cotton."
	MaxBatchSize   = 1000
)

// ValidateExternalEvents checks that SDK-submitted events have required fields
// (event name and distinct_id), don't exceed the batch size limit, and don't
// use the reserved "cotton." name prefix.
func ValidateExternalEvents(events []*eventsv1.Event) error {
	if len(events) > MaxBatchSize {
		return fmt.Errorf("batch size %d exceeds maximum of %d", len(events), MaxBatchSize)
	}
	for i, e := range events {
		if e.Event == "" {
			return fmt.Errorf("event[%d]: event name is required", i)
		}
		if e.DistinctId == "" {
			return fmt.Errorf("event[%d]: distinct_id is required", i)
		}
		if strings.HasPrefix(e.Event, reservedPrefix) {
			return fmt.Errorf("event[%d]: event name %q uses reserved prefix %q", i, e.Event, reservedPrefix)
		}
	}
	return nil
}

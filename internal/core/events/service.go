package events

import (
	"fmt"

	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
)

// ValidateExternalEvents validates batch-level constraints across the repeated
// events field that proto CEL rules cannot express (CEL rules on repeated fields
// evaluate per-element, not across elements): no duplicate event IDs within a single batch.
func ValidateExternalEvents(events []*eventsv1.Event) error {
	seen := make(map[string]struct{}, len(events))
	for i, e := range events {
		if _, exists := seen[e.GetEventId()]; exists {
			return fmt.Errorf("event[%d]: duplicate event_id %q in batch", i, e.GetEventId())
		}
		seen[e.GetEventId()] = struct{}{}
	}
	return nil
}

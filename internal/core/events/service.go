package events

import (
	"fmt"
	"strings"

	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/events/v1"
)

const reservedPrefix = "cotton."

// ValidateExternalEvents validates SDK-submitted events: no duplicate event IDs,
// no reserved "cotton." kind prefix, and all auto_properties keys must start with '$'.
func ValidateExternalEvents(events []*eventsv1.Event) error {
	seen := make(map[string]struct{}, len(events))
	for i, e := range events {
		if _, exists := seen[e.EventId]; exists {
			return fmt.Errorf("event[%d]: duplicate event_id %q in batch", i, e.EventId)
		}
		seen[e.EventId] = struct{}{}
		if strings.HasPrefix(e.Kind, reservedPrefix) {
			return fmt.Errorf("event[%d]: kind %q uses reserved prefix %q", i, e.Kind, reservedPrefix)
		}
		for k := range e.AutoProperties {
			if !strings.HasPrefix(k, "$") {
				return fmt.Errorf("event[%d]: auto_properties key %q must start with '$'", i, k)
			}
		}
	}
	return nil
}

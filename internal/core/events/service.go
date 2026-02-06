package events

import (
	"fmt"
	"strings"

	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
)

const reservedPrefix = "cotton."

// ValidateExternalEvents checks that SDK-submitted events don't use reserved names.
func ValidateExternalEvents(events []*eventsv1.Event) error {
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

package events

import (
	"fmt"
	"strings"

	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
)

const reservedPrefix = "cotton."

// ValidateExternalEvents checks that SDK-submitted events don't use the
// reserved "cotton." name prefix.
func ValidateExternalEvents(events []*eventsv1.Event) error {
	for i, e := range events {
		if strings.HasPrefix(e.Kind, reservedPrefix) {
			return fmt.Errorf("event[%d]: kind %q uses reserved prefix %q", i, e.Kind, reservedPrefix)
		}
	}
	return nil
}

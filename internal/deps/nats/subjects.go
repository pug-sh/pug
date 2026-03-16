package nats

// Subject constants for NATS publishing
const (
	// Device subjects
	DeviceOpsSubject = "devices.ops"

	// Profile subjects
	ProfileRegisterSubject = "profiles.register"
	ProfileIdentifySubject = "profiles.identify"
	ProfileAliasSubject    = "profiles.alias"

	// Campaign subjects
	CampaignScheduledSubject = "campaigns.scheduled"

	// Delivery subjects
	DeliveryEventsSubject = "deliveries.events"

	// Events subjects
	EventsIngestSubject = "events.ingest"

	// Dead letter queue subjects — mirror the ingest subject hierarchy.
	// Subscribe to "dlq.>" for all DLQ messages, or "dlq.profiles.>" for a domain.
	DLQDevicesSubject         = "dlq.devices.ops"
	DLQCampaignsSubject       = "dlq.campaigns.scheduled"
	DLQDeliveriesSubject      = "dlq.deliveries.events"
	DLQEventsSubject          = "dlq.events.ingest"
	DLQProfilesRegisterSubject = "dlq.profiles.register"
	DLQProfilesIdentifySubject = "dlq.profiles.identify"
	DLQProfilesAliasSubject   = "dlq.profiles.alias"
)

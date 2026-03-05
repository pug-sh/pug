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

	// Dead letter queue subjects
	DLQDevicesSubject    = "dlq.devices"
	DLQCampaignsSubject  = "dlq.campaigns"
	DLQDeliveriesSubject = "dlq.deliveries"
	DLQEventsSubject     = "dlq.events"
	DLQProfilesSubject   = "dlq.profiles"
)

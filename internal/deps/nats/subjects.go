package nats

// Subject constants for NATS publishing
const (
	// Device subjects
	DeviceOpsSubject = "devices.ops"

	// Profile subjects
	ProfileIdentifySubject = "profiles.identify"
	ProfileAliasSubject    = "profiles.alias"
	ProfileUpsertSubject   = "profiles.upsert"

	// Compliance subjects (GDPR/DPDP erasure, export, retention)
	ComplianceEraseSubject = "compliance.erase"

	// Campaign subjects
	CampaignScheduledSubject = "campaigns.scheduled"

	// Delivery subjects
	DeliveryEventsSubject = "deliveries.events"

	// Events subjects
	EventsIngestSubject = "events.ingest"

	// Misc subjects
	MiscEmailJobsSubject = "misc.email.jobs"

	// Dead letter queue subjects — mirror the ingest subject hierarchy.
	// Subscribe to "dlq.>" for all DLQ messages, or "dlq.profiles.>" for a domain.
	DLQDevicesSubject          = "dlq.devices.ops"
	DLQCampaignsSubject        = "dlq.campaigns.scheduled"
	DLQDeliveriesSubject       = "dlq.deliveries.events"
	DLQEventsSubject           = "dlq.events.ingest"
	DLQMiscEmailSubject        = "dlq.misc.email.jobs"
	DLQProfilesIdentifySubject = "dlq.profiles.identify"
	DLQProfilesAliasSubject    = "dlq.profiles.alias"
	DLQProfilesUpsertSubject   = "dlq.profiles.upsert"
	DLQComplianceEraseSubject  = "dlq.compliance.erase"
)

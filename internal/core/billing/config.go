package billing

// Config is the billing switch, and the default retention while it is off.
// Off means every org resolves with no allowance at all, so no client can render a
// limit that does not apply. Declared here, not in the server, so `pug billing` can
// read it without importing the server.
type Config struct {
	Enabled bool `env:"PUG_BILLING_ENABLED,default=false"`
	// RetentionDays is the default length with billing off; an org's override wins
	// (docs/architecture/data-retention.md). 0 keeps everything.
	RetentionDays int64 `env:"PUG_RETENTION_DAYS"`
}

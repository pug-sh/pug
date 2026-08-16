package billing

// Config is the whole billing switch. Off means every org resolves with no quota
// at all, so no client can render a limit that does not apply. Declared here, not
// in the server, so `pug billing` can read it without importing the server.
type Config struct {
	Enabled bool `env:"PUG_BILLING_ENABLED,default=false"`
}

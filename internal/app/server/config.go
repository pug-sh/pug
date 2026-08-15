package server

type config struct {
	Port        string `env:"PUG_SERVER_PORT,default=3000"`
	JWTKey      string `env:"PUG_JWT_SECRET_KEY,required"`
	CORSOrigins string `env:"PUG_CORS_ORIGINS,default=*"`
	// DemoEnabled mirrors the demo worker's PUG_DEMO_ENABLED switch: when true,
	// the server exposes the credential-less AuthService.DemoSignIn viewer login.
	// Off everywhere else so the demo login can't be minted on a real deployment.
	DemoEnabled bool `env:"PUG_DEMO_ENABLED,default=false"`
	Billing     BillingConfig
}

// BillingConfig is the whole billing switch. Off (a self-hosted install) means
// GetBillingStatus reports billing_enabled=false and no quota at all, so no
// client can render a limit that does not apply. Set it on every pod of a billed
// deployment.
//
// Exported on its own so `pug billing` reports what the server would resolve by
// reading this declaration, rather than restating the variable's name and
// drifting from it.
type BillingConfig struct {
	Enabled bool `env:"PUG_BILLING_ENABLED,default=false"`
}

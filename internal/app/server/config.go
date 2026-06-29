package server

type config struct {
	Port        string `env:"PUG_SERVER_PORT,default=3000"`
	JWTKey      string `env:"PUG_JWT_SECRET_KEY,required"`
	CORSOrigins string `env:"PUG_CORS_ORIGINS,default=*"`
	// DemoEnabled mirrors the demo worker's PUG_DEMO_ENABLED switch: when true,
	// the server exposes the credential-less AuthService.DemoSignIn viewer login.
	// Off everywhere else so the demo login can't be minted on a real deployment.
	DemoEnabled bool `env:"PUG_DEMO_ENABLED,default=false"`
}

package ai

// config is the ai role's env contract. MODEL_AGENT and the per-provider API
// key envs are deliberately NOT here: their format (provider:model?opts, and
// which key env applies) is owned by the assistant.Route parser, matching the
// TS service. REDIS_URL rides internal/deps/redis.Config.
type config struct {
	Port string `env:"PUG_AI_PORT,default=8001"`
	// APIBaseURL is the main pug Connect API the insight tools call back into
	// with the caller's forwarded JWT.
	APIBaseURL string `env:"PUG_API_BASE_URL,required"`
	// JWTKey enables the local spend gate: verify the caller's JWT before any
	// model spend. Same secret as the server; NOT the security boundary (the
	// insights callback re-authorizes every read).
	JWTKey      string `env:"PUG_JWT_SECRET_KEY,required"`
	CORSOrigins string `env:"PUG_CORS_ORIGINS,default=*"`
}

package server

type config struct {
	Port        string `env:"PUG_SERVER_PORT,default=3000"`
	JWTKey      string `env:"PUG_JWT_SECRET_KEY,required"`
	CORSOrigins string `env:"PUG_CORS_ORIGINS,default=*"`
}

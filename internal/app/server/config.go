package server

type config struct {
	Port        string `env:"COTTON_SERVER_PORT,default=3000"`
	JWTKey      string `env:"COTTON_JWT_SECRET_KEY,required"`
	CORSOrigins string `env:"COTTON_CORS_ORIGINS,default=*"`
}

package postgres

type Config struct {
	URL string `env:"DATABASE_URL,required"`
}

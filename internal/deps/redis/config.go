package redis

type Config struct {
	URL string `env:"REDIS_URL,required"`
}

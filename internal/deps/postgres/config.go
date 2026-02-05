package postgres

type Config struct {
	URL string `env:"DATABASE_URL,required"`
}

func (c *Config) ConnectionString() string {
	if c == nil {
		return ""
	}
	return c.URL
}

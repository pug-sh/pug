package clickhouse

type Config struct {
	URL string `env:"CLICKHOUSE_URL,required"`
}

func (c *Config) DSN() string {
	if c == nil {
		return ""
	}
	return c.URL
}

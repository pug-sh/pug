package clickhouse

import (
	"fmt"
	"net/url"
)

type Config struct {
	Host     string `env:"CLICKHOUSE_HOST,required"`
	Port     string `env:"CLICKHOUSE_PORT,required"`
	Database string `env:"CLICKHOUSE_DATABASE,required"`
	Username string `env:"CLICKHOUSE_USERNAME,required"`
	Password string `env:"CLICKHOUSE_PASSWORD,required"`
	SSL      bool   `env:"CLICKHOUSE_SSL"`
	Timeout  int    `env:"CLICKHOUSE_TIMEOUT"`
}

func (c *Config) DatabaseConfig() *Config {
	return c
}

func (c *Config) ConnectionString() string {
	if c == nil {
		return ""
	}

	scheme := "clickhouse"
	if c.SSL {
		scheme = "clickhouses"
	}

	host := c.Host
	if v := c.Port; v != "" {
		host = host + ":" + v
	}

	u := &url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   c.Database,
	}

	if c.Username != "" || c.Password != "" {
		u.User = url.UserPassword(c.Username, c.Password)
	}

	q := u.Query()
	if c.Timeout > 0 {
		q.Add("timeout", fmt.Sprintf("%ds", c.Timeout))
	}

	u.RawQuery = q.Encode()

	return u.String()
}

func (c *Config) DSN() string {
	// For ClickHouse with golang-migrate, use the standard format
	return fmt.Sprintf("clickhouse://%s@%s:%s/%s",
		url.UserPassword(c.Username, c.Password).String(), c.Host, c.Port, c.Database)
}

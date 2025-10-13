package postgres

import (
	"net/url"
	"strconv"
)

type Config struct {
	Name              string `env:"DB_NAME,required"`
	User              string `env:"DB_USER,required"`
	Host              string `env:"DB_HOST,required"`
	Port              string `env:"DB_PORT,required"`
	Password          string `env:"DB_PASSWORD,required"`
	ConnectionTimeout int    `env:"DB_CONNECTION_TIMEOUT,required"`
}

func (c *Config) DatabaseConfig() *Config {
	return c
}

func (c *Config) ConnectionString() string {
	if c == nil {
		return ""
	}

	host := c.Host
	if v := c.Port; v != "" {
		host = host + ":" + v
	}

	u := &url.URL{
		Scheme: "postgres",
		Host:   host,
		Path:   c.Name,
	}

	if c.User != "" || c.Password != "" {
		u.User = url.UserPassword(c.User, c.Password)
	}

	q := u.Query()
	if v := c.ConnectionTimeout; v > 0 {
		q.Add("connect_timeout", strconv.Itoa(v))
	}
	u.RawQuery = q.Encode()

	return u.String()
}

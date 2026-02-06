package clickhouse

type Config struct {
	URL string `env:"CLICKHOUSE_URL,required"`
}

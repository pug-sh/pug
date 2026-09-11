package billing

import (
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
)

// Config is the billing switch plus the meter's rescan window, which doubles as
// the invoicing grace so the two cannot drift. Declared here so `pug billing`
// and the cron binaries read it without importing the server.
type Config struct {
	Enabled    bool `env:"PUG_BILLING_ENABLED,default=false"`
	RescanDays int  `env:"PUG_USAGE_RESCAN_DAYS"`
}

// Grace is how long after a period ends its usage is treated as final.
func (c Config) Grace() time.Duration {
	return time.Duration(coreusage.RescanDays(c.RescanDays)) * 24 * time.Hour
}

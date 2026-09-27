package meter

import (
	"testing"

	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// Off means nothing is metered and nothing is built: an unknown provider name would
// fail construction, so reaching it proves the pass stopped first.
func TestRunIsANoOpWithBillingOff(t *testing.T) {
	t.Setenv("PUG_BILLING_ENABLED", "false")
	t.Setenv("PUG_BILLING_PROVIDER", "stripe")
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with billing off: %v", err)
	}
}

// On with no provider is a misconfiguration the CronJob must report.
func TestRunFailsWithoutAProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	t.Setenv("PUG_BILLING_ENABLED", "true")
	t.Setenv("PUG_BILLING_PROVIDER", "")
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	if err := Run(t.Context()); err == nil {
		t.Fatal("Run with billing on and no provider must fail")
	}
}

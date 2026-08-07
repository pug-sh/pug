package profiles_test

import (
	"testing"

	"github.com/pug-sh/pug/internal/testutil"
)

// TestMain tears down the containers this package's tests share. See
// testutil.Main.
func TestMain(m *testing.M) { testutil.Main(m) }

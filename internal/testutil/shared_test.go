package testutil

import (
	"errors"
	"os"
	"testing"
)

const ryukDisabledEnv = "TESTCONTAINERS_RYUK_DISABLED"

// TestForceRyukEnablesReaperByDefault pins the override that Main relies on to
// reap a package's containers when a panic or an expired -timeout kills the
// process before shutdown can run.
func TestForceRyukEnablesReaperByDefault(t *testing.T) {
	// t.Setenv registers the restore; Unsetenv then leaves the variable genuinely
	// absent, which is the state forceRyuk keys off.
	t.Setenv(ryukDisabledEnv, "")
	if err := os.Unsetenv(ryukDisabledEnv); err != nil {
		t.Fatalf("unset %s: %v", ryukDisabledEnv, err)
	}

	forceRyuk()

	if got := os.Getenv(ryukDisabledEnv); got != "false" {
		t.Errorf("%s = %q, want %q so testcontainers starts the reaper", ryukDisabledEnv, got, "false")
	}
}

// TestForceRyukRespectsExplicitOptOut pins the escape hatch. Ryuk cannot drive
// every Docker setup, and a run that cannot start it at all is worse than one
// that leaks on an abnormal exit — so an explicit opt-out has to survive.
func TestForceRyukRespectsExplicitOptOut(t *testing.T) {
	t.Setenv(ryukDisabledEnv, "true")

	forceRyuk()

	if got := os.Getenv(ryukDisabledEnv); got != "true" {
		t.Errorf("%s = %q, want the caller's %q preserved", ryukDisabledEnv, got, "true")
	}
}

// TestTeardownRegistryContainsPanickingTeardown covers the two ways one bad
// teardown used to poison the rest: it aborted the loop, stranding every
// container still queued behind it, and it escaped Main before it could exit
// with the suite's own status.
func TestTeardownRegistryContainsPanickingTeardown(t *testing.T) {
	var r teardownRegistry

	var ranAfter bool
	// Registered first, so LIFO runs it last — after the panicking one.
	r.add(func() error {
		ranAfter = true
		return nil
	})
	r.add(func() error { panic("boom") })

	err := r.run()

	if err == nil {
		t.Error("run() = nil, want the panic reported")
	}
	if !ranAfter {
		t.Error("a panicking teardown stopped the ones queued behind it")
	}
	if got := len(r.fns); got != 0 {
		t.Errorf("len(r.fns) = %d after run, want 0 so a second call is a no-op", got)
	}
}

// TestTeardownRegistryReportsErrors asserts a failed container termination
// reaches Main, which turns it into a failing run rather than a swallowed write
// to stdout.
func TestTeardownRegistryReportsErrors(t *testing.T) {
	var r teardownRegistry

	sentinel := errors.New("terminate failed")
	r.add(func() error { return sentinel })

	if err := r.run(); !errors.Is(err, sentinel) {
		t.Errorf("run() = %v, want it to wrap %v", err, sentinel)
	}
}

package testutil

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

// Main runs a package's tests and then tears down the containers they shared.
//
// Every package whose tests call a Setup* helper must wire this up:
//
//	func TestMain(m *testing.M) { testutil.Main(m) }
//
// The Setup* helpers start one container per test binary rather than one per
// test, so a container has no owning test to hang a t.Cleanup on and would
// otherwise outlive the run. TestSetupCallersWireMain enforces the wiring.
//
// The teardown below only covers a normal finish. A panicking test, an expired
// -timeout or a Ctrl-C all kill the process without m.Run ever returning, and a
// panic on a test's goroutine will not run a defer on this one, so there is no
// arrangement here that would cover them. forceRyuk is what covers them.
func Main(m *testing.M) {
	forceRyuk()

	code := m.Run()

	if err := teardowns.run(); err != nil {
		fmt.Fprintf(os.Stderr, "testutil: shutdown: %v\n", err)
		if code == 0 {
			code = 1
		}
	}

	os.Exit(code)
}

// forceRyuk turns testcontainers' reaper on for this test binary, overriding a
// ~/.testcontainers.properties that disables it.
//
// Ryuk is a container the test process holds a TCP connection to; it kills
// everything this session labelled once that connection drops. That makes it the
// only thing that reaps a package's containers when the process dies without
// reaching shutdown. Back when a container belonged to a single test, leaking one
// was survivable and disabling Ryuk cost little; one container per binary shared
// by every test in it is not, hence overriding a preference rather than reading
// it.
//
// The environment beats the properties file, and the config is read once on first
// use, so setting it before m.Run is what makes it take. An explicit environment
// opt-out still wins: Ryuk cannot drive every Docker setup, and a run that cannot
// start it at all is worse than one that leaks on an abnormal exit.
func forceRyuk() {
	if _, explicit := os.LookupEnv("TESTCONTAINERS_RYUK_DISABLED"); explicit {
		return
	}
	if err := os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "false"); err != nil {
		fmt.Fprintf(os.Stderr, "testutil: enable ryuk: %v\n", err)
	}
}

// lazyContainer holds a container started on first use and shared by the rest
// of the package's tests.
type lazyContainer[T any] struct {
	kind  string
	start func() (*T, error)

	mu  sync.Mutex
	val *T
}

// get returns the shared container, starting it if this is the first call.
//
// A failed start is deliberately not memoized. Container starts time out when
// the Docker daemon is saturated — which is exactly when a whole-suite run is
// underway — and that is transient: the next test may well get its container.
// Caching the error would turn one unlucky start into a failure for every
// remaining test in the package, each reported against a stale message that
// points at the daemon rather than at the test that actually tripped.
func (l *lazyContainer[T]) get(t *testing.T) *T {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.val != nil {
		return l.val
	}
	val, err := l.start()
	if err != nil {
		t.Fatalf("testutil: start shared %s: %v", l.kind, err)
	}
	l.val = val
	return l.val
}

// teardownRegistry collects what a package's shared containers need done when its
// tests finish. The zero value is ready to use.
type teardownRegistry struct {
	mu  sync.Mutex
	fns []func() error
}

// teardowns is the registry Main drains. It is package state because Go's testing
// framework offers nowhere else to put it: TestMain cannot hand a value to the
// tests that register against it, so the one process-wide instance is the only
// thing both ends can reach. The type keeps that reachability from also meaning
// the mutex and the slice are separately addressable — every caller goes through
// a method that holds the right lock.
var teardowns teardownRegistry

// add records a teardown to run once the package's tests finish.
func (r *teardownRegistry) add(fn func() error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fns = append(r.fns, fn)
}

// run executes every teardown, most recently registered first, and reports what
// failed. It drains the registry, so a second call is a no-op.
//
// The teardowns run off the lock: each one stops a container, and holding a
// non-reentrant mutex across that would deadlock a teardown that registered
// another.
func (r *teardownRegistry) run() error {
	r.mu.Lock()
	fns := r.fns
	r.fns = nil
	r.mu.Unlock()

	var errs []error
	for i := len(fns) - 1; i >= 0; i-- {
		errs = append(errs, runTeardown(fns[i]))
	}
	return errors.Join(errs...)
}

// runTeardown contains a panicking teardown, which would otherwise strand every
// container still queued behind it and escape Main before it could exit with the
// suite's own status — reporting a green run as a crash, under a stack through
// teardown plumbing.
func runTeardown(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("teardown panicked: %v", r)
		}
	}()
	return fn()
}

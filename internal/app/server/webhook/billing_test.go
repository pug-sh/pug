package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/billing/mandate"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// stubProvider verifies by a magic body rather than a real signature; the
// signature schemes themselves are tested in each provider's package.
type stubProvider struct {
	name         string
	verifyErr    error
	cannotVerify bool
}

const goodBody = "verify-me"

func (p stubProvider) Name() string { return p.name }

func (p stubProvider) CanVerify() bool { return !p.cannotVerify }

func (p stubProvider) Verify(_ http.Header, raw []byte) (corebilling.Delivery, error) {
	if p.verifyErr != nil {
		return corebilling.Delivery{}, p.verifyErr
	}
	if string(raw) != goodBody {
		return corebilling.Delivery{}, errors.New("bad signature")
	}
	return corebilling.Delivery{
		WebhookID:   "evt_1",
		EventType:   "payment.succeeded",
		RawPayload:  raw,
		DeliveredAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}, nil
}

// Nothing below Verify is reached by these tests: a zero event is the "store,
// mark processed, ignore" disposition, which is all the handler needs.
func (p stubProvider) Normalize(corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, nil
}

func (p stubProvider) CreateCheckoutSession(context.Context, corebilling.CheckoutInput) (string, string, error) {
	return "", "", errors.New("unused")
}
func (p stubProvider) CreatePortalSession(context.Context, string) (string, error) {
	return "", errors.New("unused")
}
func (p stubProvider) FetchSubscription(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, errors.New("unused")
}
func (p stubProvider) FetchCheckoutOutcome(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, errors.New("unused")
}

// newService builds the lifecycle the route mounts from, with provider as its own:
// the route takes its provider off the service, so a test chooses it here. A nil
// provider is the no-provider shape.
func newService(t *testing.T, provider corebilling.PaymentProvider) (*mandate.Service, *testutil.TestPostgres) {
	t.Helper()
	pg := testutil.SetupPostgres(t)
	entitlements, err := entitlement.NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("new entitlement service: %v", err)
	}
	var payments *corebilling.Payments
	if provider != nil {
		payments = &corebilling.Payments{Provider: provider}
	}
	return mandate.NewService(pg.PgRO, pg.PgW, payments, entitlements), pg
}

// handlerFor is the route's handler as MountBilling builds it, over the service's
// own provider.
func handlerFor(svc *mandate.Service) *billingHandler {
	return &billingHandler{provider: svc.Provider(), service: svc}
}

func post(t *testing.T, h http.Handler, path, body string) *http.Response {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	res, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestBillingPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// The provider is named in the URL so no verifier has to be guessed by trying
	// each in turn.
	if got := BillingPath("dodo"); got != "/billing/webhooks/dodo" {
		t.Errorf("BillingPath(dodo) = %q", got)
	}
}

// With no secret configured the route must not mount at all: 404 is the
// fail-closed direction, verify-nothing is not.
func TestMountRequiresAVerifiableProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	noProvider, _ := newService(t, nil)
	noSecret, _ := newService(t, stubProvider{name: "dodo", cannotVerify: true})
	for name, svc := range map[string]*mandate.Service{
		"no service":  nil,
		"no provider": noProvider,
		"no secret":   noSecret,
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			if MountBilling(mux, svc) {
				t.Fatal("Mount reported a route it must not have registered")
			}
			if res := post(t, mux, BillingPath("dodo"), goodBody); res.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", res.StatusCode)
			}
		})
	}
}

func TestMountRegistersBothPathForms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	svc, _ := newService(t, stubProvider{name: "dodo"})
	mux := http.NewServeMux()
	if !MountBilling(mux, svc) {
		t.Fatal("Mount did not register the route")
	}
	// ServeMux exact-matches the bare pattern and only redirects the other way,
	// so the trailing-slash twin has to be registered rather than relied on.
	for _, path := range []string{"/billing/webhooks/dodo", "/billing/webhooks/dodo/"} {
		if res := post(t, mux, path, goodBody); res.StatusCode != http.StatusNoContent {
			t.Errorf("POST %s = %d, want 204", path, res.StatusCode)
		}
	}
}

func TestHandlerStatuses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	svc, _ := newService(t, stubProvider{name: "dodo"})

	t.Run("a verified delivery is 204 once durable", func(t *testing.T) {
		res := post(t, handlerFor(svc), "/", goodBody)
		if res.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", res.StatusCode)
		}
	})

	// An auth failure, not a server fault: a 5xx here would ask the provider to
	// retry a body it can never sign.
	t.Run("an unsigned body is 401", func(t *testing.T) {
		res := post(t, handlerFor(svc), "/", "forged")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", res.StatusCode)
		}
	})

	t.Run("a body past the cap is 400", func(t *testing.T) {
		res := post(t, handlerFor(svc), "/",
			strings.Repeat("x", maxBodyBytes+1))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("a non-POST is 405 and says so", func(t *testing.T) {
		srv := httptest.NewServer(handlerFor(svc))
		t.Cleanup(srv.Close)
		res, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", res.StatusCode)
		}
		if res.Header.Get("Allow") != http.MethodPost {
			t.Errorf("Allow = %q, want POST", res.Header.Get("Allow"))
		}
	})
}

// 2xx only once the row is durable: a 5xx is what asks for the next attempt, and
// answering 204 on a failed write would lose the delivery for good.
func TestAFailedWriteIs500(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	svc, pg := newService(t, stubProvider{name: "dodo"})
	pg.PgW.Close()

	res := post(t, handlerFor(svc), "/", goodBody)
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}

// A body that verified and still will not decode is pug's fault. 401 would file the
// money path going down under the same warning a port scanner produces, and the
// provider's retry is what a redeploy needs.
func TestAnUndecodableBodyIsRetriedNotRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	provider := stubProvider{
		name:      "dodo",
		verifyErr: fmt.Errorf("%w: unexpected envelope", corebilling.ErrUndecodable),
	}
	svc, _ := newService(t, provider)
	mux := http.NewServeMux()
	if !MountBilling(mux, svc) {
		t.Fatal("Mount did not register the route")
	}

	if res := post(t, mux, BillingPath("dodo"), goodBody); res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 so the provider retries", res.StatusCode)
	}
}

// The other half: a genuine signature failure stays a 401, so a scanner probing an
// open route cannot fill the error telemetry.
func TestABadSignatureStaysUnauthorized(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	svc, _ := newService(t, stubProvider{name: "dodo"})
	mux := http.NewServeMux()
	if !MountBilling(mux, svc) {
		t.Fatal("Mount did not register the route")
	}

	if res := post(t, mux, BillingPath("dodo"), "not-the-magic-body"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}

// panickingProvider fails the way a library can: not with an error.
type panickingProvider struct{ stubProvider }

func (panickingProvider) Verify(http.Header, []byte) (corebilling.Delivery, error) {
	panic("provider library blew up")
}

// net/http recovers per connection either way; this pins the 500, without which
// the provider reads a dropped connection and retries forever.
func TestPanicIsContainedAsA500(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	provider := panickingProvider{stubProvider{name: "stub"}}
	svc, _ := newService(t, provider)
	mux := http.NewServeMux()
	if !MountBilling(mux, svc) {
		t.Fatal("Mount refused a verifiable provider")
	}

	res := post(t, mux, BillingPath("stub"), goodBody)
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}

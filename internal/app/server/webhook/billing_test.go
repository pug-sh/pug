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
func (p stubProvider) NormalizePayment(corebilling.Delivery) (corebilling.PaymentEvent, error) {
	return corebilling.PaymentEvent{}, nil
}
func (p stubProvider) Charge(context.Context, corebilling.ChargeInput) (string, error) {
	return "", errors.New("unused")
}
func (p stubProvider) ListPayments(context.Context, string, time.Time) ([]corebilling.PaymentRecord, error) {
	return nil, errors.New("unused")
}
func (p stubProvider) FetchPayment(context.Context, string) (corebilling.PaymentRecord, error) {
	return corebilling.PaymentRecord{}, errors.New("unused")
}
func (p stubProvider) SetNextBillingDate(context.Context, string, time.Time) error {
	return errors.New("unused")
}
func (p stubProvider) CancelSubscription(context.Context, string) error { return errors.New("unused") }

func newService(t *testing.T) (*corebilling.Service, *testutil.TestPostgres) {
	t.Helper()
	pg := testutil.SetupPostgres(t)
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, pg
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

	svc, _ := newService(t)
	for name, tc := range map[string]struct {
		service  *corebilling.Service
		provider corebilling.PaymentProvider
	}{
		"no service":  {nil, stubProvider{name: "dodo"}},
		"no provider": {svc, nil},
		"no secret":   {svc, stubProvider{name: "dodo", cannotVerify: true}},
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			if MountBilling(mux, tc.service, tc.provider) {
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

	svc, _ := newService(t)
	mux := http.NewServeMux()
	if !MountBilling(mux, svc, stubProvider{name: "dodo"}) {
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

	svc, _ := newService(t)

	t.Run("a verified delivery is 204 once durable", func(t *testing.T) {
		res := post(t, &billingHandler{provider: stubProvider{name: "dodo"}, service: svc}, "/", goodBody)
		if res.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", res.StatusCode)
		}
	})

	// An auth failure, not a server fault: a 5xx here would ask the provider to
	// retry a body it can never sign.
	t.Run("an unsigned body is 401", func(t *testing.T) {
		res := post(t, &billingHandler{provider: stubProvider{name: "dodo"}, service: svc}, "/", "forged")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", res.StatusCode)
		}
	})

	t.Run("a body past the cap is 400", func(t *testing.T) {
		res := post(t, &billingHandler{provider: stubProvider{name: "dodo"}, service: svc}, "/",
			strings.Repeat("x", maxBodyBytes+1))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("a non-POST is 405 and says so", func(t *testing.T) {
		srv := httptest.NewServer(&billingHandler{provider: stubProvider{name: "dodo"}, service: svc})
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

	svc, pg := newService(t)
	pg.PgW.Close()

	res := post(t, &billingHandler{provider: stubProvider{name: "dodo"}, service: svc}, "/", goodBody)
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

	svc, _ := newService(t)
	mux := http.NewServeMux()
	provider := stubProvider{
		name:      "dodo",
		verifyErr: fmt.Errorf("%w: unexpected envelope", corebilling.ErrUndecodable),
	}
	if !MountBilling(mux, svc, provider) {
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

	svc, _ := newService(t)
	mux := http.NewServeMux()
	if !MountBilling(mux, svc, stubProvider{name: "dodo"}) {
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

	svc, _ := newService(t)
	mux := http.NewServeMux()
	provider := panickingProvider{stubProvider{name: "stub"}}
	if !MountBilling(mux, svc, provider) {
		t.Fatal("Mount refused a verifiable provider")
	}

	res := post(t, mux, BillingPath("stub"), goodBody)
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}

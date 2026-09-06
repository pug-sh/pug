package dodo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	dodopayments "github.com/dodopayments/dodopayments-go"
	"github.com/dodopayments/dodopayments-go/option"
	"github.com/dodopayments/dodopayments-go/shared"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// apiClient points a Client at a stub Dodo. Built directly rather than through
// New, which has no base-URL seam: nothing in production should be able to aim
// the client somewhere else.
func apiClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{
		api: dodopayments.NewClient(
			option.WithBaseURL(srv.URL+"/"),
			option.WithBearerToken("sk_test"),
			option.WithMaxRetries(0),
		),
		products:    map[string]string{"growth": "prod_growth"},
		slugByProdM: map[string]string{"prod_growth": "growth"},
	}
}

func jsonHandler(t *testing.T, status int, body string, record func(*http.Request)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			record(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write stub response: %v", err)
		}
	})
}

// The subscription object as Dodo returns it, matching activeBody's data so the
// direct read and the delivery can be compared.
const subscriptionJSONBody = `{` +
	`"subscription_id":"sub_1","product_id":"prod_growth","status":"active",` +
	`"currency":"USD","recurring_pre_tax_amount":2000,` +
	`"customer":{"customer_id":"cus_1"},"metadata":{"org_id":"org_abc"},` +
	`"previous_billing_date":"2026-06-01T00:00:00Z","next_billing_date":"2026-07-01T00:00:00Z"}`

func TestCreateCheckoutSession(t *testing.T) {
	t.Run("sends the cart and the attribution", func(t *testing.T) {
		var got struct {
			ProductCart []struct {
				ProductID string `json:"product_id"`
				Quantity  int64  `json:"quantity"`
			} `json:"product_cart"`
			Metadata  map[string]string `json:"metadata"`
			ReturnURL string            `json:"return_url"`
			Customer  struct {
				Email string `json:"email"`
			} `json:"customer"`
		}
		var path, method string
		c := apiClient(t, jsonHandler(t, http.StatusOK,
			`{"session_id":"cs_1","checkout_url":"https://checkout.example/cs_1"}`,
			func(r *http.Request) {
				path, method = r.URL.Path, r.Method
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode checkout request: %v", err)
				}
			}))

		id, url, err := c.CreateCheckoutSession(context.Background(), corebilling.CheckoutInput{
			ProductID:     "prod_growth",
			OrgID:         "org_abc",
			ReturnURL:     "https://app.example/settings/billing",
			CustomerEmail: "buyer@example.com",
		})
		if err != nil {
			t.Fatalf("CreateCheckoutSession: %v", err)
		}
		if id != "cs_1" || url != "https://checkout.example/cs_1" {
			t.Errorf("session = (%q, %q), want (cs_1, https://checkout.example/cs_1)", id, url)
		}
		if method != http.MethodPost || path != "/checkouts" {
			t.Errorf("request = %s %s, want POST /checkouts", method, path)
		}
		if len(got.ProductCart) != 1 || got.ProductCart[0].ProductID != "prod_growth" || got.ProductCart[0].Quantity != 1 {
			t.Errorf("product_cart = %+v, want one prod_growth at quantity 1", got.ProductCart)
		}
		// Every delivery this checkout produces carries it; without it the webhook
		// can only attribute by customer id.
		if got.Metadata[metadataOrgID] != "org_abc" {
			t.Errorf("metadata[%s] = %q, want org_abc", metadataOrgID, got.Metadata[metadataOrgID])
		}
		if got.ReturnURL != "https://app.example/settings/billing" {
			t.Errorf("return_url = %q", got.ReturnURL)
		}
		if got.Customer.Email != "buyer@example.com" {
			t.Errorf("customer.email = %q, want buyer@example.com", got.Customer.Email)
		}
	})

	t.Run("omits an unset return url and customer", func(t *testing.T) {
		var raw map[string]any
		c := apiClient(t, jsonHandler(t, http.StatusOK,
			`{"session_id":"cs_1","checkout_url":"https://checkout.example/cs_1"}`,
			func(r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
					t.Errorf("decode checkout request: %v", err)
				}
			}))
		if _, _, err := c.CreateCheckoutSession(context.Background(),
			corebilling.CheckoutInput{ProductID: "prod_growth", OrgID: "org_abc"}); err != nil {
			t.Fatalf("CreateCheckoutSession: %v", err)
		}
		for _, key := range []string{"return_url", "customer"} {
			if _, ok := raw[key]; ok {
				t.Errorf("%s was sent for an unset field", key)
			}
		}
	})

	t.Run("a session with no url is an error", func(t *testing.T) {
		c := apiClient(t, jsonHandler(t, http.StatusOK, `{"session_id":"cs_1"}`, nil))
		if _, _, err := c.CreateCheckoutSession(context.Background(),
			corebilling.CheckoutInput{ProductID: "prod_growth"}); err == nil {
			t.Fatal("a session with no checkout_url was accepted; there is nowhere to send the buyer")
		}
	})

	t.Run("a provider error surfaces", func(t *testing.T) {
		c := apiClient(t, jsonHandler(t, http.StatusBadGateway, `{"error":"nope"}`, nil))
		if _, _, err := c.CreateCheckoutSession(context.Background(),
			corebilling.CheckoutInput{ProductID: "prod_growth"}); err == nil {
			t.Fatal("a 502 from the provider was reported as a checkout")
		}
	})
}

func TestCreatePortalSession(t *testing.T) {
	t.Run("returns the link", func(t *testing.T) {
		var path string
		c := apiClient(t, jsonHandler(t, http.StatusOK, `{"link":"https://portal.example/s"}`,
			func(r *http.Request) { path = r.URL.Path }))
		link, err := c.CreatePortalSession(context.Background(), "cus_1")
		if err != nil {
			t.Fatalf("CreatePortalSession: %v", err)
		}
		if link != "https://portal.example/s" {
			t.Errorf("link = %q", link)
		}
		if path != "/customers/cus_1/customer-portal/session" {
			t.Errorf("path = %q, want the customer's portal session", path)
		}
	})

	t.Run("a session with no link is an error", func(t *testing.T) {
		c := apiClient(t, jsonHandler(t, http.StatusOK, `{}`, nil))
		if _, err := c.CreatePortalSession(context.Background(), "cus_1"); err == nil {
			t.Fatal("a portal session with no link was accepted")
		}
	})

	t.Run("a provider error surfaces", func(t *testing.T) {
		c := apiClient(t, jsonHandler(t, http.StatusNotFound, `{"error":"no such customer"}`, nil))
		if _, err := c.CreatePortalSession(context.Background(), "cus_missing"); err == nil {
			t.Fatal("a 404 from the provider was reported as a portal session")
		}
	})
}

// A direct read and a delivery must normalize to the same event, or reconcile
// and the webhook would apply different things for the same subscription.
func TestFetchSubscriptionMatchesADelivery(t *testing.T) {
	var path string
	c := apiClient(t, jsonHandler(t, http.StatusOK, subscriptionJSONBody,
		func(r *http.Request) { path = r.URL.Path }))

	fetched, err := c.FetchSubscription(context.Background(), "sub_1")
	if err != nil {
		t.Fatalf("FetchSubscription: %v", err)
	}
	if path != "/subscriptions/sub_1" {
		t.Errorf("path = %q, want /subscriptions/sub_1", path)
	}

	normalized, err := c.Normalize(corebilling.Delivery{
		EventType:  "subscription.active",
		RawPayload: []byte(activeBody),
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if fetched != normalized {
		t.Errorf("fetched = %+v\nnormalized = %+v\nthe two apply paths disagree", fetched, normalized)
	}
	if fetched.OrgID != "org_abc" || fetched.Status != corebilling.SubStatusActive || fetched.PriceCents != 2000 {
		t.Errorf("event = %+v", fetched)
	}
}

func TestFetchSubscriptionProviderError(t *testing.T) {
	c := apiClient(t, jsonHandler(t, http.StatusInternalServerError, `{"error":"boom"}`, nil))
	if _, err := c.FetchSubscription(context.Background(), "sub_1"); err == nil {
		t.Fatal("a 500 from the provider was reported as a subscription")
	}
}

func TestFetchCheckoutOutcome(t *testing.T) {
	// session -> payment -> subscription, each hop stubbed independently.
	mux := func(session, payment string) http.Handler {
		m := http.NewServeMux()
		m.Handle("/checkouts/cs_1", jsonHandler(t, http.StatusOK, session, nil))
		if payment != "" {
			m.Handle("/payments/pay_1", jsonHandler(t, http.StatusOK, payment, nil))
		}
		m.Handle("/subscriptions/sub_1", jsonHandler(t, http.StatusOK, subscriptionJSONBody, nil))
		return m
	}

	t.Run("walks to the subscription", func(t *testing.T) {
		c := apiClient(t, mux(
			`{"id":"cs_1","created_at":"2026-06-01T00:00:00Z","payment_id":"pay_1","payment_status":"succeeded"}`,
			`{"payment_id":"pay_1","status":"succeeded","subscription_id":"sub_1"}`))
		got, err := c.FetchCheckoutOutcome(context.Background(), "cs_1")
		if err != nil {
			t.Fatalf("FetchCheckoutOutcome: %v", err)
		}
		if got.ProviderSubID != "sub_1" || got.OrgID != "org_abc" {
			t.Errorf("event = %+v, want the subscription the checkout produced", got)
		}
	})

	// "Not yet" is not a fault: a buyer who is still filling the form has no
	// payment, and a one-off payment has no subscription to apply.
	t.Run("an unsettled checkout is a zero event", func(t *testing.T) {
		for name, stub := range map[string][2]string{
			"no payment yet": {`{"id":"cs_1","created_at":"2026-06-01T00:00:00Z"}`, ""},
			"payment is not a subscription's": {
				`{"id":"cs_1","created_at":"2026-06-01T00:00:00Z","payment_id":"pay_1","payment_status":"processing"}`,
				`{"payment_id":"pay_1","status":"processing"}`,
			},
		} {
			t.Run(name, func(t *testing.T) {
				c := apiClient(t, mux(stub[0], stub[1]))
				got, err := c.FetchCheckoutOutcome(context.Background(), "cs_1")
				if err != nil {
					t.Fatalf("FetchCheckoutOutcome: %v", err)
				}
				if !got.IsZero() {
					t.Errorf("event = %+v, want zero", got)
				}
			})
		}
	})

	// Without this a declined card polls forever as "not yet".
	t.Run("a checkout the provider gave up on fails", func(t *testing.T) {
		for name, stub := range map[string][2]string{
			"session failed": {`{"id":"cs_1","created_at":"2026-06-01T00:00:00Z","payment_id":"pay_1","payment_status":"failed"}`, ""},
			"payment cancelled": {
				`{"id":"cs_1","created_at":"2026-06-01T00:00:00Z","payment_id":"pay_1","payment_status":"processing"}`,
				`{"payment_id":"pay_1","status":"cancelled"}`,
			},
		} {
			t.Run(name, func(t *testing.T) {
				c := apiClient(t, mux(stub[0], stub[1]))
				_, err := c.FetchCheckoutOutcome(context.Background(), "cs_1")
				if !errors.Is(err, corebilling.ErrCheckoutFailed) {
					t.Fatalf("err = %v, want ErrCheckoutFailed", err)
				}
			})
		}
	})

	t.Run("a provider error surfaces", func(t *testing.T) {
		for name, h := range map[string]http.Handler{
			"session read": jsonHandler(t, http.StatusInternalServerError, `{}`, nil),
			"payment read": mux(`{"id":"cs_1","created_at":"2026-06-01T00:00:00Z","payment_id":"pay_missing"}`, ""),
		} {
			t.Run(name, func(t *testing.T) {
				c := apiClient(t, h)
				_, err := c.FetchCheckoutOutcome(context.Background(), "cs_1")
				if err == nil || errors.Is(err, corebilling.ErrCheckoutFailed) {
					t.Fatalf("err = %v, want a transport error", err)
				}
			})
		}
	})
}

// An unknown word must delay the answer, never invent a failure.
func TestTerminalIntent(t *testing.T) {
	for status, want := range map[string]bool{
		"failed": true, "cancelled": true, "canceled": true, " Failed ": true,
		"succeeded": false, "processing": false, "requires_customer_action": false,
		"": false, "some_state_dodo_adds_later": false,
	} {
		if got := terminalIntent(status); got != want {
			t.Errorf("terminalIntent(%q) = %v, want %v", status, got, want)
		}
	}
}

// A non-string value was not written by pug, so it is dropped rather than
// coerced.
func TestStringMetadata(t *testing.T) {
	if got := stringMetadata(nil); got != nil {
		t.Errorf("stringMetadata(nil) = %v, want nil", got)
	}
	got := stringMetadata(dodopayments.Metadata{
		"org_id": shared.UnionString("org_abc"),
		"count":  shared.UnionFloat(3),
		"beta":   shared.UnionBool(true),
	})
	if len(got) != 1 || got[metadataOrgID] != "org_abc" {
		t.Errorf("stringMetadata = %v, want only the string values", got)
	}
}

func TestNameAndProductForSlug(t *testing.T) {
	c := apiClient(t, jsonHandler(t, http.StatusOK, `{}`, nil))
	if c.Name() != Name {
		t.Errorf("Name = %q, want %q", c.Name(), Name)
	}
	if id, ok := c.ProductForSlug("growth"); !ok || id != "prod_growth" {
		t.Errorf("ProductForSlug(growth) = (%q, %v)", id, ok)
	}
	if _, ok := c.ProductForSlug("free"); ok {
		t.Error("a tier with no product id was reported as purchasable")
	}
}

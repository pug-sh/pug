package dodo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	dodopayments "github.com/dodopayments/dodopayments-go"
	"github.com/dodopayments/dodopayments-go/option"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// The checkout authorizes a card and charges nothing: on_demand with mandate_only.
func TestCreateCheckoutSessionIsMandateOnly(t *testing.T) {
	var got struct {
		SubscriptionData struct {
			OnDemand struct {
				MandateOnly bool `json:"mandate_only"`
			} `json:"on_demand"`
		} `json:"subscription_data"`
		Customization struct {
			ShowOnDemandTag bool `json:"show_on_demand_tag"`
		} `json:"customization"`
	}
	c := apiClient(t, jsonHandler(t, http.StatusOK,
		`{"session_id":"cs_1","checkout_url":"https://checkout.example/cs_1"}`,
		func(r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode checkout request: %v", err)
			}
		}))
	if _, _, err := c.CreateCheckoutSession(context.Background(), corebilling.CheckoutInput{
		ProductID: "prod_mandate", OrgID: "org_abc", CheckoutRef: "ref_1",
	}); err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	if !got.SubscriptionData.OnDemand.MandateOnly {
		t.Error("subscription_data.on_demand.mandate_only was not sent; the checkout would charge the product's price")
	}
	if !got.Customization.ShowOnDemandTag {
		t.Error("show_on_demand_tag was not sent")
	}
}

func TestCharge(t *testing.T) {
	t.Run("sends the amount and the invoice metadata", func(t *testing.T) {
		var got struct {
			ProductPrice       int64             `json:"product_price"`
			ProductCurrency    string            `json:"product_currency"`
			ProductDescription string            `json:"product_description"`
			Metadata           map[string]string `json:"metadata"`
		}
		var path string
		c := apiClient(t, jsonHandler(t, http.StatusOK, `{"payment_id":"pay_1"}`, func(r *http.Request) {
			path = r.URL.Path
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode charge request: %v", err)
			}
		}))
		id, err := c.Charge(context.Background(), corebilling.ChargeInput{
			ProviderSubID: "sub_1", AmountCents: 9_700, Currency: "usd", Description: "Pug: 2.34M events",
			InvoiceID: "inv_1", OrgID: "org_abc", PeriodStart: time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("Charge: %v", err)
		}
		if id != "pay_1" || path != "/subscriptions/sub_1/charge" {
			t.Errorf("payment = %q via %s", id, path)
		}
		if got.ProductPrice != 9_700 || got.ProductCurrency != "USD" || got.ProductDescription != "Pug: 2.34M events" {
			t.Errorf("request = %+v", got)
		}
		// Explicit: the charge inherits the subscription's metadata only when none is
		// passed, and every payment webhook needs the invoice id.
		if got.Metadata["invoice_id"] != "inv_1" || got.Metadata["org_id"] != "org_abc" || got.Metadata["period_start"] != "2026-05-10T00:00:00Z" {
			t.Errorf("metadata = %v", got.Metadata)
		}
	})

	t.Run("a 402 is a definitive refusal", func(t *testing.T) {
		c := apiClient(t, jsonHandler(t, http.StatusPaymentRequired, `{"message":"card declined"}`, nil))
		_, err := c.Charge(context.Background(), corebilling.ChargeInput{ProviderSubID: "sub_1", AmountCents: 100, Currency: "USD"})
		var decline *corebilling.DeclineError
		if !errors.As(err, &decline) || decline.Code != "HTTP_402" {
			t.Fatalf("err = %v, want a DeclineError with the status", err)
		}
	})

	t.Run("a 404 is a mandate that cannot be charged", func(t *testing.T) {
		c := apiClient(t, jsonHandler(t, http.StatusNotFound, `{"message":"not found"}`, nil))
		_, err := c.Charge(context.Background(), corebilling.ChargeInput{ProviderSubID: "sub_gone", AmountCents: 100, Currency: "USD"})
		if !errors.Is(err, corebilling.ErrMandateNotChargeable) {
			t.Fatalf("err = %v, want ErrMandateNotChargeable", err)
		}
	})

	t.Run("a 5xx is ambiguous", func(t *testing.T) {
		c := apiClient(t, jsonHandler(t, http.StatusBadGateway, `{"message":"upstream"}`, nil))
		_, err := c.Charge(context.Background(), corebilling.ChargeInput{ProviderSubID: "sub_1", AmountCents: 100, Currency: "USD"})
		var decline *corebilling.DeclineError
		if err == nil || errors.As(err, &decline) || errors.Is(err, corebilling.ErrMandateNotChargeable) {
			t.Fatalf("err = %v, want a plain error the caller settles by reading", err)
		}
	})

	// Pug's own problem, not the card's: dunning one of these writes a customer
	// off for a key rotation, a rate limit, or a route or content type a
	// dependency bump changed. Only 402 is a decline, so an unenumerated 4xx has
	// to land here and not in dunning.
	for _, status := range []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusRequestTimeout, http.StatusConflict,
		http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		http.StatusMethodNotAllowed, http.StatusGone, http.StatusPreconditionFailed,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType,
		http.StatusLocked, http.StatusPreconditionRequired, http.StatusUnavailableForLegalReasons,
	} {
		t.Run(fmt.Sprintf("a %d is ambiguous, not a decline", status), func(t *testing.T) {
			c := apiClient(t, jsonHandler(t, status, `{"message":"nope"}`, nil))
			_, err := c.Charge(context.Background(), corebilling.ChargeInput{ProviderSubID: "sub_1", AmountCents: 100, Currency: "USD"})
			var decline *corebilling.DeclineError
			if err == nil || errors.As(err, &decline) {
				t.Fatalf("err = %v, want a plain error the caller settles by reading", err)
			}
		})
	}

	// The endpoint has no idempotency key, so a transport-level retry takes the
	// money again. Built the way New builds it -- the harness sets MaxRetries(0),
	// which would hide exactly the bug this pins.
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"a 5xx", http.StatusBadGateway},
		{"a 429", http.StatusTooManyRequests},
	} {
		t.Run(tc.name+" is never re-POSTed", func(t *testing.T) {
			var posts int
			srv := httptest.NewServer(jsonHandler(t, tc.status, `{"message":"upstream"}`, func(*http.Request) {
				posts++
			}))
			t.Cleanup(srv.Close)
			c := &Client{api: dodopayments.NewClient(
				option.WithBaseURL(srv.URL+"/"),
				option.WithBearerToken("sk_test"),
			)}
			if _, err := c.Charge(context.Background(), corebilling.ChargeInput{
				ProviderSubID: "sub_1", AmountCents: 9_700, Currency: "USD", InvoiceID: "inv_1",
			}); err == nil {
				t.Fatal("Charge succeeded against a failing stub")
			}
			if posts != 1 {
				t.Fatalf("charged %d times, want 1: the SDK retried a charge with no idempotency key", posts)
			}
		})
	}

	t.Run("no payment id is an error", func(t *testing.T) {
		c := apiClient(t, jsonHandler(t, http.StatusOK, `{}`, nil))
		if _, err := c.Charge(context.Background(), corebilling.ChargeInput{ProviderSubID: "sub_1", AmountCents: 100, Currency: "USD"}); err == nil {
			t.Fatal("a charge with no payment_id succeeded")
		}
	})
}

const paymentJSONBody = `{"payment_id":"pay_1","status":"failed","currency":"USD","total_amount":9700,` +
	`"created_at":"2026-06-13T12:00:00Z","customer":{"customer_id":"cus_1"},` +
	`"metadata":{"invoice_id":"inv_1","org_id":"org_abc"},` +
	`"error_code":"INSUFFICIENT_FUNDS","error_message":"Insufficient funds","invoice_url":"https://dodo.example/inv/1",` +
	`"subscription_id":"sub_1"}`

// onePage serves body once and an empty page after: the SDK's auto-pager walks
// until a page comes back with no items, so a stub that always answers loops.
func onePage(t *testing.T, body string, record func(*http.Request)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			record(r)
		}
		page := `{"items":[` + body + `]}`
		if n := r.URL.Query().Get("page_number"); n != "" && n != "0" {
			page = `{"items":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(page)); err != nil {
			t.Errorf("write stub page: %v", err)
		}
	})
}

func TestListPayments(t *testing.T) {
	var query string
	c := apiClient(t, onePage(t, paymentJSONBody, func(r *http.Request) {
		if n := r.URL.Query().Get("page_number"); n == "" || n == "0" {
			query = r.URL.RawQuery
		}
	}))
	since := time.Date(2026, 6, 13, 11, 0, 0, 0, time.UTC)
	payments, err := c.ListPayments(context.Background(), "sub_1", since)
	if err != nil {
		t.Fatalf("ListPayments: %v", err)
	}
	if len(payments) != 1 {
		t.Fatalf("got %d payments, want 1", len(payments))
	}
	p := payments[0]
	if p.PaymentID != "pay_1" || p.InvoiceID != "inv_1" || p.Status != corebilling.PaymentFailed || p.InvoiceURL != "https://dodo.example/inv/1" {
		t.Errorf("payment = %+v", p)
	}
	for _, want := range []string{
		"subscription_id=sub_1",
		"created_at_gte=" + url.QueryEscape(since.Format(time.RFC3339)),
		"page_number=0",
	} {
		if !containsStr(query, want) {
			t.Errorf("query %q is missing %s", query, want)
		}
	}
}

func TestFetchPayment(t *testing.T) {
	c := apiClient(t, jsonHandler(t, http.StatusOK, paymentJSONBody, nil))
	p, err := c.FetchPayment(context.Background(), "pay_1")
	if err != nil {
		t.Fatalf("FetchPayment: %v", err)
	}
	if p.Status != corebilling.PaymentFailed || p.ErrorCode != "INSUFFICIENT_FUNDS" || p.ErrorMessage != "Insufficient funds" || p.InvoiceID != "inv_1" {
		t.Errorf("payment = %+v", p)
	}
	if p.CreatedAt.IsZero() {
		t.Error("created_at was not carried")
	}
}

func TestSetNextBillingDateAndCancel(t *testing.T) {
	var got map[string]any
	var path, method string
	c := apiClient(t, jsonHandler(t, http.StatusOK, subscriptionJSONBody, func(r *http.Request) {
		path, method = r.URL.Path, r.Method
		got = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode update request: %v", err)
		}
	}))
	at := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	if err := c.SetNextBillingDate(context.Background(), "sub_1", at); err != nil {
		t.Fatalf("SetNextBillingDate: %v", err)
	}
	if method != http.MethodPatch || path != "/subscriptions/sub_1" {
		t.Errorf("request = %s %s", method, path)
	}
	if got["next_billing_date"] != "2026-07-13T00:00:00Z" {
		t.Errorf("next_billing_date = %v", got["next_billing_date"])
	}
	if _, cancelled := got["status"]; cancelled {
		t.Error("a pin also sent a status")
	}

	if err := c.CancelSubscription(context.Background(), "sub_1"); err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}
	if got["status"] != "cancelled" {
		t.Errorf("status = %v, want cancelled", got["status"])
	}
}

// A mandate-only session's authorization may name no subscription; the customer's
// subscriptions are then listed and matched on the ref.
func TestFetchCheckoutOutcomeFallsBackToListingSubscriptions(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/checkouts/cs_1", jsonHandler(t, http.StatusOK,
		`{"id":"cs_1","created_at":"2026-06-13T12:00:00Z","payment_id":"pay_auth","payment_status":"succeeded"}`, nil))
	mux.Handle("/payments/pay_auth", jsonHandler(t, http.StatusOK,
		`{"payment_id":"pay_auth","status":"succeeded","currency":"USD","total_amount":0,`+
			`"customer":{"customer_id":"cus_1"},"metadata":{"org_id":"org_abc","checkout_ref":"ref_deadbeef"},"created_at":"2026-06-13T12:00:00Z"}`, nil))
	mux.Handle("/subscriptions/sub_1", jsonHandler(t, http.StatusOK, subscriptionJSONBody, nil))
	var listQuery string
	mux.Handle("/subscriptions", onePage(t, subscriptionJSONBody, func(r *http.Request) {
		listQuery = r.URL.RawQuery
	}))

	c := apiClient(t, mux)
	event, err := c.FetchCheckoutOutcome(context.Background(), "cs_1")
	if err != nil {
		t.Fatalf("FetchCheckoutOutcome: %v", err)
	}
	if event.ProviderSubID != "sub_1" || event.CheckoutRef != "ref_deadbeef" {
		t.Errorf("event = %+v, want the subscription found by ref", event)
	}
	if !containsStr(listQuery, "customer_id=cus_1") {
		t.Errorf("list query %q did not filter on the customer", listQuery)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

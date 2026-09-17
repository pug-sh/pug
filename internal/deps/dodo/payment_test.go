package dodo

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// The payment object as Dodo sends it, on a delivery and a direct read alike.
const paymentJSONBody = `{"payment_id":"pay_1","subscription_id":"sub_1","status":"succeeded",` +
	`"total_amount":11800,"tax":1800,"currency":"usd","error_code":null,"error_message":null,` +
	`"invoice_url":"https://pay.example/receipt/pay_1","created_at":"2026-09-12T06:00:00Z",` +
	`"metadata":{"invoice_id":"inv_1","org_id":"org_abc","period_start":"2026-08-10T00:00:00Z"}}`

var wantPayment = corebilling.Payment{
	CreatedAt:     time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC),
	Currency:      "USD",
	InvoiceID:     "inv_1",
	InvoiceURL:    "https://pay.example/receipt/pay_1",
	PaymentID:     "pay_1",
	ProviderSubID: "sub_1",
	Status:        corebilling.PaymentSucceeded,
	TaxCents:      1_800,
	TotalCents:    11_800,
}

func normalizePayment(t *testing.T, eventType, data string) (corebilling.PaymentEvent, error) {
	t.Helper()
	return testClient(t, time.Now()).NormalizePayment(corebilling.Delivery{
		EventType:  eventType,
		RawPayload: []byte(`{"type":"` + eventType + `","data":` + data + `}`),
	})
}

// A delivery and a direct read must agree, or the webhook and the poll would settle
// one payment differently.
func TestFetchPaymentMatchesADelivery(t *testing.T) {
	var path string
	c := apiClient(t, jsonHandler(t, http.StatusOK, paymentJSONBody, func(r *http.Request) { path = r.URL.Path }))
	fetched, err := c.FetchPayment(context.Background(), "pay_1")
	if err != nil {
		t.Fatalf("FetchPayment: %v", err)
	}
	if path != "/payments/pay_1" || fetched != wantPayment {
		t.Errorf("fetched %+v from %s\nwant    %+v", fetched, path, wantPayment)
	}

	event, err := normalizePayment(t, "payment.succeeded", paymentJSONBody)
	if err != nil {
		t.Fatalf("NormalizePayment: %v", err)
	}
	if event != (corebilling.PaymentEvent{Payment: wantPayment}) {
		t.Errorf("normalized %+v\nwant       %+v", event.Payment, wantPayment)
	}

	// Settle reads a decline this way, and its code is what makes it hard or soft.
	declined := apiClient(t, jsonHandler(t, http.StatusOK,
		`{"payment_id":"pay_1","subscription_id":"sub_1","status":"failed","error_code":"STOLEN_CARD","error_message":"stolen"}`, nil))
	if p, err := declined.FetchPayment(context.Background(), "pay_1"); err != nil || p.Status != corebilling.PaymentFailed ||
		p.ErrorCode != "STOLEN_CARD" || p.ErrorMessage != "stolen" {
		t.Errorf("fetched (%+v, %v), want the decline with its code", p, err)
	}

	failing := apiClient(t, jsonHandler(t, http.StatusInternalServerError, `{"message":"boom"}`, nil))
	if _, err := failing.FetchPayment(context.Background(), "pay_1"); err == nil {
		t.Error("a 500 from the provider was read as a payment")
	}
}

func TestNormalizePayment(t *testing.T) {
	t.Run("a failed payment carries its decline", func(t *testing.T) {
		event, err := normalizePayment(t, "payment.failed",
			`{"payment_id":"pay_1","subscription_id":"sub_1","status":"failed","error_code":"STOLEN_CARD",`+
				`"error_message":"stolen","metadata":{"invoice_id":"inv_1"}}`)
		if err != nil {
			t.Fatalf("NormalizePayment: %v", err)
		}
		if p := event.Payment; p.Status != corebilling.PaymentFailed || p.ErrorCode != "STOLEN_CARD" ||
			p.ErrorMessage != "stolen" || p.InvoiceID != "inv_1" {
			t.Errorf("payment = %+v", p)
		}
	})

	for name, tc := range map[string]struct {
		data    string
		partial bool
	}{
		"a full refund":    {`{"refund_id":"rf_1","payment_id":"pay_1","amount":11800,"is_partial":false}`, false},
		"a partial refund": {`{"refund_id":"rf_1","payment_id":"pay_1","amount":250,"is_partial":true}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			event, err := normalizePayment(t, "refund.succeeded", tc.data)
			if err != nil {
				t.Fatalf("NormalizePayment: %v", err)
			}
			if event.RefundID != "rf_1" || event.Payment.PaymentID != "pay_1" || event.PartialRefund != tc.partial ||
				event.RefundCents == 0 {
				t.Errorf("event = %+v", event)
			}
		})
	}

	// A type that settles nothing must never 500 and never retry forever.
	t.Run("every other type is ignored", func(t *testing.T) {
		for _, eventType := range []string{"payment.processing", "payment.cancelled", "refund.failed",
			"dispute.opened", "some.future.thing"} {
			event, err := normalizePayment(t, eventType, `{"payment_id":"pay_1"}`)
			if err != nil || !event.IsZero() {
				t.Errorf("%s = (%+v, %v), want ignored", eventType, event, err)
			}
		}
	})

	// Retried rather than consumed: a shape that changed under pug is fixed by a redeploy.
	t.Run("a payload it cannot read is an error", func(t *testing.T) {
		for name, tc := range map[string][2]string{
			"no payment id":         {"payment.succeeded", `{"status":"succeeded"}`},
			"data that is not JSON": {"payment.failed", `"garbled"`},
			// Consumed as still processing, a late success on a reopened invoice is lost.
			"a success still processing": {"payment.succeeded", `{"payment_id":"pay_1","status":"processing"}`},
			"a success with no status":   {"payment.succeeded", `{"payment_id":"pay_1","status":null}`},
			"a failure that succeeded":   {"payment.failed", `{"payment_id":"pay_1","status":"succeeded"}`},
			"a refund with no id":        {"refund.succeeded", `{"payment_id":"pay_1","is_partial":false}`},
			// Read as full, a partial refund would mark the invoice refunded.
			"a refund that does not say if it is partial": {"refund.succeeded", `{"refund_id":"rf_1","payment_id":"pay_1"}`},
		} {
			if _, err := normalizePayment(t, tc[0], tc[1]); err == nil {
				t.Errorf("%s: normalized without an error", name)
			}
		}
	})
}

// Anything else, requires_* included, is not an outcome yet: an unknown word may delay
// a settle but never invents one.
func TestPaymentStatusMapping(t *testing.T) {
	for in, want := range map[string]corebilling.PaymentStatus{
		"succeeded":                         corebilling.PaymentSucceeded,
		"failed":                            corebilling.PaymentFailed,
		"cancelled":                         corebilling.PaymentFailed,
		"processing":                        corebilling.PaymentProcessing,
		"requires_customer_action":          corebilling.PaymentProcessing,
		"requires_merchant_action":          corebilling.PaymentProcessing,
		"requires_payment_method":           corebilling.PaymentProcessing,
		"requires_confirmation":             corebilling.PaymentProcessing,
		"requires_capture":                  corebilling.PaymentProcessing,
		"partially_captured":                corebilling.PaymentProcessing,
		"partially_captured_and_capturable": corebilling.PaymentProcessing,
		"":                                  corebilling.PaymentProcessing,
		"something_invented":                corebilling.PaymentProcessing,
	} {
		if got := paymentStatus(in); got != want {
			t.Errorf("paymentStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListPayments(t *testing.T) {
	since := time.Date(2026, 9, 12, 5, 59, 0, 0, time.UTC)
	pages := map[string]string{
		"0": `{"items":[{"payment_id":"pay_1","subscription_id":"sub_1","status":"processing","total_amount":11800,` +
			`"created_at":"2026-09-12T06:00:00Z","metadata":{"invoice_id":"inv_1"}}]}`,
		"1": `{"items":[{"payment_id":"pay_2","subscription_id":"sub_1","status":"failed","total_amount":11800,` +
			`"created_at":"2026-09-12T06:01:00Z","metadata":{}}]}`,
		"2": `{"items":[]}`,
	}
	var asked []string
	c := apiClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/payments" || q.Get("subscription_id") != "sub_1" ||
			q.Get("created_at_gte") != "2026-09-12T05:59:00Z" || q.Get("page_size") != "100" {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		asked = append(asked, q.Get("page_number"))
		jsonHandler(t, http.StatusOK, pages[q.Get("page_number")], nil).ServeHTTP(w, r)
	}))

	got, err := c.ListPayments(context.Background(), "sub_1", since.In(time.FixedZone("IST", 19800)))
	if err != nil {
		t.Fatalf("ListPayments: %v", err)
	}
	// No amounts: the list carries no tax, so a total here would be compared on the wrong base.
	want := []corebilling.Payment{
		{CreatedAt: since.Add(time.Minute), InvoiceID: "inv_1", PaymentID: "pay_1", ProviderSubID: "sub_1",
			Status: corebilling.PaymentProcessing},
		{CreatedAt: since.Add(2 * time.Minute), PaymentID: "pay_2", ProviderSubID: "sub_1",
			Status: corebilling.PaymentFailed},
	}
	if !slices.Equal(got, want) {
		t.Errorf("payments = %+v\nwant       %+v", got, want)
	}
	if !slices.Equal(asked, []string{"0", "1", "2"}) {
		t.Errorf("pages asked = %q, want every page from 0", asked)
	}

	failing := apiClient(t, jsonHandler(t, http.StatusBadGateway, `{"message":"upstream"}`, nil))
	if _, err := failing.ListPayments(context.Background(), "sub_1", since); err == nil {
		t.Error("a failed listing was read as no payments")
	}
}

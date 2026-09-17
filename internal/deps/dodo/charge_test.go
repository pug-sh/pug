package dodo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dodopayments "github.com/dodopayments/dodopayments-go"
	"github.com/dodopayments/dodopayments-go/option"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

func TestCharge(t *testing.T) {
	in := corebilling.ChargeInput{
		AmountCents:   8_552,
		Currency:      "USD",
		Description:   "Pug: 2,340,000 events, 17 Aug to 16 Sep 2026",
		InvoiceID:     "inv_1",
		OrgID:         "org_abc",
		PeriodStart:   time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		ProviderSubID: "sub_1",
	}

	t.Run("sends whole cents and the invoice metadata", func(t *testing.T) {
		var got struct {
			ProductPrice       int64             `json:"product_price"`
			ProductCurrency    string            `json:"product_currency"`
			ProductDescription string            `json:"product_description"`
			Metadata           map[string]string `json:"metadata"`
		}
		var method, path string
		c := apiClient(t, jsonHandler(t, http.StatusOK, `{"payment_id":"pay_1"}`, func(r *http.Request) {
			method, path = r.Method, r.URL.Path
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode charge request: %v", err)
			}
		}))
		id, err := c.Charge(context.Background(), in)
		if err != nil {
			t.Fatalf("Charge: %v", err)
		}
		if id != "pay_1" || method != http.MethodPost || path != "/subscriptions/sub_1/charge" {
			t.Errorf("payment %q from %s %s", id, method, path)
		}
		if got.ProductPrice != 8_552 || got.ProductCurrency != "USD" || got.ProductDescription != in.Description {
			t.Errorf("request = %+v", got)
		}
		if got.Metadata["invoice_id"] != "inv_1" || got.Metadata["org_id"] != "org_abc" ||
			got.Metadata["period_start"] != "2026-08-17T00:00:00Z" {
			t.Errorf("metadata = %v", got.Metadata)
		}
	})

	for _, tc := range []struct {
		status                  int
		declined, notChargeable bool
	}{
		{http.StatusPaymentRequired, true, false},
		{http.StatusNotFound, false, true},
		{http.StatusBadRequest, false, false},
		{http.StatusUnauthorized, false, false},
		{http.StatusForbidden, false, false},
		{http.StatusConflict, false, false},
		{http.StatusUnprocessableEntity, false, false},
		{http.StatusTooManyRequests, false, false},
	} {
		t.Run(fmt.Sprintf("a %d is a refusal", tc.status), func(t *testing.T) {
			c := apiClient(t, jsonHandler(t, tc.status, `{"message":"no"}`, nil))
			_, err := c.Charge(context.Background(), in)
			var refused *corebilling.ChargeError
			if !errors.As(err, &refused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if refused.Code != fmt.Sprintf("HTTP_%d", tc.status) ||
				refused.Declined != tc.declined || refused.NotChargeable != tc.notChargeable {
				t.Errorf("refusal = %+v", refused)
			}
		})
	}

	// A decline's own code is what makes it hard or soft; every other refusal keeps its status.
	t.Run("only a decline takes its code from the body", func(t *testing.T) {
		for status, want := range map[int]string{http.StatusPaymentRequired: "STOLEN_CARD", http.StatusNotFound: "HTTP_404"} {
			c := apiClient(t, jsonHandler(t, status, `{"code":"STOLEN_CARD","message":"no"}`, nil))
			_, err := c.Charge(context.Background(), in)
			var refused *corebilling.ChargeError
			if !errors.As(err, &refused) || refused.Code != want {
				t.Errorf("%d: err = %v, want code %s", status, err, want)
			}
		}
	})

	for name, h := range map[string]http.Handler{
		"a 5xx":         jsonHandler(t, http.StatusBadGateway, `{"message":"upstream"}`, nil),
		"no payment id": jsonHandler(t, http.StatusOK, `{}`, nil),
	} {
		t.Run(name+" leaves the outcome unknown", func(t *testing.T) {
			_, err := apiClient(t, h).Charge(context.Background(), in)
			var refused *corebilling.ChargeError
			if err == nil || errors.As(err, &refused) {
				t.Fatalf("err = %v, want an unknown outcome", err)
			}
		})
	}

	for _, status := range []int{http.StatusBadGateway, http.StatusTooManyRequests} {
		t.Run(fmt.Sprintf("a %d is never sent again", status), func(t *testing.T) {
			var posts atomic.Int32
			srv := httptest.NewServer(jsonHandler(t, status, `{"message":"no"}`, func(*http.Request) {
				posts.Add(1)
			}))
			t.Cleanup(srv.Close)
			// The SDK's default retries, as New leaves them: apiClient turns them off.
			c := &Client{api: dodopayments.NewClient(option.WithBaseURL(srv.URL+"/"), option.WithBearerToken("sk_test"))}
			if _, err := c.Charge(context.Background(), in); err == nil {
				t.Fatal("Charge succeeded against a failing stub")
			}
			if n := posts.Load(); n != 1 {
				t.Errorf("POSTed %d times, want 1", n)
			}
		})
	}

	t.Run("a dropped connection leaves the outcome unknown and is never sent again", func(t *testing.T) {
		var posts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			posts.Add(1)
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		}))
		t.Cleanup(srv.Close)
		c := &Client{api: dodopayments.NewClient(option.WithBaseURL(srv.URL+"/"), option.WithBearerToken("sk_test"))}
		_, err := c.Charge(context.Background(), in)
		var refused *corebilling.ChargeError
		if err == nil || errors.As(err, &refused) {
			t.Fatalf("err = %v, want an unknown outcome", err)
		}
		if n := posts.Load(); n != 1 {
			t.Errorf("POSTed %d times, want 1", n)
		}
	})
}

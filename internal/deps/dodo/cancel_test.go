package dodo

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Both writes PATCH the one subscription and answer with it as a read does, so a pin
// and a removal store what the provider now holds.
func TestSetNextBillingDateAndCancelSubscription(t *testing.T) {
	fetched, err := apiClient(t, jsonHandler(t, http.StatusOK, subscriptionJSONBody, nil)).
		FetchSubscription(context.Background(), "sub_1")
	if err != nil {
		t.Fatalf("FetchSubscription: %v", err)
	}
	var method, path string
	var body map[string]any
	c := apiClient(t, jsonHandler(t, http.StatusOK, subscriptionJSONBody, func(r *http.Request) {
		method, path, body = r.Method, r.URL.Path, map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode update request: %v", err)
		}
	}))

	pinned, err := c.SetNextBillingDate(context.Background(), "sub_1", time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SetNextBillingDate: %v", err)
	}
	if method != http.MethodPatch || path != "/subscriptions/sub_1" || len(body) != 1 ||
		body["next_billing_date"] != "2026-07-13T00:00:00Z" {
		t.Errorf("pin sent %s %s %v, want only the date", method, path, body)
	}
	if pinned != fetched {
		t.Errorf("pinned = %+v\nwant the subscription as a read returns it: %+v", pinned, fetched)
	}

	cancelled, err := c.CancelSubscription(context.Background(), "sub_1")
	if err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}
	if method != http.MethodPatch || path != "/subscriptions/sub_1" || len(body) != 2 ||
		body["status"] != "cancelled" || body["cancel_reason"] != "cancelled_by_customer" {
		t.Errorf("cancel sent %s %s %v", method, path, body)
	}
	if cancelled != fetched {
		t.Errorf("cancelled = %+v\nwant the subscription as a read returns it: %+v", cancelled, fetched)
	}

	refusing := apiClient(t, jsonHandler(t, http.StatusUnprocessableEntity, `{"message":"no"}`, nil))
	if _, err := refusing.SetNextBillingDate(context.Background(), "sub_1", time.Now()); err == nil {
		t.Error("a refused pin was reported as done")
	}
	if _, err := refusing.CancelSubscription(context.Background(), "sub_1"); err == nil {
		t.Error("a refused cancellation was reported as done")
	}
}
